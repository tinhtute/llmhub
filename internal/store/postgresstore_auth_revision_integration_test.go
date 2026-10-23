package store

import (
	"errors"
	"os"
	"sync"
	"testing"

	cliproxyauth "github.com/therealtinhtute/llmhub/sdk/cliproxy/auth"
)

// Concurrent compare-and-swap writers on one auth row produce exactly one
// committed write; every other writer gets ErrRevisionConflict. Unconditional
// Saves must advance the revision too, so they invalidate expectations.
func TestPostgresStoreSaveIfRevisionConcurrentOneWinner(t *testing.T) {
	ctx, store, _, _ := newPostgresRuntimeControlTestStore(t)
	record := &cliproxyauth.Auth{
		ID:       "cas-race.json",
		FileName: "cas-race.json",
		Provider: "claude",
		Metadata: map[string]any{"type": "claude"},
	}
	if _, err := store.Save(ctx, record); err != nil {
		t.Fatalf("initial Save() error = %v", err)
	}
	baseRevision, err := store.AuthRevision(ctx, record.ID)
	if err != nil {
		t.Fatalf("AuthRevision() error = %v", err)
	}

	const writers = 8
	start := make(chan struct{})
	results := make(chan error, writers)
	var wg sync.WaitGroup
	for i := range writers {
		wg.Add(1)
		go func(writer int) {
			defer wg.Done()
			candidate := &cliproxyauth.Auth{
				ID:       record.ID,
				FileName: record.FileName,
				Provider: record.Provider,
				Metadata: map[string]any{"type": "claude", "writer": writer},
			}
			<-start
			_, errSave := store.SaveIfRevision(ctx, candidate, baseRevision)
			results <- errSave
		}(i)
	}
	close(start)
	wg.Wait()
	close(results)

	committed := 0
	conflicts := 0
	for errSave := range results {
		switch {
		case errSave == nil:
			committed++
		case errors.Is(errSave, cliproxyauth.ErrRevisionConflict):
			conflicts++
		default:
			t.Fatalf("SaveIfRevision() unexpected error = %v", errSave)
		}
	}
	if committed != 1 || conflicts != writers-1 {
		t.Fatalf("committed=%d conflicts=%d, want 1 and %d", committed, conflicts, writers-1)
	}

	revision, err := store.AuthRevision(ctx, record.ID)
	if err != nil {
		t.Fatalf("AuthRevision() after race error = %v", err)
	}
	if revision != baseRevision+1 {
		t.Fatalf("revision after race = %d, want %d", revision, baseRevision+1)
	}

	if _, err := store.Save(ctx, record); err != nil {
		t.Fatalf("unconditional Save() error = %v", err)
	}
	advanced, err := store.AuthRevision(ctx, record.ID)
	if err != nil {
		t.Fatalf("AuthRevision() after unconditional save error = %v", err)
	}
	if advanced != revision+1 {
		t.Fatalf("unconditional Save did not advance the revision: %d -> %d", revision, advanced)
	}

	// A stale expectation now loses even though the record still exists.
	if _, err := store.SaveIfRevision(ctx, record, baseRevision); !errors.Is(err, cliproxyauth.ErrRevisionConflict) {
		t.Fatalf("stale SaveIfRevision() error = %v, want ErrRevisionConflict", err)
	}
	if _, err := store.SaveIfRevision(ctx, record, 0); err == nil {
		t.Fatal("SaveIfRevision() with expected 0 must fail")
	}

	// Callers branch on this: a row deleted between the revision read and the
	// write must be recognizable as "gone", not as a conflict. The wrap must
	// keep os.ErrNotExist reachable through errors.Is.
	if err := store.Delete(ctx, record.ID); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if _, err := store.SaveIfRevision(ctx, record, advanced); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("SaveIfRevision() on a deleted row error = %v, want os.ErrNotExist", err)
	}
	if _, err := store.AuthRevision(ctx, record.ID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("AuthRevision() on a deleted row error = %v, want os.ErrNotExist", err)
	}
}

// An auth id is the auth_store primary key, so the write path and the revision
// read must agree on one normalised key: "./x.json" and "x.json" are the same
// row, and a cleaned id is never reported as "row gone".
func TestPostgresStoreAuthRowKeyIsSharedByReadAndWrite(t *testing.T) {
	ctx, store, _, _ := newPostgresRuntimeControlTestStore(t)
	record := &cliproxyauth.Auth{
		ID:       "./x.json",
		FileName: "x.json",
		Provider: "claude",
		Metadata: map[string]any{"type": "claude"},
	}
	if _, err := store.Save(ctx, record); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	count, err := store.CountAuths(ctx)
	if err != nil {
		t.Fatalf("CountAuths() error = %v", err)
	}
	if count != 1 {
		t.Fatalf("auth rows = %d, want 1", count)
	}

	cleanRevision, err := store.AuthRevision(ctx, "x.json")
	if err != nil {
		t.Fatalf("AuthRevision(%q) error = %v", "x.json", err)
	}
	uncleanRevision, err := store.AuthRevision(ctx, "./x.json")
	if err != nil {
		t.Fatalf("AuthRevision(%q) error = %v (same row as %q)", "./x.json", err, "x.json")
	}
	if uncleanRevision != cleanRevision {
		t.Fatalf("AuthRevision(\"./x.json\") = %d, AuthRevision(\"x.json\") = %d", uncleanRevision, cleanRevision)
	}

	casRecord := &cliproxyauth.Auth{
		ID:       "./x.json",
		FileName: "x.json",
		Provider: "claude",
		Metadata: map[string]any{"type": "claude", "note": "cas"},
	}
	next, err := store.SaveIfRevision(ctx, casRecord, cleanRevision)
	if err != nil {
		t.Fatalf("SaveIfRevision() with an unclean id error = %v", err)
	}
	if next != cleanRevision+1 {
		t.Fatalf("revision after CAS = %d, want %d", next, cleanRevision+1)
	}
	if count, err = store.CountAuths(ctx); err != nil || count != 1 {
		t.Fatalf("CountAuths() = %d, err = %v; want 1 row", count, err)
	}
	if revision, err := store.AuthRevision(ctx, "x.json"); err != nil || revision != next {
		t.Fatalf("AuthRevision(\"x.json\") = %d, err = %v; want %d", revision, err, next)
	}
}
