package management

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/therealtinhtute/llmhub/internal/config"
	fileauth "github.com/therealtinhtute/llmhub/sdk/auth"
	coreauth "github.com/therealtinhtute/llmhub/sdk/cliproxy/auth"
)

func TestPatchAuthFileFields_MergeHeadersAndDeleteEmptyValues(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	record := &coreauth.Auth{
		ID:       "test.json",
		FileName: "test.json",
		Provider: "claude",
		Attributes: map[string]string{
			"path":            "/tmp/test.json",
			"header:X-Old":    "old",
			"header:X-Remove": "gone",
		},
		Metadata: map[string]any{
			"type": "claude",
			"headers": map[string]any{
				"X-Old":    "old",
				"X-Remove": "gone",
			},
		},
	}
	if _, errRegister := manager.Register(context.Background(), record); errRegister != nil {
		t.Fatalf("failed to register auth record: %v", errRegister)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)

	body := `{"name":"test.json","prefix":"p1","proxy_url":"http://proxy.local","headers":{"X-Old":"new","X-New":"v","X-Remove":"  ","X-Nope":""}}`
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPatch, "/v0/management/auth-files/fields", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	ctx.Request = req
	h.PatchAuthFileFields(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d with body %s", http.StatusOK, rec.Code, rec.Body.String())
	}

	updated, ok := manager.GetByID("test.json")
	if !ok || updated == nil {
		t.Fatalf("expected auth record to exist after patch")
	}

	if updated.Prefix != "p1" {
		t.Fatalf("prefix = %q, want %q", updated.Prefix, "p1")
	}
	if updated.ProxyURL != "http://proxy.local" {
		t.Fatalf("proxy_url = %q, want %q", updated.ProxyURL, "http://proxy.local")
	}

	if updated.Metadata == nil {
		t.Fatalf("expected metadata to be non-nil")
	}
	if got, _ := updated.Metadata["prefix"].(string); got != "p1" {
		t.Fatalf("metadata.prefix = %q, want %q", got, "p1")
	}
	if got, _ := updated.Metadata["proxy_url"].(string); got != "http://proxy.local" {
		t.Fatalf("metadata.proxy_url = %q, want %q", got, "http://proxy.local")
	}

	headersMeta, ok := updated.Metadata["headers"].(map[string]any)
	if !ok {
		raw, _ := json.Marshal(updated.Metadata["headers"])
		t.Fatalf("metadata.headers = %T (%s), want map[string]any", updated.Metadata["headers"], string(raw))
	}
	if got := headersMeta["X-Old"]; got != "new" {
		t.Fatalf("metadata.headers.X-Old = %#v, want %q", got, "new")
	}
	if got := headersMeta["X-New"]; got != "v" {
		t.Fatalf("metadata.headers.X-New = %#v, want %q", got, "v")
	}
	if _, ok := headersMeta["X-Remove"]; ok {
		t.Fatalf("expected metadata.headers.X-Remove to be deleted")
	}
	if _, ok := headersMeta["X-Nope"]; ok {
		t.Fatalf("expected metadata.headers.X-Nope to be absent")
	}

	if got := updated.Attributes["header:X-Old"]; got != "new" {
		t.Fatalf("attrs header:X-Old = %q, want %q", got, "new")
	}
	if got := updated.Attributes["header:X-New"]; got != "v" {
		t.Fatalf("attrs header:X-New = %q, want %q", got, "v")
	}
	if _, ok := updated.Attributes["header:X-Remove"]; ok {
		t.Fatalf("expected attrs header:X-Remove to be deleted")
	}
	if _, ok := updated.Attributes["header:X-Nope"]; ok {
		t.Fatalf("expected attrs header:X-Nope to be absent")
	}
}

func TestPatchAuthFileFields_HeadersEmptyMapIsNoop(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	record := &coreauth.Auth{
		ID:       "noop.json",
		FileName: "noop.json",
		Provider: "claude",
		Attributes: map[string]string{
			"path":         "/tmp/noop.json",
			"header:X-Kee": "1",
		},
		Metadata: map[string]any{
			"type": "claude",
			"headers": map[string]any{
				"X-Kee": "1",
			},
		},
	}
	if _, errRegister := manager.Register(context.Background(), record); errRegister != nil {
		t.Fatalf("failed to register auth record: %v", errRegister)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)

	body := `{"name":"noop.json","note":"hello","headers":{}}`
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPatch, "/v0/management/auth-files/fields", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	ctx.Request = req
	h.PatchAuthFileFields(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d with body %s", http.StatusOK, rec.Code, rec.Body.String())
	}

	updated, ok := manager.GetByID("noop.json")
	if !ok || updated == nil {
		t.Fatalf("expected auth record to exist after patch")
	}
	if got := updated.Attributes["header:X-Kee"]; got != "1" {
		t.Fatalf("attrs header:X-Kee = %q, want %q", got, "1")
	}
	headersMeta, ok := updated.Metadata["headers"].(map[string]any)
	if !ok {
		t.Fatalf("expected metadata.headers to remain a map, got %T", updated.Metadata["headers"])
	}
	if got := headersMeta["X-Kee"]; got != "1" {
		t.Fatalf("metadata.headers.X-Kee = %#v, want %q", got, "1")
	}
}

func TestPatchAuthFileFields_WebsocketsFalseIsUpdate(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	record := &coreauth.Auth{
		ID:       "codex.json",
		FileName: "codex.json",
		Provider: "codex",
		Attributes: map[string]string{
			"path":       "/tmp/codex.json",
			"websockets": "true",
		},
		Metadata: map[string]any{
			"type":       "codex",
			"websockets": true,
		},
	}
	if _, errRegister := manager.Register(context.Background(), record); errRegister != nil {
		t.Fatalf("failed to register auth record: %v", errRegister)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)

	body := `{"name":"codex.json","websockets":false}`
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPatch, "/v0/management/auth-files/fields", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	ctx.Request = req
	h.PatchAuthFileFields(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d with body %s", http.StatusOK, rec.Code, rec.Body.String())
	}

	updated, ok := manager.GetByID("codex.json")
	if !ok || updated == nil {
		t.Fatalf("expected auth record to exist after patch")
	}
	if got := updated.Attributes["websockets"]; got != "false" {
		t.Fatalf("attrs websockets = %q, want %q", got, "false")
	}
	if got, ok := updated.Metadata["websockets"].(bool); !ok || got {
		t.Fatalf("metadata.websockets = %#v, want false", updated.Metadata["websockets"])
	}
}

func TestPatchAuthFileFields_ArbitraryFieldsPersistToFile(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	authDir := t.TempDir()
	fileName := "generic.json"
	filePath := filepath.Join(authDir, fileName)
	store := fileauth.NewFileTokenStore()
	store.SetBaseDir(authDir)
	manager := coreauth.NewManager(store, nil, nil)
	record := &coreauth.Auth{
		ID:       fileName,
		FileName: fileName,
		Provider: "codex",
		Attributes: map[string]string{
			"path": filePath,
		},
		Metadata: map[string]any{
			"type": "codex",
		},
	}
	if _, errRegister := manager.Register(context.Background(), record); errRegister != nil {
		t.Fatalf("failed to register auth record: %v", errRegister)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, manager)

	body := `{"name":"generic.json","abc":true,"nested.cde":true,"fgh":{"ijk":true}}`
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPatch, "/v0/management/auth-files/fields", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	ctx.Request = req
	h.PatchAuthFileFields(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d with body %s", http.StatusOK, rec.Code, rec.Body.String())
	}

	raw, errRead := os.ReadFile(filePath)
	if errRead != nil {
		t.Fatalf("failed to read updated auth file: %v", errRead)
	}
	var data map[string]any
	if errUnmarshal := json.Unmarshal(raw, &data); errUnmarshal != nil {
		t.Fatalf("failed to unmarshal updated auth file: %v", errUnmarshal)
	}
	if got := data["abc"]; got != true {
		t.Fatalf("abc = %#v, want true", got)
	}
	nested, ok := data["nested"].(map[string]any)
	if !ok {
		t.Fatalf("nested = %#v, want object", data["nested"])
	}
	if got := nested["cde"]; got != true {
		t.Fatalf("nested.cde = %#v, want true", got)
	}
	fgh, ok := data["fgh"].(map[string]any)
	if !ok {
		t.Fatalf("fgh = %#v, want object", data["fgh"])
	}
	if got := fgh["ijk"]; got != true {
		t.Fatalf("fgh.ijk = %#v, want true", got)
	}
}

func TestPatchAuthFileFields_ValidatesCredentialWeight(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	store := &pathlessMemoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	if _, errRegister := manager.Register(context.Background(), &coreauth.Auth{
		ID:       "weighted.json",
		FileName: "weighted.json",
		Provider: "codex",
		Attributes: map[string]string{
			"source": "postgres",
		},
		Metadata: map[string]any{"type": "codex"},
	}); errRegister != nil {
		t.Fatalf("failed to register auth record: %v", errRegister)
	}
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)
	h.tokenStore = store

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPatch, "/v0/management/auth-files/fields", strings.NewReader(`{"id":"weighted.json","weight":5}`))
	req.Header.Set("Content-Type", "application/json")
	ctx.Request = req
	h.PatchAuthFileFields(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d with body %s", http.StatusOK, rec.Code, rec.Body.String())
	}
	updated, _ := manager.GetByID("weighted.json")
	if got := updated.Attributes["weight"]; got != "5" {
		t.Fatalf("weight attr = %q, want 5", got)
	}
	entry := firstAuthFileEntry(t, h)
	if got := entry["weight"]; got != float64(5) && got != int64(5) && got != int(5) {
		t.Fatalf("listed weight = %#v, want 5", got)
	}

	badRec := httptest.NewRecorder()
	badCtx, _ := gin.CreateTestContext(badRec)
	badReq := httptest.NewRequest(http.MethodPatch, "/v0/management/auth-files/fields", strings.NewReader(`{"id":"weighted.json","weight":0}`))
	badReq.Header.Set("Content-Type", "application/json")
	badCtx.Request = badReq
	h.PatchAuthFileFields(badCtx)

	if badRec.Code != http.StatusBadRequest {
		t.Fatalf("expected invalid weight status %d, got %d with body %s", http.StatusBadRequest, badRec.Code, badRec.Body.String())
	}
	updated, _ = manager.GetByID("weighted.json")
	if got := updated.Attributes["weight"]; got != "5" {
		t.Fatalf("invalid patch changed weight attr to %q", got)
	}
}

func TestPatchAuthFileFields_SaveErrorReturnsFailure(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	record := &coreauth.Auth{
		ID:       "postgres-edit.json",
		FileName: "postgres-edit.json",
		Provider: "codex",
		Metadata: map[string]any{
			"type": "codex",
		},
	}
	if _, errRegister := manager.Register(context.Background(), record); errRegister != nil {
		t.Fatalf("failed to register auth record: %v", errRegister)
	}
	manager.SetStore(&failingPathlessAuthStore{err: errors.New("database unavailable")})

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)

	body := `{"name":"postgres-edit.json","note":"should fail"}`
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPatch, "/v0/management/auth-files/fields", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	ctx.Request = req

	h.PatchAuthFileFields(ctx)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected status %d, got %d with body %s", http.StatusInternalServerError, rec.Code, rec.Body.String())
	}
}

func TestSyncAuthFilePriorityAttributeTracksFileSource(t *testing.T) {
	auth := &coreauth.Auth{
		Attributes: map[string]string{"path": "/auth/test.json"},
		Metadata:   map[string]any{"priority": float64(1)},
	}
	syncAuthFilePriorityAttribute(auth)
	if auth.Attributes[coreauth.AttributeFilePriority] != "true" {
		t.Fatal("added file priority not marked as inherited")
	}
	delete(auth.Metadata, "priority")
	syncAuthFilePriorityAttribute(auth)
	if _, inherited := auth.Attributes[coreauth.AttributeFilePriority]; inherited {
		t.Fatal("removed file priority still marked as inherited")
	}
}

// raceAuthFileWrites releases two auth-files PATCH calls through one barrier so
// they contend for the same store revision.
func raceAuthFileWrites(t *testing.T, first, second func() *httptest.ResponseRecorder) []*httptest.ResponseRecorder {
	t.Helper()
	start := make(chan struct{})
	done := make(chan *httptest.ResponseRecorder, 2)
	for _, run := range []func() *httptest.ResponseRecorder{first, second} {
		go func(run func() *httptest.ResponseRecorder) {
			<-start
			done <- run()
		}(run)
	}
	close(start)
	return []*httptest.ResponseRecorder{<-done, <-done}
}

// assertOneWinnerAndConflict asserts that exactly one racing write committed and
// the loser was answered with a conflict: 409 when it lost the store revision
// check, 412 when its If-Match was already stale. Both report the fresh revision.
func assertOneWinnerAndConflict(t *testing.T, recs []*httptest.ResponseRecorder, freshRevision int64, label string) *httptest.ResponseRecorder {
	t.Helper()
	if got := countStatus(recs, http.StatusOK); got != 1 {
		t.Fatalf("%s: expected exactly 1 successful write, got %d (statuses %d/%d, bodies %q / %q)",
			label, got, recs[0].Code, recs[1].Code, recs[0].Body.String(), recs[1].Body.String())
	}
	var loser *httptest.ResponseRecorder
	for _, rec := range recs {
		if rec.Code != http.StatusOK {
			loser = rec
		}
	}
	switch loser.Code {
	case http.StatusConflict:
		var body struct {
			Error    string `json:"error"`
			Revision int64  `json:"revision"`
		}
		if errDecode := json.Unmarshal(loser.Body.Bytes(), &body); errDecode != nil {
			t.Fatalf("%s: decode conflict body: %v", label, errDecode)
		}
		if body.Error != "conflict" {
			t.Fatalf("%s: conflict error = %q, want conflict", label, body.Error)
		}
		if body.Revision != freshRevision {
			t.Fatalf("%s: conflict reported revision %d, want %d", label, body.Revision, freshRevision)
		}
	case http.StatusPreconditionFailed:
		if got := loser.Header().Get("ETag"); got != formatConfigETag(freshRevision) {
			t.Fatalf("%s: stale precondition ETag = %q, want %q", label, got, formatConfigETag(freshRevision))
		}
	default:
		t.Fatalf("%s: loser status = %d, want 409 or 412 (body %s)", label, loser.Code, loser.Body.String())
	}
	return loser
}

func countStatus(recs []*httptest.ResponseRecorder, status int) int {
	count := 0
	for _, rec := range recs {
		if rec.Code == status {
			count++
		}
	}
	return count
}

func storedAuthRecord(t *testing.T, store *memoryAuthStore, id string) *coreauth.Auth {
	t.Helper()
	store.mu.Lock()
	defer store.mu.Unlock()
	record := store.items[id]
	if record == nil {
		t.Fatalf("auth record %s is not stored", id)
	}
	return record.Clone()
}

func patchAuthFileFieldsRequest(h *Handler, ifMatch string, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPatch, "/v0/management/auth-files/fields", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if ifMatch != "" {
		req.Header.Set("If-Match", ifMatch)
	}
	ctx.Request = req
	h.PatchAuthFileFields(ctx)
	return rec
}

func patchAuthFileStatusRequest(h *Handler, ifMatch string, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPatch, "/v0/management/auth-files/status", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if ifMatch != "" {
		req.Header.Set("If-Match", ifMatch)
	}
	ctx.Request = req
	h.PatchAuthFileStatus(ctx)
	return rec
}

// Two writers that read the same revision may not both commit: one wins, the
// other sees a 409 and nothing of its edit survives.
func TestPatchAuthFileFields_ConcurrentSameRevisionOneWinnerOneConflict(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	if _, err := manager.Register(context.Background(), &coreauth.Auth{
		ID:       "raced.json",
		FileName: "raced.json",
		Provider: "claude",
		Status:   coreauth.StatusActive,
		Metadata: map[string]any{"type": "claude"},
	}); err != nil {
		t.Fatalf("failed to register auth record: %v", err)
	}
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)
	h.tokenStore = store

	baseRevision, err := store.AuthRevision(context.Background(), "raced.json")
	if err != nil {
		t.Fatalf("failed to read base revision: %v", err)
	}
	ifMatch := formatConfigETag(baseRevision)
	savesBefore := store.SaveCount()

	recs := raceAuthFileWrites(t,
		func() *httptest.ResponseRecorder {
			return patchAuthFileFieldsRequest(h, ifMatch, `{"name":"raced.json","note":"first"}`)
		},
		func() *httptest.ResponseRecorder {
			return patchAuthFileFieldsRequest(h, ifMatch, `{"name":"raced.json","prefix":"second"}`)
		},
	)

	assertOneWinnerAndConflict(t, recs, baseRevision+1, "fields patch race")

	stored := storedAuthRecord(t, store, "raced.json")
	_, hasNote := stored.Metadata["note"]
	_, hasPrefix := stored.Metadata["prefix"]
	if hasNote == hasPrefix {
		t.Fatalf("expected exactly one winning edit, got note=%v prefix=%v", hasNote, hasPrefix)
	}
	storedRevision, err := store.AuthRevision(context.Background(), "raced.json")
	if err != nil {
		t.Fatalf("failed to read stored revision: %v", err)
	}
	if storedRevision != baseRevision+1 {
		t.Fatalf("stored revision = %d, want %d", storedRevision, baseRevision+1)
	}
	if got := store.SaveCount() - savesBefore; got != 1 {
		t.Fatalf("store received %d writes, want exactly 1", got)
	}
}

// The loser can re-read and retry: a conflict is a rejection, not a lockout.
func TestPatchAuthFileFields_ConflictIsRecoverable(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	if _, err := manager.Register(context.Background(), &coreauth.Auth{
		ID:       "retry.json",
		FileName: "retry.json",
		Provider: "claude",
		Status:   coreauth.StatusActive,
		Metadata: map[string]any{"type": "claude"},
	}); err != nil {
		t.Fatalf("failed to register auth record: %v", err)
	}
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)
	h.tokenStore = store

	stale, err := store.AuthRevision(context.Background(), "retry.json")
	if err != nil {
		t.Fatalf("failed to read revision: %v", err)
	}
	if rec := patchAuthFileFieldsRequest(h, formatConfigETag(stale), `{"name":"retry.json","note":"first"}`); rec.Code != http.StatusOK {
		t.Fatalf("first write status = %d, body %s", rec.Code, rec.Body.String())
	}
	if rec := patchAuthFileFieldsRequest(h, formatConfigETag(stale), `{"name":"retry.json","prefix":"second"}`); rec.Code != http.StatusPreconditionFailed {
		t.Fatalf("stale If-Match status = %d, body %s", rec.Code, rec.Body.String())
	}

	fresh, err := store.AuthRevision(context.Background(), "retry.json")
	if err != nil {
		t.Fatalf("failed to read fresh revision: %v", err)
	}
	if rec := patchAuthFileFieldsRequest(h, formatConfigETag(fresh), `{"name":"retry.json","prefix":"second"}`); rec.Code != http.StatusOK {
		t.Fatalf("retry after conflict status = %d, body %s", rec.Code, rec.Body.String())
	}
	stored := storedAuthRecord(t, store, "retry.json")
	if stored.Metadata["note"] != "first" || stored.Metadata["prefix"] != "second" {
		t.Fatalf("retry lost an earlier edit: %#v", stored.Metadata)
	}
}

// A status toggle and a field patch racing on one record cannot both commit.
func TestPatchAuthFileStatus_RacingFieldPatchHasOneWinner(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	if _, err := manager.Register(context.Background(), &coreauth.Auth{
		ID:       "status-raced.json",
		FileName: "status-raced.json",
		Provider: "claude",
		Status:   coreauth.StatusActive,
		Metadata: map[string]any{"type": "claude"},
	}); err != nil {
		t.Fatalf("failed to register auth record: %v", err)
	}
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)
	h.tokenStore = store

	baseRevision, err := store.AuthRevision(context.Background(), "status-raced.json")
	if err != nil {
		t.Fatalf("failed to read base revision: %v", err)
	}
	ifMatch := formatConfigETag(baseRevision)

	recs := raceAuthFileWrites(t,
		func() *httptest.ResponseRecorder {
			return patchAuthFileStatusRequest(h, ifMatch, `{"name":"status-raced.json","disabled":true}`)
		},
		func() *httptest.ResponseRecorder {
			return patchAuthFileFieldsRequest(h, ifMatch, `{"name":"status-raced.json","note":"fifth"}`)
		},
	)

	assertOneWinnerAndConflict(t, recs, baseRevision+1, "status vs fields race")

	stored := storedAuthRecord(t, store, "status-raced.json")
	_, patchedField := stored.Metadata["note"]
	if stored.Disabled == patchedField {
		t.Fatalf("expected exactly one winning edit, got disabled=%v note=%v", stored.Disabled, patchedField)
	}
	if stored.Disabled && stored.Status != coreauth.StatusDisabled {
		t.Fatalf("disabled record kept status %q", stored.Status)
	}
}

// racingDeleteAuthStore models another instance deleting the row around this
// instance's write: either before the revision read or between the read and the
// compare-and-swap.
type racingDeleteAuthStore struct {
	*memoryAuthStore
	goneOnRead bool
	goneOnSave bool
}

func (s *racingDeleteAuthStore) AuthRevision(ctx context.Context, id string) (int64, error) {
	if s.goneOnRead {
		return 0, os.ErrNotExist
	}
	return s.memoryAuthStore.AuthRevision(ctx, id)
}

func (s *racingDeleteAuthStore) SaveIfRevision(ctx context.Context, auth *coreauth.Auth, expected int64) (int64, error) {
	if s.goneOnSave {
		return 0, os.ErrNotExist
	}
	return s.memoryAuthStore.SaveIfRevision(ctx, auth, expected)
}

// newRacingDeleteHandler registers one auth record and returns a handler whose
// token store reports the row as deleted by another instance.
func newRacingDeleteHandler(t *testing.T, goneOnRead, goneOnSave bool) (*Handler, *racingDeleteAuthStore, *coreauth.Manager) {
	t.Helper()
	store := &racingDeleteAuthStore{
		memoryAuthStore: &memoryAuthStore{},
		goneOnRead:      goneOnRead,
		goneOnSave:      goneOnSave,
	}
	manager := coreauth.NewManager(store, nil, nil)
	if _, err := manager.Register(context.Background(), &coreauth.Auth{
		ID:       "vanished.json",
		FileName: "vanished.json",
		Provider: "claude",
		Status:   coreauth.StatusActive,
		Metadata: map[string]any{"type": "claude"},
	}); err != nil {
		t.Fatalf("failed to register auth record: %v", err)
	}
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)
	h.tokenStore = store
	return h, store, manager
}

// assertGoneRecordConverged checks the contract for a row another instance
// deleted: the caller is told not-found, the store is not written (so the record
// is not resurrected), and the runtime stops advertising it. Repeating the
// request must stay in that state.
func assertGoneRecordConverged(t *testing.T, h *Handler, store *racingDeleteAuthStore, manager *coreauth.Manager, rec *httptest.ResponseRecorder) {
	t.Helper()
	if rec.Code != http.StatusNotFound {
		t.Fatalf("deleted row: status = %d, want %d (body %s)", rec.Code, http.StatusNotFound, rec.Body.String())
	}
	if got := store.SaveCount(); got != 1 {
		t.Fatalf("store received %d writes, want only the initial registration", got)
	}
	if _, ok := manager.GetByID("vanished.json"); ok {
		t.Fatal("runtime still advertises a record the store no longer has")
	}
	repeat := patchAuthFileFieldsRequest(h, "", `{"name":"vanished.json","note":"again"}`)
	if repeat.Code != http.StatusNotFound {
		t.Fatalf("repeated request: status = %d, want %d (body %s)", repeat.Code, http.StatusNotFound, repeat.Body.String())
	}
	if got := store.SaveCount(); got != 1 {
		t.Fatalf("repeated request wrote to the store %d times, want 0", got-1)
	}
}

// The row is deleted between this request's revision read and its write.
func TestPatchAuthFileFields_RowDeletedDuringWriteAnswersNotFound(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	h, store, manager := newRacingDeleteHandler(t, false, true)
	rec := patchAuthFileFieldsRequest(h, "", `{"name":"vanished.json","note":"racing"}`)
	assertGoneRecordConverged(t, h, store, manager, rec)
}

// The row is already gone when the request reads its revision, while this
// instance still lists the credential.
func TestPatchAuthFileFields_RowAlreadyGoneAnswersNotFound(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	h, store, manager := newRacingDeleteHandler(t, true, false)
	rec := patchAuthFileFieldsRequest(h, "", `{"name":"vanished.json","note":"stale"}`)
	assertGoneRecordConverged(t, h, store, manager, rec)
}

// Auths the manager never persists — runtime-only credentials and auths without
// metadata — have no store row, so they have no revision to compare. They must
// stay editable through the runtime and must not be dropped from it.
func TestPatchAuthFileStatus_RuntimeOnlyAuthStaysUpdatable(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	cases := []struct {
		name     string
		id       string
		provider string
		metadata map[string]any
		attribs  map[string]string
	}{
		{
			name:     "runtime_only attribute",
			id:       "gemini-virtual-child.json",
			provider: "gemini",
			metadata: map[string]any{"type": "gemini"},
			attribs:  map[string]string{"runtime_only": "true"},
		},
		{
			name:     "empty metadata",
			id:       "config-api-key.json",
			provider: "openai",
			metadata: nil,
			attribs:  map[string]string{"source": "config"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := &memoryAuthStore{}
			manager := coreauth.NewManager(store, nil, nil)
			if _, err := manager.Register(context.Background(), &coreauth.Auth{
				ID:         tc.id,
				FileName:   tc.id,
				Provider:   tc.provider,
				Status:     coreauth.StatusActive,
				Metadata:   tc.metadata,
				Attributes: tc.attribs,
			}); err != nil {
				t.Fatalf("failed to register auth record: %v", err)
			}
			h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)
			h.tokenStore = store

			disable := patchAuthFileStatusRequest(h, "", `{"name":"`+tc.id+`","disabled":true}`)
			if disable.Code != http.StatusOK {
				t.Fatalf("disable: status = %d, want %d (body %s)", disable.Code, http.StatusOK, disable.Body.String())
			}
			if got := store.SaveCount(); got != 0 {
				t.Fatalf("a non-persisted auth reached the store %d times", got)
			}
			record, ok := manager.GetByID(tc.id)
			if !ok || record == nil {
				t.Fatal("runtime dropped an auth the store never had")
			}
			if !record.Disabled || record.Status != coreauth.StatusDisabled {
				t.Fatalf("runtime record not disabled: disabled=%v status=%q", record.Disabled, record.Status)
			}

			enable := patchAuthFileStatusRequest(h, "", `{"name":"`+tc.id+`","disabled":false}`)
			if enable.Code != http.StatusOK {
				t.Fatalf("enable: status = %d, want %d (body %s)", enable.Code, http.StatusOK, enable.Body.String())
			}
			record, ok = manager.GetByID(tc.id)
			if !ok || record == nil {
				t.Fatal("runtime dropped an auth the store never had")
			}
			if record.Disabled {
				t.Fatal("runtime record still disabled after enable")
			}

			if tc.metadata == nil {
				// Metadata-less auths are config-sourced, which the fields endpoint
				// has always refused; only the status path applies to them.
				return
			}
			fields := patchAuthFileFieldsRequest(h, "", `{"name":"`+tc.id+`","note":"edited"}`)
			if fields.Code != http.StatusOK {
				t.Fatalf("fields patch: status = %d, want %d (body %s)", fields.Code, http.StatusOK, fields.Body.String())
			}
			if got := store.SaveCount(); got != 0 {
				t.Fatalf("a non-persisted auth reached the store %d times", got)
			}
			record, ok = manager.GetByID(tc.id)
			if !ok || record == nil {
				t.Fatal("runtime dropped an auth the store never had")
			}
			if record.Metadata["note"] != "edited" {
				t.Fatalf("runtime record did not keep the edit: %#v", record.Metadata)
			}
		})
	}
}
