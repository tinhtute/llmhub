package logging

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// The request-log filename is built from the client-supplied request path and
// request ID. On Windows a backslash in either one survived sanitisation, so
// filepath.Join could climb out of the log directory; on every platform a
// parent-directory sequence in the request ID reached the filename untouched.
func TestGenerateFilenameRejectsPathSeparators(t *testing.T) {
	logger := &FileRequestLogger{}
	for _, raw := range []string{
		`/v1beta/models/x\..\..\..\..\probe:generateContent`,
		`/v1/realtime/calls/x\..\..\..\probe/hangup`,
		`/v1/models/../../etc/passwd`,
		`/v1/models/..\..\..\..\windows\win.ini`,
		`/v1/\..\..\..\root`,
		`/v1/models/a//b//c`,
		`/.`,
		`/..`,
		`/...`,
	} {
		for label, got := range map[string]string{
			"generateFilename":      logger.generateFilename(raw, "deadbeef"),
			"generateErrorFilename": logger.generateErrorFilename(raw, "deadbeef"),
		} {
			if strings.ContainsAny(got, `/\`) {
				t.Fatalf("%s(%q) = %q, want no path separators", label, raw, got)
			}
			if strings.Contains(got, "..") {
				t.Fatalf("%s(%q) = %q, want no parent-directory sequence", label, raw, got)
			}
			if filepath.Base(got) != got {
				t.Fatalf("%s(%q) = %q, want a single path component", label, raw, got)
			}
			if !strings.HasSuffix(got, ".log") {
				t.Fatalf("%s(%q) = %q, want a .log suffix", label, raw, got)
			}
		}
	}
}

// A request ID reaches the filename through ShortRequestID, which keeps an ID
// of eight characters or fewer verbatim, so it is a second way for a separator
// or a parent sequence to get in.
func TestGenerateFilenameRejectsTraversalInRequestID(t *testing.T) {
	logger := &FileRequestLogger{}
	for _, raw := range []string{"../../x", `..\..\x`, "..", "a/../b"} {
		got := logger.generateFilename("/v1/chat/completions", raw)
		if got != filepath.Base(got) || strings.ContainsAny(got, `/\`) || strings.Contains(got, "..") {
			t.Fatalf("generateFilename(requestID=%q) = %q, want a single safe path component", raw, got)
		}
	}
}

// Ordinary request paths must keep producing exactly the name they produced
// before the traversal guard: the segment before the timestamp is asserted
// verbatim, the timestamp and the request ID against the documented shape.
func TestGenerateFilenameKeepsOrdinaryPathsByteIdentical(t *testing.T) {
	logger := &FileRequestLogger{}
	tail := regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{6}-deadbeef\.log$`)
	for _, tc := range []struct{ url, wantSegment string }{
		{"/v1/chat/completions", "v1-chat-completions"},
		{"/v1/responses", "v1-responses"},
		{"/v1beta/models/gemini-3.1-pro:generateContent?alt=sse", "v1beta-models-gemini-3.1-pro-generateContent"},
		{"/v1/files/foo.txt", "v1-files-foo.txt"},
		{"/", "root"},
	} {
		for label, got := range map[string]string{
			"generateFilename":      logger.generateFilename(tc.url, "deadbeef"),
			"generateErrorFilename": logger.generateErrorFilename(tc.url, "deadbeef"),
		} {
			wantPrefix := tc.wantSegment + "-"
			if label == "generateErrorFilename" {
				wantPrefix = "error-" + wantPrefix
			}
			if !strings.HasPrefix(got, wantPrefix) {
				t.Fatalf("%s(%q) = %q, want prefix %q", label, tc.url, got, wantPrefix)
			}
			if rest := strings.TrimPrefix(got, wantPrefix); !tail.MatchString(rest) {
				t.Fatalf("%s(%q) = %q, want a timestamp-ID tail, got %q", label, tc.url, got, rest)
			}
		}
	}
}

func TestCreateUniqueLogFileRejectsTraversalNames(t *testing.T) {
	dir := t.TempDir()
	for _, bad := range []string{
		"",
		".",
		"..",
		"../outside.log",
		`..\outside.log`,
		"sub/inside.log",
		`sub\inside.log`,
		"v1-x-2026-01-01T000000-..2.log",
	} {
		f, path, err := createUniqueLogFile(dir, bad)
		if err == nil {
			_ = f.Close()
			t.Fatalf("createUniqueLogFile(%q) succeeded with path %q, want error", bad, path)
		}
	}
}

// End to end: whatever the client puts in the path or the request ID, every
// file the logger writes stays directly in logsDir.
func TestLogRequestWithTraversalPathStaysInsideLogsDir(t *testing.T) {
	for _, tc := range []struct {
		name      string
		enabled   bool
		force     bool
		url       string
		requestID string
	}{
		{name: "forced_error_log_when_disabled", enabled: false, force: true, url: `/v1beta/models/x\..\..\..\..\traversal-probe:generateContent`, requestID: "deadbeef"},
		{name: "normal_request_log_when_enabled", enabled: true, force: false, url: `/v1beta/models/x\..\..\..\..\traversal-probe:generateContent`, requestID: "deadbeef"},
		{name: "parent_sequence_in_request_id", enabled: true, force: false, url: "/v1/chat/completions", requestID: "../../outside"},
		{name: "separators_in_request_id", enabled: true, force: false, url: "/v1/chat/completions", requestID: `..\..\outside`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rootDir := t.TempDir()
			logsDir := filepath.Join(rootDir, "a", "b", "c", "logs")
			logger := NewFileRequestLogger(tc.enabled, logsDir, rootDir, 10)
			now := time.Now()
			if errLog := logger.LogRequestWithOptions(
				tc.url,
				"POST",
				map[string][]string{"Content-Type": {"application/json"}},
				[]byte(`{"contents":[]}`),
				400,
				map[string][]string{"Content-Type": {"application/json"}},
				[]byte(`{"error":"unknown model"}`),
				nil, nil, nil, nil, nil,
				tc.force,
				tc.requestID,
				now,
				now,
			); errLog != nil {
				t.Fatalf("LogRequestWithOptions error: %v", errLog)
			}

			written := 0
			if errWalk := filepath.Walk(rootDir, func(path string, info os.FileInfo, err error) error {
				if err != nil {
					return err
				}
				if info.IsDir() {
					return nil
				}
				if filepath.Dir(path) != logsDir {
					t.Fatalf("file written outside logsDir: %s (logsDir=%s)", path, logsDir)
				}
				written++
				return nil
			}); errWalk != nil {
				t.Fatalf("filepath.Walk error: %v", errWalk)
			}
			if written == 0 {
				t.Fatalf("no log file written at all; the guard must not silence logging")
			}
		})
	}
}
