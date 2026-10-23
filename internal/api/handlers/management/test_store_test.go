package management

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"sync"

	coreauth "github.com/therealtinhtute/llmhub/sdk/cliproxy/auth"
)

type memoryAuthStore struct {
	mu        sync.Mutex
	items     map[string]*coreauth.Auth
	revisions map[string]int64
	saveCount int
}

func (s *memoryAuthStore) List(_ context.Context) ([]*coreauth.Auth, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]*coreauth.Auth, 0, len(s.items))
	for _, item := range s.items {
		out = append(out, item)
	}
	return out, nil
}

func (s *memoryAuthStore) Save(_ context.Context, auth *coreauth.Auth) (string, error) {
	if auth == nil {
		return "", nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.items == nil {
		s.items = make(map[string]*coreauth.Auth)
	}
	s.items[auth.ID] = auth
	if s.revisions == nil {
		s.revisions = make(map[string]int64)
	}
	// Unconditional writes still advance the revision, exactly like the
	// Postgres store, so they invalidate in-flight expectations.
	s.revisions[auth.ID]++
	s.saveCount++
	return auth.ID, nil
}

func (s *memoryAuthStore) AuthRevision(_ context.Context, id string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	revision, ok := s.revisions[id]
	if !ok {
		return 0, os.ErrNotExist
	}
	return revision, nil
}

func (s *memoryAuthStore) SaveIfRevision(_ context.Context, auth *coreauth.Auth, expected int64) (int64, error) {
	if auth == nil {
		return 0, errors.New("auth is nil")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	current, ok := s.revisions[auth.ID]
	if !ok {
		return 0, os.ErrNotExist
	}
	if current != expected {
		return current, coreauth.ErrRevisionConflict
	}
	if s.items == nil {
		s.items = make(map[string]*coreauth.Auth)
	}
	clone := auth.Clone()
	s.items[auth.ID] = clone
	s.revisions[auth.ID] = current + 1
	s.saveCount++
	return current + 1, nil
}

var _ coreauth.RevisionedStore = (*memoryAuthStore)(nil)

func (s *memoryAuthStore) SaveCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.saveCount
}

func (s *memoryAuthStore) Delete(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	delete(s.items, id)
	return nil
}

func (s *memoryAuthStore) SetBaseDir(string) {}

type pathlessMemoryAuthStore struct {
	memoryAuthStore
}

func (s *pathlessMemoryAuthStore) PathlessAuthStore() bool { return true }

func (s *pathlessMemoryAuthStore) LoadAuthContent(_ context.Context, id string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.items == nil {
		return nil, os.ErrNotExist
	}
	auth := s.items[id]
	if auth == nil {
		return nil, os.ErrNotExist
	}
	raw, err := json.Marshal(auth.Metadata)
	if err != nil {
		return nil, err
	}
	return raw, nil
}

type failingPathlessAuthStore struct {
	pathlessMemoryAuthStore
	err error
}

func (s *failingPathlessAuthStore) Save(context.Context, *coreauth.Auth) (string, error) {
	if s.err == nil {
		s.err = errors.New("save failed")
	}
	return "", s.err
}
