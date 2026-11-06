package xai

import (
	"context"
	"strings"
	"testing"

	coreauth "github.com/therealtinhtute/llmhub/sdk/cliproxy/auth"
)

func TestCredentialFileNameHashesIdentity(t *testing.T) {
	upper := CredentialFileName("User@Example.com", "")
	lower := CredentialFileName("user@example.com", "")
	if upper != lower {
		t.Fatalf("email case changed the filename: %q vs %q", upper, lower)
	}

	// Both sanitize to the same readable segment; only the identity hash keeps
	// them apart.
	plus := CredentialFileName("a+b@example.com", "")
	dash := CredentialFileName("a-b@example.com", "")
	if plus == dash {
		t.Fatalf("distinct identities collided on %q", plus)
	}

	if !strings.HasPrefix(plus, "xai-a-b@example.com-") || !strings.HasSuffix(plus, ".json") {
		t.Fatalf("unexpected filename shape: %q", plus)
	}

	subjectOnly := CredentialFileName("", "subject-1")
	if !strings.HasPrefix(subjectOnly, "xai-subject-1-") {
		t.Fatalf("subject-only filename = %q", subjectOnly)
	}
	if subjectOnly == CredentialFileName("", "subject-2") {
		t.Fatal("distinct subjects must not collide")
	}
}

type listOnlyStore struct {
	records []*coreauth.Auth
}

func (s *listOnlyStore) List(context.Context) ([]*coreauth.Auth, error) { return s.records, nil }
func (s *listOnlyStore) Save(context.Context, *coreauth.Auth) (string, error) {
	return "", nil
}
func (s *listOnlyStore) Delete(context.Context, string) error { return nil }

func TestFindMatchingLegacyCredential(t *testing.T) {
	const email = "user@example.com"
	target := &coreauth.Auth{
		Provider: "xai",
		FileName: CredentialFileName(email, ""),
		Metadata: map[string]any{"email": email},
	}
	legacy := &coreauth.Auth{
		Provider: "xai",
		FileName: "xai-user@example.com.json",
		Metadata: map[string]any{"email": email},
	}
	foreign := &coreauth.Auth{
		Provider: "xai",
		FileName: "xai-other@example.com.json",
		Metadata: map[string]any{"email": "other@example.com"},
	}

	store := &listOnlyStore{records: []*coreauth.Auth{target, legacy, foreign}}
	found, err := FindMatchingLegacyCredential(context.Background(), store, target)
	if err != nil {
		t.Fatalf("FindMatchingLegacyCredential error: %v", err)
	}
	if found != legacy {
		t.Fatalf("found = %+v, want the pre-hash credential", found)
	}

	// A candidate whose stored identity conflicts must never be claimed.
	store.records = []*coreauth.Auth{target, foreign}
	found, err = FindMatchingLegacyCredential(context.Background(), store, target)
	if err != nil {
		t.Fatalf("FindMatchingLegacyCredential error: %v", err)
	}
	if found != nil {
		t.Fatalf("foreign credential was claimed: %+v", found)
	}
}
