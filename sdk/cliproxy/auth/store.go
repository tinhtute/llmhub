package auth

import (
	"context"
	"errors"
)

// ErrRevisionConflict reports that an optimistic auth write lost a race: the
// revision stored for the record no longer matches the revision the writer
// based its edit on. Handlers translate it into a visible conflict response;
// it is never swallowed into a successful write.
var ErrRevisionConflict = errors.New("auth revision conflict")

// Store abstracts persistence of Auth state across restarts.
type Store interface {
	// List returns all auth records stored in the backend.
	List(ctx context.Context) ([]*Auth, error)
	// Save persists the provided auth record, replacing any existing one with same ID.
	Save(ctx context.Context, auth *Auth) (string, error)
	// Delete removes the auth record identified by id.
	Delete(ctx context.Context, id string) error
}

// RevisionedStore is an optional Store extension that exposes a per-record
// revision for optimistic concurrency control. A plain Save must still advance
// the revision so unconditional writers invalidate in-flight expectations.
type RevisionedStore interface {
	Store
	// AuthRevision returns the stored revision for id, or os.ErrNotExist.
	AuthRevision(ctx context.Context, id string) (int64, error)
	// SaveIfRevision persists auth only when the stored revision still equals
	// expected, returning the new revision. A stale expected revision returns
	// ErrRevisionConflict; a missing record returns os.ErrNotExist.
	SaveIfRevision(ctx context.Context, auth *Auth, expected int64) (int64, error)
}
