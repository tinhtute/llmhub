package handlers

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/klauspost/compress/zstd"
)

const (
	// maxRequestBodyZstdWindowBytes is the largest zstd window a request body may declare: 8 MiB covers every zstd level up
	// to 19, so a frame declaring more is rejected instead of allocating it. Twin of maxCapturedZstdWindowBytes in
	// internal/api/middleware/request_logging.go; the two decode paths must bound a request body identically.
	maxRequestBodyZstdWindowBytes = 8 << 20
	// maxRequestBodyDecodedBytes caps a decoded request body until max-request-body-bytes is configured, matching
	// maxDeferredErrorRequestBodyBytes in internal/api/middleware/request_logging.go.
	maxRequestBodyDecodedBytes int64 = 32 << 20
)

// ReadRequestBody reads the incoming request body and decodes supported
// Content-Encoding values before handlers inspect JSON fields.
func ReadRequestBody(c *gin.Context) ([]byte, error) {
	raw, err := c.GetRawData()
	if err != nil {
		return nil, err
	}

	encoding := ""
	if c != nil && c.Request != nil {
		encoding = strings.TrimSpace(c.Request.Header.Get("Content-Encoding"))
	}
	if encoding == "" || strings.EqualFold(encoding, "identity") {
		return raw, nil
	}

	decoded, err := decodeRequestBody(raw, encoding, requestBodyDecodedLimit(c))
	if err != nil {
		if json.Valid(raw) {
			return raw, nil
		}
		return nil, err
	}
	return decoded, nil
}

// requestBodyDecodedLimit returns the cap for the decoded request body: max-request-body-bytes when it is set below the
// fallback, otherwise the fallback. The request-log middleware clamps the same key the same way.
func requestBodyDecodedLimit(c *gin.Context) int64 {
	if c != nil {
		if configured := c.GetInt64("maxRequestBodyBytes"); configured > 0 && configured < maxRequestBodyDecodedBytes {
			return configured
		}
	}
	return maxRequestBodyDecodedBytes
}

func decodeRequestBody(raw []byte, encoding string, limit int64) ([]byte, error) {
	parts := strings.Split(encoding, ",")
	body := raw
	for i := len(parts) - 1; i >= 0; i-- {
		enc := strings.ToLower(strings.TrimSpace(parts[i]))
		switch enc {
		case "", "identity":
			continue
		case "zstd":
			decoded, err := decodeZstdRequestBody(body, limit)
			if err != nil {
				return nil, err
			}
			body = decoded
		default:
			return nil, fmt.Errorf("unsupported request content encoding: %s", enc)
		}
	}
	return body, nil
}

func decodeZstdRequestBody(raw []byte, limit int64) ([]byte, error) {
	// One decoder goroutine and a bounded window: a small frame can declare a multi-gigabyte window, and decoding it would
	// allocate that window for every tenant. The bound rejects the frame before any of it is allocated.
	decoder, err := zstd.NewReader(bytes.NewReader(raw), zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxWindow(maxRequestBodyZstdWindowBytes))
	if err != nil {
		return nil, fmt.Errorf("failed to create zstd request decoder: %w", err)
	}
	defer decoder.Close()

	decoded, err := io.ReadAll(io.LimitReader(decoder, limit+1))
	if err != nil {
		return nil, fmt.Errorf("failed to decode zstd request body: %w", err)
	}
	if int64(len(decoded)) > limit {
		return nil, fmt.Errorf("decoded zstd request body exceeds the %d byte limit", limit)
	}
	return decoded, nil
}
