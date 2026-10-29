package api

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// transientAcceptErrorListener fails accept with EMFILE a fixed number of times
// before delegating, the way a momentarily descriptor-starved server does.
type transientAcceptErrorListener struct {
	net.Listener
	remaining atomic.Int32
}

func (l *transientAcceptErrorListener) Accept() (net.Conn, error) {
	for {
		remaining := l.remaining.Load()
		if remaining <= 0 {
			return l.Listener.Accept()
		}
		if l.remaining.CompareAndSwap(remaining, remaining-1) {
			return nil, &net.OpError{Op: "accept", Net: "tcp", Err: os.NewSyscallError("accept4", syscall.EMFILE)}
		}
	}
}

func TestIsTransientAcceptError(t *testing.T) {
	emfile := &net.OpError{Op: "accept", Net: "tcp", Err: os.NewSyscallError("accept4", syscall.EMFILE)}
	enfile := &net.OpError{Op: "accept", Net: "tcp", Err: os.NewSyscallError("accept4", syscall.ENFILE)}
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{name: "EMFILE", err: emfile, want: true},
		{name: "ENFILE", err: enfile, want: true},
		{name: "closed", err: net.ErrClosed, want: false},
		{name: "nil", err: nil, want: false},
		{name: "other", err: io.ErrUnexpectedEOF, want: false},
	}
	for _, tc := range cases {
		if got := isTransientAcceptError(tc.err); got != tc.want {
			t.Fatalf("%s: isTransientAcceptError = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// A transient accept failure must not stop the server: the accept loop retries
// with backoff and keeps serving once descriptors free up.
func TestAcceptMuxRecoversFromTransientEMFILE(t *testing.T) {
	base, errListen := net.Listen("tcp", "127.0.0.1:0")
	if errListen != nil {
		t.Fatalf("listen: %v", errListen)
	}
	defer func() { _ = base.Close() }()

	listener := &transientAcceptErrorListener{Listener: base}
	listener.remaining.Store(3)

	var routed atomic.Int32
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		routed.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	muxLn := newMuxListener(base.Addr(), 1024)
	server := &Server{managementRoutesEnabled: atomic.Bool{}}
	errCh := make(chan error, 1)
	go func() { errCh <- server.acceptMuxConnections(listener, muxLn) }()

	srv.Listener = muxLn
	srv.Start()

	client := &http.Client{Timeout: 3 * time.Second}
	resp, errGet := client.Get("http://" + base.Addr().String() + "/")
	if errGet != nil {
		t.Fatalf("HTTP request failed after transient EMFILE: %v", errGet)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	if routed.Load() != 1 {
		t.Fatalf("routed = %d, want 1", routed.Load())
	}
	if remaining := listener.remaining.Load(); remaining != 0 {
		t.Fatalf("remaining injected failures = %d, want 0", remaining)
	}
	select {
	case errAccept := <-errCh:
		t.Fatalf("accept loop stopped: %v", errAccept)
	default:
	}
}

// An unauthenticated client that opens a shared-port connection, sends the RESP
// prefix and then nothing must be dropped once the setup deadline expires.
func TestRedisSetupDeadlineDropsIdleUnauthenticatedConnection(t *testing.T) {
	previous := redisSetupReadDeadline
	redisSetupReadDeadline = 100 * time.Millisecond
	t.Cleanup(func() { redisSetupReadDeadline = previous })

	serverConn, clientConn := net.Pipe()
	defer func() { _ = clientConn.Close() }()
	// Drain the client side so the server's error write cannot block.
	go func() { _, _ = io.Copy(io.Discard, clientConn) }()

	server := &Server{managementRoutesEnabled: atomic.Bool{}}
	server.managementRoutesEnabled.Store(true)

	done := make(chan struct{})
	go func() {
		server.handleRedisConnection(serverConn, nil)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handleRedisConnection kept an idle unauthenticated connection open past the setup deadline")
	}
}

// A response written while the request body is still unread must close the
// connection instead of waiting for the rest of the body.
func TestUnconsumedRequestBodyClosesConnection(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"missing api key"}`))
	})
	srv := httptest.NewServer(wrapUnconsumedRequestBodyHandler(handler))
	defer srv.Close()

	addr := strings.TrimPrefix(srv.URL, "http://")
	conn, errDial := net.DialTimeout("tcp", addr, time.Second)
	if errDial != nil {
		t.Fatalf("dial: %v", errDial)
	}
	defer func() { _ = conn.Close() }()

	request := "POST /v1/chat/completions HTTP/1.1\r\nHost: " + addr +
		"\r\nContent-Type: application/json\r\nContent-Length: 1048577\r\n\r\n{"
	if _, errWrite := conn.Write([]byte(request)); errWrite != nil {
		t.Fatalf("write partial request: %v", errWrite)
	}
	if errDeadline := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); errDeadline != nil {
		t.Fatalf("set read deadline: %v", errDeadline)
	}

	rawResponse, errRead := io.ReadAll(conn)
	if errRead != nil {
		t.Fatalf("server did not close the connection promptly (err=%v, partial=%q)", errRead, string(rawResponse))
	}
	responseText := string(rawResponse)
	if !strings.HasPrefix(responseText, "HTTP/1.1 401 ") {
		t.Fatalf("response = %q, want a 401", responseText)
	}
	if !strings.Contains(strings.ToLower(responseText), "connection: close") {
		t.Fatalf("response is missing Connection: close: %q", responseText)
	}
}

// A fully consumed body must not force the connection closed.
func TestConsumedRequestBodyKeepsConnectionReusable(t *testing.T) {
	var requestCount atomic.Int32
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount.Add(1)
		if _, errRead := io.ReadAll(r.Body); errRead != nil {
			t.Errorf("read body: %v", errRead)
		}
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(wrapUnconsumedRequestBodyHandler(handler))
	defer srv.Close()

	for i := range 2 {
		resp, errPost := http.Post(srv.URL, "application/json", strings.NewReader(`{"model":"test"}`))
		if errPost != nil {
			t.Fatalf("request %d: %v", i, errPost)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("request %d status = %d, want 200", i, resp.StatusCode)
		}
		if strings.Contains(strings.ToLower(strings.Join(resp.Header.Values("Connection"), ",")), "close") {
			t.Fatalf("request %d forced Connection: close on a fully consumed body", i)
		}
	}
	if requestCount.Load() != 2 {
		t.Fatalf("handled %d requests, want 2", requestCount.Load())
	}
}
