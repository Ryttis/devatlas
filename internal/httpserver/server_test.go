package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Ryttis/devatlas/internal/registryfile"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// testStore returns a registry store backed by a fresh temporary file.
func testStore(t *testing.T) *registryfile.Store {
	t.Helper()
	return registryfile.NewStore(filepath.Join(t.TempDir(), "registrations.yaml"))
}

func TestConfigRejectsNonLoopbackHosts(t *testing.T) {
	bad := []string{"", "0.0.0.0", "::", "192.168.1.10", "10.0.0.1", "172.17.0.1", "8.8.8.8", "localhost", "example.com", "127.0.0.1:80", "not an ip"}
	for _, host := range bad {
		cfg := DefaultConfig()
		cfg.Host = host
		if err := cfg.Validate(); err == nil {
			t.Errorf("host %q: expected validation error, got nil", host)
		}
		if _, err := New(cfg, testStore(t), testLogger()); err == nil {
			t.Errorf("host %q: New should fail for a non-loopback host", host)
		}
	}
}

func TestConfigAcceptsLoopbackHosts(t *testing.T) {
	for _, host := range []string{"127.0.0.1", "127.0.0.2", "127.255.255.254", "::1"} {
		cfg := DefaultConfig()
		cfg.Host = host
		if err := cfg.Validate(); err != nil {
			t.Errorf("host %q: unexpected error: %v", host, err)
		}
	}
}

func TestConfigRejectsBadPortAndTimeout(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Port = 65536
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "outside the valid range") {
		t.Errorf("port 65536: got %v", err)
	}
	cfg = DefaultConfig()
	cfg.Port = -5
	if err := cfg.Validate(); err == nil {
		t.Error("port -5: expected error")
	}
	cfg = DefaultConfig()
	cfg.ShutdownTimeout = 0
	if err := cfg.Validate(); err == nil {
		t.Error("zero shutdown timeout: expected error")
	}
}

// startTestServer listens on an ephemeral loopback port and serves until the
// returned stop function is called. stop returns the Serve error.
func startTestServer(t *testing.T) (base string, stop func() error) {
	t.Helper()
	return startTestServerWith(t, testStore(t))
}

func startTestServerWith(t *testing.T, store *registryfile.Store) (base string, stop func() error) {
	t.Helper()
	cfg := DefaultConfig()
	cfg.Port = 0
	cfg.ShutdownTimeout = 2 * time.Second
	srv, err := New(cfg, store, testLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ln, err := srv.Listen()
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx, ln) }()

	stop = func() error {
		cancel()
		select {
		case err := <-done:
			return err
		case <-time.After(5 * time.Second):
			return errors.New("server did not stop within 5s")
		}
	}
	return "http://" + ln.Addr().String(), stop
}

func TestHealthEndpoint(t *testing.T) {
	base, stop := startTestServer(t)
	defer func() {
		if err := stop(); err != nil {
			t.Errorf("stop: %v", err)
		}
	}()

	resp, err := http.Get(base + "/health")
	if err != nil {
		t.Fatalf("GET /health: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, key := range []string{"status", "service", "version", "commit", "modified", "checked_at", "uptime_seconds"} {
		if _, ok := fields[key]; !ok {
			t.Errorf("health JSON is missing field %q: %s", key, raw)
		}
	}
	var h Health
	if err := json.Unmarshal(raw, &h); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if h.Status != "ok" || h.Service != "devatlas" {
		t.Errorf("unexpected body: %+v", h)
	}
	if h.Version == "" || h.Commit == "" {
		t.Errorf("version/commit must be populated: %+v", h)
	}
	if time.Since(h.CheckedAt) > time.Minute || h.CheckedAt.IsZero() {
		t.Errorf("checked_at is not current: %v", h.CheckedAt)
	}
	if h.UptimeSeconds < 0 {
		t.Errorf("uptime_seconds negative: %d", h.UptimeSeconds)
	}

	// Only GET is served.
	post, err := http.Post(base+"/health", "text/plain", strings.NewReader(""))
	if err != nil {
		t.Fatalf("POST /health: %v", err)
	}
	post.Body.Close()
	if post.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST /health status = %d, want 405", post.StatusCode)
	}
}

func TestHomePage(t *testing.T) {
	base, stop := startTestServer(t)
	defer func() {
		if err := stop(); err != nil {
			t.Errorf("stop: %v", err)
		}
	}()

	resp, err := http.Get(base + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("Content-Type = %q, want text/html", ct)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"<title>DevAtlas</title>", "Implementation scope", "Not yet implemented", "/health", "127.0.0.1"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("home page missing %q", want)
		}
	}

	// Unknown paths are not served by the home handler.
	nf, err := http.Get(base + "/does-not-exist")
	if err != nil {
		t.Fatal(err)
	}
	nf.Body.Close()
	if nf.StatusCode != http.StatusNotFound {
		t.Errorf("GET /does-not-exist status = %d, want 404", nf.StatusCode)
	}
}

func TestListenReportsOccupiedPort(t *testing.T) {
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	port := occupied.Addr().(*net.TCPAddr).Port

	cfg := DefaultConfig()
	cfg.Port = port
	srv, err := New(cfg, testStore(t), testLogger())
	if err != nil {
		t.Fatal(err)
	}
	ln, err := srv.Listen()
	if err == nil {
		ln.Close()
		t.Fatal("expected an error listening on an occupied port")
	}
	var inUse *AddrInUseError
	if !errors.As(err, &inUse) {
		t.Fatalf("error is %T (%v), want *AddrInUseError", err, err)
	}
	if !strings.Contains(err.Error(), "already in use") || !strings.Contains(err.Error(), cfg.Addr()) {
		t.Errorf("unhelpful error message: %v", err)
	}
}

func TestGracefulShutdownWaitsForInFlightRequest(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Port = 0
	cfg.ShutdownTimeout = 3 * time.Second
	srv, err := New(cfg, testStore(t), testLogger())
	if err != nil {
		t.Fatal(err)
	}
	// Swap in a slow handler to observe that Shutdown waits for it.
	entered := make(chan struct{})
	release := make(chan struct{})
	srv.http.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		w.WriteHeader(http.StatusOK)
	})
	ln, err := srv.Listen()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx, ln) }()

	result := make(chan int, 1)
	go func() {
		resp, err := http.Get("http://" + ln.Addr().String() + "/")
		if err != nil {
			result <- -1
			return
		}
		resp.Body.Close()
		result <- resp.StatusCode
	}()
	select {
	case <-entered: // the request is now in flight
	case <-time.After(5 * time.Second):
		t.Fatal("request never reached the handler")
	}
	cancel() // begin graceful shutdown

	select {
	case <-done:
		t.Fatal("Serve returned before the in-flight request completed")
	case <-time.After(200 * time.Millisecond):
	}
	close(release)

	if code := <-result; code != http.StatusOK {
		t.Errorf("in-flight request status = %d, want 200", code)
	}
	if err := <-done; err != nil {
		t.Errorf("Serve returned error after graceful shutdown: %v", err)
	}
}
