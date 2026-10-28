package cmd

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"

	"github.com/therealtinhtute/llmhub/internal/config"
)

// A service account imported without an explicit location is written to the
// credential file with the global location, not a hard-coded region.
func TestVertexImportDefaultsLocationToGlobal(t *testing.T) {
	key, errGen := rsa.GenerateKey(rand.Reader, 2048)
	if errGen != nil {
		t.Fatalf("generate test key: %v", errGen)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	serviceAccount := map[string]any{
		"type":           "service_account",
		"project_id":     "p9-project",
		"client_email":   "p9@p9-project.iam.gserviceaccount.com",
		"private_key":    string(keyPEM),
		"private_key_id": "p9-key",
	}
	raw, errMarshal := json.Marshal(serviceAccount)
	if errMarshal != nil {
		t.Fatalf("marshal service account: %v", errMarshal)
	}
	authDir := t.TempDir()
	keyPath := filepath.Join(authDir, "sa.json")
	if errWrite := os.WriteFile(keyPath, raw, 0o600); errWrite != nil {
		t.Fatalf("write service account: %v", errWrite)
	}

	DoVertexImport(&config.Config{AuthDir: authDir}, keyPath, "")

	savedPath := filepath.Join(authDir, "vertex-p9-project.json")
	saved, errRead := os.ReadFile(savedPath)
	if errRead != nil {
		t.Fatalf("read imported credential %s: %v", savedPath, errRead)
	}
	var stored struct {
		Location       string         `json:"location"`
		Type           string         `json:"type"`
		ProjectID      string         `json:"project_id"`
		ServiceAccount map[string]any `json:"service_account"`
	}
	if errUnmarshal := json.Unmarshal(saved, &stored); errUnmarshal != nil {
		t.Fatalf("unmarshal imported credential: %v", errUnmarshal)
	}
	// The imported file is the credential payload the loader hands to the
	// executor as auth metadata, so its top-level location is the region the
	// credential runs with.
	if stored.Location != "global" {
		t.Fatalf("imported location = %q, want global", stored.Location)
	}
	if stored.Type != "vertex" || stored.ProjectID != "p9-project" || stored.ServiceAccount == nil {
		t.Fatalf("imported credential = %+v, want a vertex service-account payload", stored)
	}
}
