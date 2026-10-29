package handlers

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"runtime"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/klauspost/compress/zstd"
)

// readZstdRequestBody drives ReadRequestBody the way the OpenAI, image and video handlers do: a gin request whose body is
// zstd-compressed, with the same maxRequestBodyBytes gin key the request-log middleware reads.
func readZstdRequestBody(t *testing.T, body []byte, limit int64) ([]byte, error) {
	t.Helper()
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	c.Request.Header.Set("Content-Encoding", "zstd")
	if limit != 0 {
		c.Set("maxRequestBodyBytes", limit)
	}
	return ReadRequestBody(c)
}

// streamingZstdRequestBodyBomb is a small zstd stream frame (1 MiB window, as zstd makes from a pipe) that expands to size
// bytes of zeros.
func streamingZstdRequestBodyBomb(t *testing.T, size int) []byte {
	t.Helper()
	var out bytes.Buffer
	encoder, errNewWriter := zstd.NewWriter(&out, zstd.WithEncoderLevel(zstd.SpeedBestCompression), zstd.WithWindowSize(1<<20))
	if errNewWriter != nil {
		t.Fatal(errNewWriter)
	}
	chunk := make([]byte, 1<<20)
	for written := 0; written < size; written += len(chunk) {
		if _, errWrite := encoder.Write(chunk); errWrite != nil {
			t.Fatal(errWrite)
		}
	}
	if errClose := encoder.Close(); errClose != nil {
		t.Fatal(errClose)
	}
	return out.Bytes()
}

// A request body that expands past the 32 MiB fallback is rejected instead of being read whole into memory.
func TestReadRequestBodyRejectsAZstdExpansionPastTheBodyBound(t *testing.T) {
	gin.SetMode(gin.TestMode)
	bomb := streamingZstdRequestBodyBomb(t, 40<<20)
	for _, limit := range []int64{0, -1, 1 << 40, 4096} {
		decoded, errDecode := readZstdRequestBody(t, bomb, limit)
		if errDecode == nil {
			t.Fatalf("limit %d: a %d byte zstd expansion was decoded (%d bytes) instead of rejected", limit, len(bomb), len(decoded))
		}
	}

	// The endpoint path answers 400 instead of tearing the request down: the same call the OpenAI, image and video handlers make.
	router := gin.New()
	router.POST("/v1/chat/completions", func(c *gin.Context) {
		if _, errRead := ReadRequestBody(c); errRead != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": errRead.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(bomb))
	req.Header.Set("Content-Encoding", "zstd")
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for a %d byte zstd expansion", resp.Code, len(bomb))
	}
}

// The classic bomb (one segment that declares its whole content as its window, here 64 MiB from a few KB) is rejected before
// its window is allocated.
func TestReadRequestBodyDoesNotDecodeASingleSegmentBomb(t *testing.T) {
	gin.SetMode(gin.TestMode)
	encoder, errNewWriter := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedBestCompression))
	if errNewWriter != nil {
		t.Fatal(errNewWriter)
	}
	bomb := encoder.EncodeAll(make([]byte, 64<<20), nil)
	if len(bomb) > 1<<20 {
		t.Fatalf("the probe body is %d bytes, expected a few KB", len(bomb))
	}

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	decoded, errDecode := readZstdRequestBody(t, bomb, 0)
	runtime.ReadMemStats(&after)

	if errDecode == nil {
		t.Fatalf("a frame whose window is 64 MiB was decoded (%d bytes) instead of rejected", len(decoded))
	}
	if grown := after.TotalAlloc - before.TotalAlloc; grown > 16<<20 {
		t.Fatalf("rejecting the bomb still allocated %d MiB", grown>>20)
	}
}

// A frame that declares a 4 GiB window is rejected: a single segment frame carries its content size in the header, and in
// single segment mode the window is that size. The header alone is enough, so the frame carries one empty last raw block.
func TestReadRequestBodyRejectsAFourGiBZstdWindowFrame(t *testing.T) {
	gin.SetMode(gin.TestMode)
	frame := []byte{
		0x28, 0xb5, 0x2f, 0xfd, // zstd magic
		0xe0,                                           // frame header descriptor: single segment, 8-byte content size
		0x00, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, // content size 0x0000000100000000 = 4 GiB
		0x01, 0x00, 0x00, // last block, raw, size 0
	}
	decoded, errDecode := readZstdRequestBody(t, frame, 0)
	if errDecode == nil {
		t.Fatalf("a 4 GiB window frame was accepted (%d bytes decoded)", len(decoded))
	}
}

// An ordinary compressed body still decodes to exactly the bytes the client sent, and a configured smaller limit is honoured.
func TestReadRequestBodyDecodesAnOrdinaryZstdBody(t *testing.T) {
	gin.SetMode(gin.TestMode)
	payload := []byte(`{"model":"gpt-5","messages":[{"role":"user","content":"hello"}]}`)
	encoder, errNewWriter := zstd.NewWriter(nil)
	if errNewWriter != nil {
		t.Fatal(errNewWriter)
	}
	compressed := encoder.EncodeAll(payload, nil)

	decoded, errDecode := readZstdRequestBody(t, compressed, 0)
	if errDecode != nil {
		t.Fatalf("decoding an ordinary body failed: %v", errDecode)
	}
	if !bytes.Equal(decoded, payload) {
		t.Fatalf("decoded body = %q, want %q", decoded, payload)
	}

	decoded, errDecode = readZstdRequestBody(t, compressed, 4096)
	if errDecode != nil || !bytes.Equal(decoded, payload) {
		t.Fatalf("a body under the configured 4096 byte limit did not decode unchanged: %v (%d bytes)", errDecode, len(decoded))
	}

	large := bytes.Repeat([]byte("a"), 8192)
	largeCompressed := encoder.EncodeAll(large, nil)
	if _, errDecode = readZstdRequestBody(t, largeCompressed, 4096); errDecode == nil {
		t.Fatal("a body past the configured 4096 byte limit was decoded instead of rejected")
	}
}
