//go:build linux

package cli

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gosuda/cohotfs/internal/apperr"
	"github.com/gosuda/cohotfs/internal/audit"
	"github.com/gosuda/cohotfs/internal/config"
	"github.com/gosuda/cohotfs/internal/hostroot"
	"github.com/gosuda/cohotfs/internal/hostservice"
	"github.com/gosuda/cohotfs/internal/runtime"
	"github.com/gosuda/cohotfs/internal/runtime/docker"
	"github.com/gosuda/cohotfs/internal/state"
	"github.com/gosuda/cohotfs/internal/toolchain"
	workspaceservice "github.com/gosuda/cohotfs/internal/workspace"
	"github.com/spf13/cobra"
)

func buildWorkspaceLifecycleCommands(deps Dependencies) []*cobra.Command {
	create := &cobra.Command{
		Use:  "create",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cwd, err := os.Getwd()
			if err != nil {
				return err
			}
			return withWorkspaceRuntime(cmd.Context(), deps, func(root *hostroot.Root, _ *state.Store, backend *docker.Adapter, service *workspaceservice.DockerService, host config.HostConfig) error {
				prepared, err := prepareWorkspaceDirectory(root, cwd, host, false)
				if err != nil {
					return err
				}
				record, err := createPreparedWorkspace(cmd.Context(), root, backend, service, host, prepared)
				if err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "created %s (%s)\n", record.Name, record.ID)
				return nil
			})
		},
	}
	start := workspaceStateCommand(deps, "start <workspace>", "workspace.start", func(ctx context.Context, service *workspaceservice.DockerService, id, key string) (state.Workspace, error) {
		return service.Start(ctx, id, key)
	})
	stop := workspaceStateCommand(deps, "stop <workspace>", "workspace.stop", func(ctx context.Context, service *workspaceservice.DockerService, id, key string) (state.Workspace, error) {
		return service.Stop(ctx, id, key, 10*time.Second)
	})
	restart := workspaceStateCommand(deps, "restart <workspace>", "workspace.restart", func(ctx context.Context, service *workspaceservice.DockerService, id, key string) (state.Workspace, error) {
		return service.Restart(ctx, id, key, 10*time.Second)
	})
	rotateHostKey := workspaceStateCommand(deps, "rotate-host-key <workspace>", "workspace.rotate-host-key", func(ctx context.Context, service *workspaceservice.DockerService, id, key string) (state.Workspace, error) {
		return service.RotateSSHHostKey(ctx, id, key)
	})
	remove := &cobra.Command{
		Use:  "remove <workspace>",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			confirmed, _ := cmd.Flags().GetBool("yes")
			if !confirmed {
				return apperr.New(apperr.ExitStateConflict, "confirmation_required", "workspace removal requires --yes")
			}
			return withWorkspaceRuntime(cmd.Context(), deps, func(root *hostroot.Root, store *state.Store, _ *docker.Adapter, service *workspaceservice.DockerService, _ config.HostConfig) error {
				record, err := resolveWorkspace(store, args[0])
				if err != nil {
					return err
				}
				key, acknowledge, err := stableOperationKey(root, record.ID, "workspace.remove", nil)
				if err != nil {
					return err
				}
				removeErr := service.Remove(cmd.Context(), record.ID, key)
				acknowledgeErr := acknowledge()
				if removeErr != nil || acknowledgeErr != nil {
					return errors.Join(removeErr, acknowledgeErr)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "removed %s\n", record.ID)
				return nil
			})
		},
	}
	remove.Flags().Bool("yes", false, "confirm configured operation")
	recoverCommand := &cobra.Command{
		Use:  "recover <workspace>",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			confirmed, _ := cmd.Flags().GetBool("yes")
			return withWorkspaceRecoveryRuntime(cmd.Context(), deps, func(root *hostroot.Root, store *state.Store, backend *docker.Adapter, service *workspaceservice.DockerService, _ config.HostConfig) error {
				record, err := resolveWorkspace(store, args[0])
				if err != nil {
					return err
				}
				reconciler := workspaceservice.NewService(store, map[string]runtime.Lifecycle{"docker": backend}, audit.New(root))
				report, reconcileErr := reconciler.Reconcile(cmd.Context(), record.ID)
				writeRecoveryReport(cmd, report)
				if reconcileErr != nil {
					return reconcileErr
				}
				if !confirmed {
					return apperr.New(apperr.ExitStateConflict, "confirmation_required", "recovery preview complete; rerun with --yes to remove identity-matched resources")
				}
				key, acknowledge, err := stableOperationKey(root, record.ID, "workspace.recover", nil)
				if err != nil {
					return err
				}
				recoverErr := service.Recover(cmd.Context(), record.ID, key)
				acknowledgeErr := acknowledge()
				if recoverErr != nil || acknowledgeErr != nil {
					return errors.Join(recoverErr, acknowledgeErr)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "recovered %s\n", record.ID)
				return nil
			})
		},
	}
	recoverCommand.Flags().Bool("yes", false, "remove identity-matched resources")
	return []*cobra.Command{create, start, stop, restart, remove, recoverCommand, rotateHostKey}
}

func writeRecoveryReport(cmd *cobra.Command, report workspaceservice.ReconcileReport) {
	for _, item := range report.Matched {
		fmt.Fprintf(cmd.OutOrStdout(), "matched\t%s\n", item)
	}
	for _, item := range report.Missing {
		fmt.Fprintf(cmd.OutOrStdout(), "missing\t%s\n", item)
	}
	for _, item := range report.Quarantined {
		fmt.Fprintf(cmd.OutOrStdout(), "quarantined\t%s\n", item)
	}
}

type preparedWorkspace struct {
	workspace       config.Workspace
	source          string
	maskCohotfsRoot bool
	digest          string
}

func prepareWorkspaceDirectory(root *hostroot.Root, cwd string, host config.HostConfig, mountCurrentDirectory bool) (preparedWorkspace, error) {
	canonical, _, projectKey, err := config.ProjectIdentity(cwd)
	if err != nil {
		return preparedWorkspace{}, apperr.Wrap(apperr.ExitUsage, "project", err, "resolve current directory: %v", err)
	}
	manifestPath := filepath.Join(canonical, ".cohotfs", "workspace.yaml")
	_, readErr := os.Lstat(manifestPath)

	var workspace config.Workspace
	switch {
	case readErr == nil:
		overridePath, err := root.HostPath(filepath.Join("projects", projectKey, "override.yaml"))
		if err != nil {
			return preparedWorkspace{}, err
		}
		workspace, err = config.ResolveWorkspace(manifestPath, overridePath, host, config.WorkspaceFlags{}, "ghcr.io/gosuda/cohotfs/workspace-base:"+Version)
		if err != nil {
			return preparedWorkspace{}, apperr.Wrap(apperr.ExitUsage, "manifest", err, "resolve workspace: %v", err)
		}
	case errors.Is(readErr, os.ErrNotExist):
		workspace, err = config.ResolveDefaultWorkspace(workspaceNameForPath(canonical), host, config.WorkspaceFlags{}, "ghcr.io/gosuda/cohotfs/workspace-base:"+Version)
		if err != nil {
			return preparedWorkspace{}, apperr.Wrap(apperr.ExitUsage, "workspace", err, "resolve default workspace: %v", err)
		}
		workspace.Spec.Setup.Mode = "manual"
	default:
		return preparedWorkspace{}, apperr.Wrap(apperr.ExitUsage, "manifest", readErr, "read workspace manifest: %v", readErr)
	}

	sourcePath := filepath.Join(canonical, workspace.Spec.Workspace.Source)
	if mountCurrentDirectory {
		workspace.Spec.Workspace.Source = "."
		workspace.Spec.Workspace.Target = "/workspace"
		sourcePath = canonical
	}
	source, err := workspaceservice.CanonicalSource(sourcePath)
	if err != nil {
		return preparedWorkspace{}, err
	}
	effectiveRaw, err := config.Render(workspace)
	if err != nil {
		return preparedWorkspace{}, err
	}
	return preparedWorkspace{workspace: workspace, source: source, digest: workspaceservice.ManifestDigest(effectiveRaw)}, nil
}

func workspaceNameForPath(canonical string) string {
	name := strings.ToLower(filepath.Base(canonical))
	name = strings.Trim(invalidName.ReplaceAllString(name, "-"), "-._")
	if name == "" {
		return "workspace"
	}
	return name
}

func createPreparedWorkspace(ctx context.Context, root *hostroot.Root, backend *docker.Adapter, service *workspaceservice.DockerService, host config.HostConfig, prepared preparedWorkspace) (state.Workspace, error) {
	info, err := backend.Probe(ctx)
	if err != nil {
		return state.Workspace{}, apperr.Wrap(apperr.ExitUnavailable, "docker", err, "Docker is unavailable: %v", err)
	}
	image, err := backend.Pull(ctx, runtimePullRequest(prepared.workspace.Spec.Image.Ref))
	if err != nil {
		return state.Workspace{}, apperr.Wrap(apperr.ExitRuntime, "image_pull", err, "pull image: %v", err)
	}
	image, err = backend.CheckCompatibility(ctx, image)
	if err != nil {
		return state.Workspace{}, apperr.Wrap(apperr.ExitRuntime, "image_incompatible", err, "%v", err)
	}

	var selectedToolchains []toolchain.Candidate
	overlayAvailable := false
	toolchains := prepared.workspace.Spec.Integrations.HostToolchains
	if toolchains.Enabled {
		candidates, err := toolchain.Discover(ctx, os.Environ())
		if err != nil {
			return state.Workspace{}, err
		}
		selectedToolchains, err = toolchain.ResolveSelections(candidates, toolchains, host.Toolchains)
		if err != nil {
			return state.Workspace{}, apperr.Wrap(apperr.ExitPolicy, "toolchain_selection", err, "%v", err)
		}
		if toolchains.Go.Enabled && toolchains.Go.Caches == "cow" || toolchains.Rust.Enabled && toolchains.Rust.Caches == "cow" {
			overlayAvailable, err = toolchain.ProbeOverlay(root)
			if err != nil {
				return state.Workspace{}, apperr.Wrap(apperr.ExitRuntime, "overlay_probe", err, "probe host cache OverlayFS: %v", err)
			}
		}
	}

	if err := ensureSSHClientKey(root); err != nil {
		return state.Workspace{}, apperr.Wrap(apperr.ExitPolicy, "ssh_key", err, "prepare SSH client key: %v", err)
	}
	publicKeyPath, err := root.HostPath("ssh/id_ed25519.pub")
	if err != nil {
		return state.Workspace{}, err
	}
	operationBody := []byte(prepared.source + "\x00" + prepared.digest + "\x00" + image.Digest)
	operationKey, acknowledge, err := stableOperationKey(root, prepared.source, "workspace.create", operationBody)
	if err != nil {
		return state.Workspace{}, err
	}
	workspaceID, err := state.WorkspaceIDForOperationKey(operationKey)
	if err != nil {
		return state.Workspace{}, err
	}
	sshDirectory := filepath.Join("run", "workspaces", workspaceID)
	if err := root.EnsureDir(sshDirectory, 0o700); err != nil {
		return state.Workspace{}, err
	}
	sshSocketPath, err := root.SocketPath(filepath.Join(sshDirectory, "ssh.sock"))
	if err != nil {
		return state.Workspace{}, apperr.Wrap(apperr.ExitUnavailable, "ssh_transport", err, "prepare SSH transport: %v", err)
	}
	record, err := service.Create(ctx, workspaceservice.CreateRequest{
		Workspace: prepared.workspace, CanonicalSource: prepared.source, ManifestDigest: prepared.digest,
		OwnerUID: os.Getuid(), OwnerGID: os.Getgid(), Image: image, BackendInfo: info,
		GVisorRuntime: host.Runtime.Docker.GVisorRuntime, SSHSocketPath: sshSocketPath, BootstrapSource: publicKeyPath,
		OperationKey:        operationKey,
		ToolchainCandidates: selectedToolchains, PermittedRoots: host.PermittedRoots, OverlayAvailable: overlayAvailable,
		MaskCohotfsRoot: prepared.maskCohotfsRoot,
		Environment:     os.Environ(),
	})
	acknowledgeErr := acknowledge()
	if err != nil {
		return record, apperr.Wrap(apperr.ExitRuntime, "workspace_create", errors.Join(err, acknowledgeErr), "create workspace: %v", errors.Join(err, acknowledgeErr))
	}
	return record, acknowledgeErr
}

func runBareWorkspace(deps Dependencies, cmd *cobra.Command, cwd string, homeMount bool) error {
	return withWorkspaceRuntime(cmd.Context(), deps, func(root *hostroot.Root, store *state.Store, backend *docker.Adapter, service *workspaceservice.DockerService, host config.HostConfig) error {
		prepared, err := prepareWorkspaceDirectory(root, cwd, host, true)
		if err != nil {
			return err
		}
		prepared.maskCohotfsRoot = homeMount
		if homeMount {
			prepared.digest = workspaceservice.ManifestDigest([]byte(prepared.digest + "\nmask-home-cohotfs-root:v1"))
		}
		record, found, err := workspaceForDirectory(store, prepared)
		if err != nil {
			return err
		}
		if !found {
			record, err = createPreparedWorkspace(cmd.Context(), root, backend, service, host, prepared)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "created %s (%s)\n", record.Name, record.ID)
		}
		switch record.Status {
		case state.StatusReady:
		case state.StatusStopped, state.StatusSetupFailed, state.StatusError:
			key, acknowledge, keyErr := stableOperationKey(root, record.ID, "workspace.start", nil)
			if keyErr != nil {
				return keyErr
			}
			record, err = service.Start(cmd.Context(), record.ID, key)
			acknowledgeErr := acknowledge()
			if err != nil || acknowledgeErr != nil {
				return apperr.Wrap(apperr.ExitRuntime, "workspace_start", errors.Join(err, acknowledgeErr), "start workspace: %v", errors.Join(err, acknowledgeErr))
			}
		default:
			return apperr.New(apperr.ExitStateConflict, "state_conflict", "workspace %s is %s; wait for or recover the in-progress operation", record.ID, record.Status)
		}
		return runOpenSSH(cmd.Context(), cmd, root, record, true, bareWorkspaceRemoteCommand())
	})
}

func bareWorkspaceRemoteCommand() []string {
	return []string{"/bin/sh", "-lc", "cd /workspace && exec /bin/sh -l"}
}

func workspaceForDirectory(store *state.Store, prepared preparedWorkspace) (state.Workspace, bool, error) {
	records, err := store.ListWorkspaces()
	if err != nil {
		return state.Workspace{}, false, err
	}
	var sourceMatches, matches []state.Workspace
	for _, record := range records {
		if record.CanonicalSource != prepared.source {
			continue
		}
		sourceMatches = append(sourceMatches, record)
		if record.ManifestDigest == prepared.digest {
			matches = append(matches, record)
		}
	}
	if len(matches) > 1 {
		return state.Workspace{}, false, apperr.New(apperr.ExitStateConflict, "state_conflict", "multiple workspaces match %s; remove duplicates or use cohotfs shell <workspace>", prepared.source)
	}
	if len(matches) == 1 {
		return matches[0], true, nil
	}
	if len(sourceMatches) != 0 {
		return state.Workspace{}, false, apperr.New(apperr.ExitStateConflict, "state_conflict", "workspace configuration changed for %s; remove the existing workspace before using the bare command", prepared.source)
	}
	return state.Workspace{}, false, nil
}

func workspaceStateCommand(deps Dependencies, use, operationName string, operation func(context.Context, *workspaceservice.DockerService, string, string) (state.Workspace, error)) *cobra.Command {
	return &cobra.Command{Use: use, Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		return withWorkspaceRuntime(cmd.Context(), deps, func(root *hostroot.Root, store *state.Store, _ *docker.Adapter, service *workspaceservice.DockerService, _ config.HostConfig) error {
			record, err := resolveWorkspace(store, args[0])
			if err != nil {
				return err
			}
			key, acknowledge, err := stableOperationKey(root, record.ID, operationName, nil)
			if err != nil {
				return err
			}
			record, operationErr := operation(cmd.Context(), service, record.ID, key)
			acknowledgeErr := acknowledge()
			if operationErr != nil || acknowledgeErr != nil {
				return errors.Join(operationErr, acknowledgeErr)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\n", record.ID, record.Status)
			return nil
		})
	}}
}

func withWorkspaceRuntime(ctx context.Context, deps Dependencies, fn func(*hostroot.Root, *state.Store, *docker.Adapter, *workspaceservice.DockerService, config.HostConfig) error) error {
	return withWorkspaceRuntimeReconcile(ctx, deps, false, fn)
}

func withWorkspaceRecoveryRuntime(ctx context.Context, deps Dependencies, fn func(*hostroot.Root, *state.Store, *docker.Adapter, *workspaceservice.DockerService, config.HostConfig) error) error {
	return withWorkspaceRuntimeReconcile(ctx, deps, true, fn)
}

func withWorkspaceRuntimeReconcile(ctx context.Context, deps Dependencies, allowPartialCleanup bool, fn func(*hostroot.Root, *state.Store, *docker.Adapter, *workspaceservice.DockerService, config.HostConfig) error) error {
	return withRoot(deps, func(root *hostroot.Root) error {
		store, err := state.NewStore(root)
		if err != nil {
			return err
		}
		path, _ := root.HostPath("config.yaml")
		host, err := config.LoadHost(path)
		if err != nil {
			return err
		}
		endpoint, err := resolveDockerEndpoint(host.Runtime.Docker.Endpoint)
		if err != nil {
			return apperr.Wrap(apperr.ExitUnavailable, "docker_endpoint", err, "resolve Docker endpoint: %v", err)
		}
		backend, err := docker.New(endpoint, host.Runtime.Docker.GVisorRuntime)
		if err != nil {
			return err
		}
		service := workspaceservice.NewDockerService(root, store, backend)
		service.SetIntegrationHostFactory(func(ctx context.Context) (workspaceservice.IntegrationHost, error) {
			return hostservice.EnsureRunning(ctx, root)
		})
		logger := audit.New(root)
		reconciler := workspaceservice.NewService(store, map[string]runtime.Lifecycle{"docker": backend}, logger)
		if _, err := reconciler.ReconcileAll(ctx); err != nil && (!allowPartialCleanup || apperr.Code(err) != apperr.ExitPartialCleanup) {
			return err
		}
		_ = logger.Append(audit.Event{Operation: "workspace.cli", Result: "begin"})
		return fn(root, store, backend, service, host)
	})
}

func runtimePullRequest(reference string) runtime.PullRequest {
	return runtime.PullRequest{Reference: reference, Platform: "linux/amd64"}
}
func idempotencyKey() string {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return ""
	}
	return hex.EncodeToString(raw[:])
}

func stableOperationKey(root *hostroot.Root, subject, operation string, body []byte) (string, func() error, error) {
	if subject == "" || operation == "" {
		return "", nil, fmt.Errorf("operation subject and name are required")
	}
	store, err := state.NewStore(root)
	if err != nil {
		return "", nil, err
	}
	lock, err := store.LockOperation(subject, operation)
	if err != nil {
		return "", nil, err
	}
	fail := func(err error) (string, func() error, error) {
		return "", nil, errors.Join(err, lock.Close())
	}
	if err := root.EnsureDir("run/operations", 0o700); err != nil {
		return fail(err)
	}
	pathDigest := sha256.Sum256([]byte(subject + "\x00" + operation))
	path := filepath.Join("run", "operations", hex.EncodeToString(pathDigest[:16])+".key")
	bodyDigest := sha256.Sum256(body)
	digest := hex.EncodeToString(bodyDigest[:])
	raw, err := root.ReadFile(path)
	var key string
	switch {
	case err == nil:
		fields := strings.Fields(string(raw))
		if len(fields) != 2 || fields[1] != digest {
			return fail(apperr.New(apperr.ExitStateConflict, "idempotency_conflict", "pending operation input changed"))
		}
		key = fields[0]
	case errors.Is(err, os.ErrNotExist):
		key = idempotencyKey()
		if key == "" {
			return fail(fmt.Errorf("generate idempotency key"))
		}
		if err := root.AtomicWrite(path, []byte(key+" "+digest+"\n"), 0o600); err != nil {
			return fail(err)
		}
	default:
		return fail(err)
	}
	acknowledge := func() error {
		removeErr := root.Remove(path)
		if errors.Is(removeErr, os.ErrNotExist) {
			removeErr = nil
		}
		return errors.Join(removeErr, lock.Close())
	}
	return key, acknowledge, nil
}
