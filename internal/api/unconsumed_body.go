package api

import (
	"bufio"
	"errors"
	"io"
	"net"
	"net/http"
	"time"
)

// requestBodyTracker records how much of a request body the handler consumed,
// so the server can tell a fully handled request from one whose body was left
// unread (for example an unauthenticated request rejected before its body).
type requestBodyTracker struct {
	io.ReadCloser
	contentLength int64
	bytesRead     int64
	sawEOF        bool
}

func (t *requestBodyTracker) Read(p []byte) (int, error) {
	if t == nil || t.ReadCloser == nil {
		return 0, io.EOF
	}
	n, errRead := t.ReadCloser.Read(p)
	if n > 0 {
		t.bytesRead += int64(n)
	}
	if errors.Is(errRead, io.EOF) {
		t.sawEOF = true
	}
	return n, errRead
}

func (t *requestBodyTracker) isComplete() bool {
	if t == nil {
		return true
	}
	return t.sawEOF || (t.contentLength >= 0 && t.bytesRead >= t.contentLength)
}

// unconsumedBodyResponseWriter closes the connection once a response is written
// while the request body is still unread. Without it the server waits for the
// rest of a body the client may never send, holding the descriptor open for the
// whole wait.
type unconsumedBodyResponseWriter struct {
	http.ResponseWriter
	tracker     *requestBodyTracker
	status      int
	wroteHeader bool
	hijacked    bool
	closed      bool
}

func (w *unconsumedBodyResponseWriter) Unwrap() http.ResponseWriter {
	if w == nil {
		return nil
	}
	return w.ResponseWriter
}

func (w *unconsumedBodyResponseWriter) WriteHeader(code int) {
	w.status = code
	w.closeIfBodyUnconsumed(code)
	w.wroteHeader = true
	w.ResponseWriter.WriteHeader(code)
}

func (w *unconsumedBodyResponseWriter) Write(data []byte) (int, error) {
	w.markHeaderWritten()
	return w.ResponseWriter.Write(data)
}

func (w *unconsumedBodyResponseWriter) WriteString(s string) (int, error) {
	w.markHeaderWritten()
	return io.WriteString(w.ResponseWriter, s)
}

func (w *unconsumedBodyResponseWriter) markHeaderWritten() {
	if w.wroteHeader {
		return
	}
	if w.status == 0 {
		w.status = http.StatusOK
	}
	w.closeIfBodyUnconsumed(w.status)
	w.wroteHeader = true
}

func (w *unconsumedBodyResponseWriter) Flush() {
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (w *unconsumedBodyResponseWriter) FlushError() error {
	if errFlusher, ok := w.ResponseWriter.(interface{ FlushError() error }); ok {
		return errFlusher.FlushError()
	}
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
	return nil
}

func (w *unconsumedBodyResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hijacker, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, http.ErrNotSupported
	}
	conn, readWriter, errHijack := hijacker.Hijack()
	if errHijack == nil {
		w.hijacked = true
	}
	return conn, readWriter, errHijack
}

func (w *unconsumedBodyResponseWriter) Push(target string, opts *http.PushOptions) error {
	if pusher, ok := w.ResponseWriter.(http.Pusher); ok {
		return pusher.Push(target, opts)
	}
	return http.ErrNotSupported
}

// closeIfBodyUnconsumed marks the response for connection close and expires the
// connection's read deadline, so the server does not wait on a body the client
// has not finished sending.
func (w *unconsumedBodyResponseWriter) closeIfBodyUnconsumed(status int) {
	if w == nil || w.closed || w.hijacked || status < http.StatusOK {
		return
	}
	if w.tracker != nil && w.tracker.isComplete() {
		return
	}
	w.closed = true
	if !w.wroteHeader {
		w.ResponseWriter.Header().Set("Connection", "close")
	}
	_ = http.NewResponseController(w.ResponseWriter).SetReadDeadline(time.Unix(1, 0))
}

// wrapUnconsumedRequestBodyHandler guards the HTTP server handler: when a
// response finishes while the request body is still unread, the connection is
// closed instead of drained.
func wrapUnconsumedRequestBodyHandler(next http.Handler) http.Handler {
	if next == nil {
		return nil
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r == nil || r.Body == nil || r.Body == http.NoBody || (r.ContentLength == 0 && len(r.TransferEncoding) == 0) {
			next.ServeHTTP(w, r)
			return
		}
		tracker, ok := r.Body.(*requestBodyTracker)
		if !ok {
			tracker = &requestBodyTracker{
				ReadCloser:    r.Body,
				contentLength: r.ContentLength,
			}
			r.Body = tracker
		}
		wrappedWriter := &unconsumedBodyResponseWriter{
			ResponseWriter: w,
			tracker:        tracker,
		}
		defer func() {
			status := wrappedWriter.status
			if status == 0 {
				status = http.StatusOK
			}
			wrappedWriter.closeIfBodyUnconsumed(status)
		}()
		next.ServeHTTP(wrappedWriter, r)
	})
}
