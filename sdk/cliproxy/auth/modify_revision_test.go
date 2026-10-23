package auth

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
)

// staleReadRevisionStore reports a pinned revision on read while the write path
// compares against the real one. It models another instance advancing the row
// between our read and our write, which is the case the in-process mutation
// gate cannot serialize.
type staleReadRevisionStore struct {
	mu        sync.Mutex
	items     map[string]*Auth
	revisions map[string]int64
	pinned    map[string]int64
}

func newStaleReadRevisionStore() *staleReadRevisionStore {
	return &staleReadRevisionStore{
		items:     make(map[string]*Auth),
		revisions: make(map[string]int64),
		pinned:    make(map[string]int64),
	}
}

func (s *staleReadRevisionStore) pin(id string, revision int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pinned[id] = revision
}

func (s *staleReadRevisionStore) unpin(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.pinned, id)
}

func (s *staleReadRevisionStore) List(context.Context) ([]*Auth, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Auth, 0, len(s.items))
	for _, item := range s.items {
		out = append(out, item.Clone())
	}
	return out, nil
}

func (s *staleReadRevisionStore) Save(_ context.Context, auth *Auth) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.items[auth.ID] = auth.Clone()
	s.revisions[auth.ID]++
	return auth.ID, nil
}

func (s *staleReadRevisionStore) Delete(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.items, id)
	delete(s.revisions, id)
	return nil
}

func (s *staleReadRevisionStore) AuthRevision(_ context.Context, id string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.revisions[id]; !ok {
		return 0, os.ErrNotExist
	}
	if pinned, ok := s.pinned[id]; ok {
		return pinned, nil
	}
	return s.revisions[id], nil
}

func (s *staleReadRevisionStore) SaveIfRevision(_ context.Context, auth *Auth, expected int64) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	current, ok := s.revisions[auth.ID]
	if !ok {
		return 0, os.ErrNotExist
	}
	if current != expected {
		return current, ErrRevisionConflict
	}
	s.items[auth.ID] = auth.Clone()
	s.revisions[auth.ID] = current + 1
	return current + 1, nil
}

var _ RevisionedStore = (*staleReadRevisionStore)(nil)

// Two writers that both pass the local read-modify-write gate still cannot both
// commit when the store moved under them: exactly one wins and the other gets
// ErrRevisionConflict. The loser's edit must not reach the runtime snapshot.
func TestManager_Modify_ConcurrentSameRevisionOneWinnerOneConflict(t *testing.T) {
	ctx := context.Background()
	store := newStaleReadRevisionStore()
	manager := NewManager(store, &RoundRobinSelector{}, nil)
	record := &Auth{
		ID:       "modify-raced",
		Provider: "claude",
		Status:   StatusActive,
		Metadata: map[string]any{"type": "claude"},
	}
	if _, err := manager.Register(ctx, record); err != nil {
		t.Fatalf("register: %v", err)
	}

	baseRevision, err := store.AuthRevision(ctx, record.ID)
	if err != nil {
		t.Fatalf("read base revision: %v", err)
	}
	// Every writer that reads now sees the pre-race revision, while the store
	// still rejects writes whose expectation no longer holds.
	store.pin(record.ID, baseRevision)

	type result struct {
		committed bool
		err       error
	}
	start := make(chan struct{})
	results := make(chan result, 2)
	for _, field := range []struct{ key, value string }{
		{"note", "first"},
		{"prefix", "second"},
	} {
		go func(key, value string) {
			<-start
			_, committed, errModify := manager.Modify(ctx, record.ID, baseRevision, func(current *Auth) error {
				if current.Metadata == nil {
					current.Metadata = make(map[string]any)
				}
				current.Metadata[key] = value
				return nil
			})
			results <- result{committed: committed, err: errModify}
		}(field.key, field.value)
	}
	close(start)

	winners := 0
	conflicts := 0
	for range 2 {
		res := <-results
		switch {
		case res.committed && res.err == nil:
			winners++
		case !res.committed && errors.Is(res.err, ErrRevisionConflict):
			conflicts++
		default:
			t.Fatalf("unexpected modify outcome: committed=%v err=%v", res.committed, res.err)
		}
	}
	if winners != 1 || conflicts != 1 {
		t.Fatalf("winners=%d conflicts=%d, want exactly one of each", winners, conflicts)
	}

	store.unpin(record.ID)
	storedRevision, err := store.AuthRevision(ctx, record.ID)
	if err != nil {
		t.Fatalf("read stored revision: %v", err)
	}
	if storedRevision != baseRevision+1 {
		t.Fatalf("stored revision = %d, want %d", storedRevision, baseRevision+1)
	}

	memoryAuth, ok := manager.GetByID(record.ID)
	if !ok || memoryAuth == nil {
		t.Fatalf("auth %s missing from runtime snapshot", record.ID)
	}
	_, hasNote := memoryAuth.Metadata["note"]
	_, hasPrefix := memoryAuth.Metadata["prefix"]
	if hasNote == hasPrefix {
		t.Fatalf("runtime snapshot kept the rejected edit: note=%v prefix=%v", hasNote, hasPrefix)
	}
}
