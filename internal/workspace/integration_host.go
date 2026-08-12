package workspace

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/gosuda/cohotfs/internal/api"
	"github.com/gosuda/cohotfs/internal/state"
)

type IntegrationHost interface {
	Acquire(context.Context, api.LeaseRequest) (api.LeaseResponse, error)
	Release(context.Context, api.ReleaseRequest) error
	Status(context.Context) (api.HostStatus, error)
}

type IntegrationHostFactory func(context.Context) (IntegrationHost, error)

func (s *DockerService) SetIntegrationHost(host IntegrationHost) {
	s.integrationHost = host
}

func (s *DockerService) SetIntegrationHostFactory(factory IntegrationHostFactory) {
	s.integrationHostFactory = factory
}

func (s *DockerService) loadPlan(id string) (Plan, error) {
	raw, err := s.store.LoadWorkspaceArtifact(id, "plan.json")
	if err != nil {
		return Plan{}, err
	}
	var plan Plan
	if err := json.Unmarshal(raw, &plan); err != nil || plan.SchemaVersion != 1 || plan.WorkspaceID != id {
		return Plan{}, fmt.Errorf("workspace plan identity is invalid")
	}
	return plan, nil
}

func needsIntegrationHost(plan Plan) bool {
	return plan.Integrations["browser"] || plan.Integrations["gitCredentials"] || anyAgentEnabled(plan.IntegrationSettings.Agents)
}

func (s *DockerService) ensureIntegrationHost(ctx context.Context) (IntegrationHost, error) {
	if s.integrationHost != nil {
		return s.integrationHost, nil
	}
	if s.integrationHostFactory == nil {
		return nil, fmt.Errorf("enabled host integrations require the Cohotfs host service")
	}
	host, err := s.integrationHostFactory(ctx)
	if err != nil {
		return nil, err
	}
	if host == nil {
		return nil, fmt.Errorf("integration host factory returned no client")
	}
	s.integrationHost = host
	return host, nil
}

func (s *DockerService) acquireIntegrationLeases(ctx context.Context, record *state.Workspace, plan Plan) error {
	if !needsIntegrationHost(plan) {
		return nil
	}
	host, err := s.ensureIntegrationHost(ctx)
	if err != nil {
		return err
	}
	if err := s.releaseIntegrationLeases(ctx, record, host); err != nil {
		return fmt.Errorf("release prior integration leases: %w", err)
	}
	requests, err := integrationLeaseRequests(plan)
	if err != nil {
		return err
	}
	acquired := make([]state.ExternalResource, 0, len(requests))
	for _, request := range requests {
		response, acquireErr := host.Acquire(ctx, request)
		if acquireErr != nil {
			cleanupErr := s.rollbackIntegrationLeases(ctx, record, host, acquired)
			return errors.Join(acquireErr, cleanupErr)
		}
		if response.LeaseID == "" {
			cleanupErr := s.rollbackIntegrationLeases(ctx, record, host, acquired)
			return errors.Join(fmt.Errorf("host returned an empty %s lease ID", request.Kind), cleanupErr)
		}
		identity := map[string]string{"kind": string(request.Kind), "endpoint": response.Endpoint}
		for key, value := range response.Metadata {
			identity[key] = value
		}
		resource := state.ExternalResource{
			Type: "host-lease", ID: response.LeaseID, AcquiredAt: s.now().UTC(), Identity: identity,
		}
		record.Resources = append(record.Resources, resource)
		acquired = append(acquired, resource)
		if err := s.store.SaveWorkspace(*record); err != nil {
			cleanupErr := s.rollbackIntegrationLeases(ctx, record, host, acquired)
			return errors.Join(err, cleanupErr)
		}
	}
	return nil
}

func integrationLeaseRequests(plan Plan) ([]api.LeaseRequest, error) {
	requests := make([]api.LeaseRequest, 0, 3)
	base := func(kind api.LeaseKind) api.LeaseRequest {
		return api.LeaseRequest{WorkspaceID: plan.WorkspaceID, IdempotencyKey: plan.CreationNonce + ":" + string(kind), Kind: kind}
	}
	if plan.Integrations["browser"] {
		request := base(api.LeaseChrome)
		request.Parameters = map[string]string{
			"platform":      plan.IntegrationSettings.Browser.Platform,
			"executable":    plan.IntegrationSettings.Browser.Executable,
			"retainProfile": strconv.FormatBool(plan.IntegrationSettings.Browser.RetainProfile),
		}
		requests = append(requests, request)
	}
	if plan.Integrations["gitCredentials"] {
		contexts, err := json.Marshal(plan.IntegrationSettings.GitCredentials.AllowedContexts)
		if err != nil {
			return nil, err
		}
		request := base(api.LeaseGitCredential)
		request.Parameters = map[string]string{"allowedContexts": string(contexts)}
		requests = append(requests, request)
	}
	if anyAgentEnabled(plan.IntegrationSettings.Agents) {
		requests = append(requests, base(api.LeaseAgentSecret))
	}
	return requests, nil
}

func (s *DockerService) rollbackIntegrationLeases(ctx context.Context, record *state.Workspace, host IntegrationHost, acquired []state.ExternalResource) error {
	var failures []error
	for index := len(acquired) - 1; index >= 0; index-- {
		resource := acquired[index]
		if err := host.Release(ctx, api.ReleaseRequest{WorkspaceID: record.ID, LeaseID: resource.ID}); err != nil {
			failures = append(failures, err)
			continue
		}
		markResourceReleased(record, resource.ID, s.now())
	}
	if err := s.store.SaveWorkspace(*record); err != nil {
		failures = append(failures, err)
	}
	return errors.Join(failures...)
}

func (s *DockerService) releaseIntegrationLeases(ctx context.Context, record *state.Workspace, host IntegrationHost) error {
	status, statusErr := host.Status(ctx)
	active := map[string]bool{}
	if statusErr == nil {
		for _, lease := range status.Leases {
			if lease.WorkspaceID == record.ID {
				active[lease.LeaseID] = true
			}
		}
	}
	var failures []error
	changed := false
	for index := len(record.Resources) - 1; index >= 0; index-- {
		resource := &record.Resources[index]
		if resource.Type != "host-lease" || resource.ReleasedAt != nil {
			continue
		}
		if statusErr != nil || active[resource.ID] {
			if err := host.Release(ctx, api.ReleaseRequest{WorkspaceID: record.ID, LeaseID: resource.ID}); err != nil {
				failures = append(failures, err)
				continue
			}
		}
		released := s.now().UTC()
		resource.ReleasedAt = &released
		changed = true
	}
	if changed {
		if err := s.store.SaveWorkspace(*record); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

func markResourceReleased(record *state.Workspace, id string, now time.Time) {
	for index := len(record.Resources) - 1; index >= 0; index-- {
		if record.Resources[index].Type == "host-lease" && record.Resources[index].ID == id && record.Resources[index].ReleasedAt == nil {
			released := now.UTC()
			record.Resources[index].ReleasedAt = &released
			return
		}
	}
}

func hasActiveIntegrationLease(record state.Workspace) bool {
	for _, resource := range record.Resources {
		if resource.Type == "host-lease" && resource.ReleasedAt == nil {
			return true
		}
	}
	return false
}

func (s *DockerService) releaseActiveIntegrationLeases(ctx context.Context, record *state.Workspace) error {
	if !hasActiveIntegrationLease(*record) {
		return nil
	}
	host, err := s.ensureIntegrationHost(ctx)
	if err != nil {
		return err
	}
	return s.releaseIntegrationLeases(ctx, record, host)
}
