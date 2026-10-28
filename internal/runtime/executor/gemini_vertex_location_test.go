package executor

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"testing"

	cliproxyauth "github.com/therealtinhtute/llmhub/sdk/cliproxy/auth"
)

// vertexTestServiceAccount builds a service-account payload that
// vertexCreds/NormalizeServiceAccountMap accept, so a test exercises the real
// credential path rather than a stub.
func vertexTestServiceAccount(t *testing.T) map[string]any {
	t.Helper()
	key, errGen := rsa.GenerateKey(rand.Reader, 2048)
	if errGen != nil {
		t.Fatalf("generate test key: %v", errGen)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	return map[string]any{
		"type":           "service_account",
		"project_id":     "p9-project",
		"client_email":   "p9@p9-project.iam.gserviceaccount.com",
		"private_key":    string(pemBytes),
		"private_key_id": "p9-key",
	}
}

// A credential that never set a location must resolve to the global endpoint,
// both in the credential reader and in the base URL derived from it. An
// explicit region keeps its own regional host.
func TestVertexLocationDefaultsToGlobal(t *testing.T) {
	sa := vertexTestServiceAccount(t)
	auth := &cliproxyauth.Auth{Metadata: map[string]any{"project_id": "p9-project", "service_account": sa}}
	_, location, _, errCreds := vertexCreds(auth)
	if errCreds != nil {
		t.Fatalf("vertexCreds() error = %v", errCreds)
	}
	if location != "global" {
		t.Fatalf("vertexCreds() location = %q, want global", location)
	}

	regional := &cliproxyauth.Auth{Metadata: map[string]any{
		"project_id":      "p9-project",
		"service_account": sa,
		"location":        "us-central1",
	}}
	if _, got, _, errRegional := vertexCreds(regional); errRegional != nil {
		t.Fatalf("vertexCreds(regional) error = %v", errRegional)
	} else if got != "us-central1" {
		t.Fatalf("vertexCreds(regional) location = %q, want us-central1", got)
	}

	for _, loc := range []string{"", "   ", "global", "GLOBAL"} {
		if got := vertexBaseURL(loc); got != "https://aiplatform.googleapis.com" {
			t.Errorf("vertexBaseURL(%q) = %q, want https://aiplatform.googleapis.com", loc, got)
		}
	}
	if got := vertexBaseURL("us-central1"); got != "https://us-central1-aiplatform.googleapis.com" {
		t.Errorf("vertexBaseURL(%q) = %q, want the regional host", "us-central1", got)
	}
}
