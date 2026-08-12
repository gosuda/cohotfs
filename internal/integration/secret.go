package integration

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"sync"
	"time"
)

type SecretLease struct {
	ID          string
	WorkspaceID string
	Destination string
	ExpiresAt   time.Time
	value       []byte
	used        bool
}

type SecretStore struct {
	mu     sync.Mutex
	leases map[string]*SecretLease
	now    func() time.Time
}

func NewSecretStore() *SecretStore {
	return &SecretStore{leases: make(map[string]*SecretLease), now: time.Now}
}

func (s *SecretStore) Issue(workspaceID, agent, destination string, value []byte, lifetime time.Duration) (string, time.Time, error) {
	if workspaceID == "" || !AgentDestinationAllowed(agent, destination) || len(value) == 0 || lifetime <= 0 || lifetime > 60*time.Second {
		return "", time.Time{}, fmt.Errorf("invalid agent secret lease")
	}
	var random [24]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", time.Time{}, err
	}
	id := base64.RawURLEncoding.EncodeToString(random[:])
	expires := s.now().Add(lifetime).UTC()
	copied := append([]byte(nil), value...)
	s.mu.Lock()
	s.leases[id] = &SecretLease{ID: id, WorkspaceID: workspaceID, Destination: destination, ExpiresAt: expires, value: copied}
	s.mu.Unlock()
	return id, expires, nil
}

func (s *SecretStore) Fetch(id, workspaceID string) (destination string, value []byte, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	lease := s.leases[id]
	if lease == nil || lease.WorkspaceID != workspaceID || lease.used || !s.now().Before(lease.ExpiresAt) {
		if lease != nil && !s.now().Before(lease.ExpiresAt) {
			zero(lease.value)
			delete(s.leases, id)
		}
		return "", nil, errors.New("secret lease is missing, used, expired, or belongs to another workspace")
	}
	lease.used = true
	value = append([]byte(nil), lease.value...)
	zero(lease.value)
	delete(s.leases, id)
	return lease.Destination, value, nil
}

func (s *SecretStore) Revoke(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if lease := s.leases[id]; lease != nil {
		zero(lease.value)
		delete(s.leases, id)
	}
}

func AgentDestinationAllowed(agent, destination string) bool {
	switch agent {
	case "omp", "codex":
		return destination == "OPENAI_API_KEY"
	case "claude":
		return destination == "ANTHROPIC_API_KEY"
	default:
		return false
	}
}

func zero(value []byte) {
	for index := range value {
		value[index] = 0
	}
}
