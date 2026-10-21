package cli

import (
	"bytes"
	"net"
	"net/http"
	"os"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestRunArgumentErrors(t *testing.T) {
	cases := []struct {
		name     string
		args     []string
		wantCode int
		wantErr  string // substring expected on stderr
	}{
		{"no command", nil, ExitUsage, "missing command"},
		{"unknown command", []string{"frobnicate"}, ExitUsage, `unknown command "frobnicate"`},
		{"version with extra args", []string{"version", "extra"}, ExitUsage, "unexpected arguments: extra"},
		{"serve unknown flag", []string{"serve", "-bogus"}, ExitUsage, "flag provided but not defined: -bogus"},
		{"serve non-numeric port", []string{"serve", "-port", "abc"}, ExitUsage, `invalid value "abc" for flag -port`},
		{"serve port too large", []string{"serve", "-port", "70000"}, ExitUsage, "outside the valid range"},
		{"serve negative port", []string{"serve", "-port", "-1"}, ExitUsage, "outside the valid range"},
		{"serve all interfaces", []string{"serve", "-host", "0.0.0.0"}, ExitUsage, "not a loopback address"},
		{"serve ipv6 any", []string{"serve", "-host", "::"}, ExitUsage, "not a loopback address"},
		{"serve lan address", []string{"serve", "-host", "192.168.1.10"}, ExitUsage, "not a loopback address"},
		{"serve hostname", []string{"serve", "-host", "localhost"}, ExitUsage, "not an IP address"},
		{"serve empty host", []string{"serve", "-host", ""}, ExitUsage, "bind host is empty"},
		{"serve bad log level", []string{"serve", "-log-level", "loud"}, ExitUsage, `unknown log level "loud"`},
		{"serve bad timeout", []string{"serve", "-shutdown-timeout", "0s"}, ExitUsage, "shutdown timeout must be positive"},
		{"serve positional arg", []string{"serve", "extra"}, ExitUsage, "unexpected arguments: extra"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := Run(tc.args, &stdout, &stderr)
			if code != tc.wantCode {
				t.Fatalf("exit code = %d, want %d; stderr:\n%s", code, tc.wantCode, stderr.String())
			}
			if !strings.Contains(stderr.String(), tc.wantErr) {
				t.Fatalf("stderr does not contain %q:\n%s", tc.wantErr, stderr.String())
			}
			if stdout.Len() != 0 {
				t.Fatalf("expected no stdout output on error, got:\n%s", stdout.String())
			}
		})
	}
}

func TestRunHelp(t *testing.T) {
	for _, args := range [][]string{{"help"}, {"-h"}, {"--help"}, {"serve", "-h"}, {"version", "-h"}} {
		var stdout, stderr bytes.Buffer
		if code := Run(args, &stdout, &stderr); code != ExitOK {
			t.Fatalf("%v: exit code = %d, want %d", args, code, ExitOK)
		}
		if !strings.Contains(stdout.String()+stderr.String(), "Usage:") {
			t.Fatalf("%v: expected usage text, got stdout=%q stderr=%q", args, stdout.String(), stderr.String())
		}
	}
}

func TestRunVersion(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"version"}, &stdout, &stderr); code != ExitOK {
		t.Fatalf("exit code = %d, want %d; stderr: %s", code, ExitOK, stderr.String())
	}
	out := stdout.String()
	if !strings.HasPrefix(out, "devatlas ") || !strings.Contains(out, "(commit ") || !strings.Contains(out, "go1.") {
		t.Fatalf("unexpected version output: %q", out)
	}
	if stderr.Len() != 0 {
		t.Fatalf("expected empty stderr, got: %s", stderr.String())
	}
}

// syncBuffer is a bytes.Buffer safe for concurrent writes and reads, so a
// test can watch the server log while serve runs in another goroutine.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func waitForLog(t *testing.T, buf *syncBuffer, substr string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(buf.String(), substr) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("log never contained %q:\n%s", substr, buf.String())
}

func TestServeOccupiedPortExitsWithFailure(t *testing.T) {
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	port := occupied.Addr().(*net.TCPAddr).Port

	var stdout, stderr bytes.Buffer
	code := Run([]string{"serve", "-port", strconv.Itoa(port)}, &stdout, &stderr)
	if code != ExitFailure {
		t.Fatalf("exit code = %d, want %d; stderr:\n%s", code, ExitFailure, stderr.String())
	}
	if !strings.Contains(stderr.String(), "already in use") {
		t.Fatalf("stderr does not explain the port conflict:\n%s", stderr.String())
	}
}

// TestServeStopsOnInterrupt runs the real serve command and delivers SIGINT
// to the test process, exercising the signal-to-graceful-shutdown path.
func TestServeStopsOnInterrupt(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("sending SIGINT to self is not supported on Windows")
	}
	var stdout bytes.Buffer
	stderr := &syncBuffer{}
	done := make(chan int, 1)
	go func() { done <- Run([]string{"serve", "-port", "0", "-shutdown-timeout", "2s"}, &stdout, stderr) }()

	// The listening line is logged after the signal handler is installed,
	// so SIGINT sent after it is guaranteed to be caught and not kill us.
	waitForLog(t, stderr, "devatlas listening")
	url := regexp.MustCompile(`url=(http://\S+)`).FindStringSubmatch(stderr.String())
	if url == nil {
		t.Fatalf("no url in log:\n%s", stderr.String())
	}
	resp, err := http.Get(url[1] + "/health")
	if err != nil {
		t.Fatalf("GET /health: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("health status = %d", resp.StatusCode)
	}

	if err := syscall.Kill(os.Getpid(), syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	select {
	case code := <-done:
		if code != ExitOK {
			t.Fatalf("exit code after SIGINT = %d, want 0; stderr:\n%s", code, stderr.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("serve did not stop after SIGINT; stderr:\n%s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "shutdown complete") {
		t.Errorf("expected 'shutdown complete' in log:\n%s", stderr.String())
	}
}
