package openai

import (
	"net/http"
	"testing"
)

// TestChunkTerminalError pins the embedded-error-chunk classifier
// (upstream be69162478da): an error object without a choices array is a
// terminal 400, anything else is a normal chunk.
func TestChunkTerminalError(t *testing.T) {
	tests := []struct {
		name       string
		chunk      string
		wantMsg    string
		wantStatus int
	}{
		{
			name:       "embedded error object",
			chunk:      `{"error":{"message":"blocked by cyber_policy","type":"invalid_request_error","code":"cyber_policy"}}`,
			wantMsg:    "blocked by cyber_policy",
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "embedded error object behind data prefix",
			chunk:      `data: {"error":{"message":"blocked by cyber_policy"}}`,
			wantMsg:    "blocked by cyber_policy",
			wantStatus: http.StatusBadRequest,
		},
		{
			name:  "choices present is a normal chunk",
			chunk: `{"choices":[{"delta":{"content":"hi"}}],"error":{"message":"ignored"}}`,
		},
		{
			name:  "error is not an object",
			chunk: `{"error":"plain string"}`,
		},
		{
			name:  "plain text chunk",
			chunk: `[DONE]`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := chunkTerminalError([]byte(tc.chunk))
			if tc.wantStatus == 0 {
				if got != nil {
					t.Fatalf("chunkTerminalError(%q) = %+v, want nil", tc.chunk, got)
				}
				return
			}
			if got == nil {
				t.Fatalf("chunkTerminalError(%q) = nil, want status %d", tc.chunk, tc.wantStatus)
			}
			if got.StatusCode != tc.wantStatus {
				t.Fatalf("chunkTerminalError(%q).StatusCode = %d, want %d", tc.chunk, got.StatusCode, tc.wantStatus)
			}
			if got.Error == nil || got.Error.Error() != tc.wantMsg {
				t.Fatalf("chunkTerminalError(%q).Error = %v, want %q", tc.chunk, got.Error, tc.wantMsg)
			}
		})
	}
}
