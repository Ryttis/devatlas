package cli

import (
	"bytes"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/Ryttis/devatlas/internal/registry"
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

func runCLI(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := Run(args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func TestRegistrationCommandsEndToEnd(t *testing.T) {
	root := t.TempDir()
	cfg := filepath.Join(root, "conf", "registrations.yaml")
	t.Setenv("DEVATLAS_CONFIG", cfg) // used by commands that do not pass -config
	repo := filepath.Join(root, "repo")
	wt := filepath.Join(root, "repo-wt")
	os.Mkdir(repo, 0o755)
	os.Mkdir(wt, 0o755)

	// Usage errors.
	for _, args := range [][]string{
		{"add"},       // no directory
		{"add", repo}, // no -project
		{"add", "-project", "Bad", "-name", "x", repo}, // bad id
		{"add", "-project", "p", "-name", "x", repo, "extra"},
		{"remove"},
		{"list", "extra"},
	} {
		if code, _, _ := runCLI(t, args...); code != ExitUsage {
			t.Errorf("%v: exit %d, want %d", args, code, ExitUsage)
		}
	}
	// Missing directory and missing name are usage errors too.
	if code, _, errOut := runCLI(t, "add", "-project", "p", "-name", "x", filepath.Join(root, "nope")); code != ExitUsage || !strings.Contains(errOut, "does not exist") {
		t.Errorf("missing dir: %d %s", code, errOut)
	}
	if code, _, errOut := runCLI(t, "add", "-project", "p", repo); code != ExitUsage || !strings.Contains(errOut, "project name is required") {
		t.Errorf("missing name: %d %s", code, errOut)
	}

	// Empty list.
	code, out, _ := runCLI(t, "list")
	if code != ExitOK || !strings.Contains(out, "no projects registered") || !strings.Contains(out, cfg) {
		t.Fatalf("empty list: %d %s", code, out)
	}

	// Add, with -config flag overriding the environment.
	code, out, errOut := runCLI(t, "add", "-config", cfg, "-project", "demo", "-name", "Demo Project", repo)
	if code != ExitOK || !strings.Contains(out, "created project demo") || !strings.Contains(out, "registered checkout ") {
		t.Fatalf("add: %d\n%s\n%s", code, out, errOut)
	}
	id := regexp.MustCompile(`registered checkout ([0-9a-f]{12})`).FindStringSubmatch(out)[1]

	// Duplicate (through a symlink) fails with exit 1 and names the checkout.
	link := filepath.Join(root, "link")
	if err := os.Symlink(repo, link); err == nil {
		code, _, errOut = runCLI(t, "add", "-project", "demo", link)
		if code != ExitFailure || !strings.Contains(errOut, "already registered") || !strings.Contains(errOut, id) {
			t.Errorf("duplicate via symlink: %d %s", code, errOut)
		}
	}
	// Name mismatch fails.
	if code, _, errOut = runCLI(t, "add", "-project", "demo", "-name", "Other", wt); code != ExitFailure || !strings.Contains(errOut, "different name") {
		t.Errorf("name mismatch: %d %s", code, errOut)
	}
	// Worktree as second checkout.
	if code, out, errOut = runCLI(t, "add", "-project", "demo", wt); code != ExitOK || strings.Contains(out, "created project") {
		t.Fatalf("add worktree: %d %s %s", code, out, errOut)
	}

	// List shows both; JSON matches the API shape.
	code, out, _ = runCLI(t, "list")
	if code != ExitOK || strings.Count(out, "demo") < 2 || !strings.Contains(out, "present") || !strings.Contains(out, "not collected") {
		t.Fatalf("list: %d\n%s", code, out)
	}
	code, out, _ = runCLI(t, "list", "-json")
	if code != ExitOK || !strings.Contains(out, `"schema_version": 1`) || strings.Count(out, `"runtime"`) != 2 {
		t.Fatalf("list -json: %d\n%s", code, out)
	}

	// Remove unknown, then remove both; the project goes with the last one.
	if code, _, errOut = runCLI(t, "remove", "zzz"); code != ExitFailure || !strings.Contains(errOut, "unknown checkout ID") {
		t.Errorf("remove unknown: %d %s", code, errOut)
	}
	marker := filepath.Join(repo, "keep.txt")
	os.WriteFile(marker, []byte("x"), 0o600)
	if code, out, _ = runCLI(t, "remove", id); code != ExitOK || strings.Contains(out, "removed project") {
		t.Fatalf("remove first: %d %s", code, out)
	}
	wtID := registry.CheckoutID(mustCanonical(t, wt))
	if code, out, _ = runCLI(t, "remove", wtID); code != ExitOK || !strings.Contains(out, "removed project demo") {
		t.Fatalf("remove last: %d %s", code, out)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("remove touched project files")
	}
	code, out, _ = runCLI(t, "list")
	if code != ExitOK || !strings.Contains(out, "no projects registered") {
		t.Fatalf("list after removal: %s", out)
	}

	// Malformed file: every command reports it and leaves it alone.
	bad := "schema_version: 1\nprojects: [\n"
	os.WriteFile(cfg, []byte(bad), 0o600)
	for _, args := range [][]string{{"list"}, {"add", "-project", "x", "-name", "X", repo}, {"remove", id}, {"serve", "-port", "0"}} {
		code, _, errOut := runCLI(t, args...)
		if code != ExitFailure || !strings.Contains(errOut, "malformed registration file") {
			t.Errorf("%v on malformed file: exit %d, stderr %s", args, code, errOut)
		}
	}
	if after, _ := os.ReadFile(cfg); string(after) != bad {
		t.Fatal("malformed file was rewritten")
	}
}

func mustCanonical(t *testing.T, p string) string {
	t.Helper()
	c, err := registry.CanonicalDir(p)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestListShowsGitObservations(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	root := t.TempDir()
	cfg := filepath.Join(root, "registrations.yaml")
	t.Setenv("DEVATLAS_CONFIG", cfg)
	repo := filepath.Join(root, "repo")
	os.Mkdir(repo, 0o755)
	for _, args := range [][]string{{"-c", "init.defaultBranch=main", "init", "-q"}, {"remote", "add", "origin", "git@github.com:o/r.git"}} {
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	plain := filepath.Join(root, "plain")
	os.Mkdir(plain, 0o755)
	if code, _, errOut := runCLI(t, "add", "-project", "p", "-name", "P", repo); code != ExitOK {
		t.Fatal(errOut)
	}
	if code, _, errOut := runCLI(t, "add", "-project", "p", plain); code != ExitOK {
		t.Fatal(errOut)
	}

	code, out, errOut := runCLI(t, "list")
	if code != ExitOK {
		t.Fatalf("list: %d %s", code, errOut)
	}
	for _, want := range []string{"GIT", "BRANCH", "REMOTES", "ok", "main (no commits)", "origin=github.com[github]", "not_git", "git not_git: not a Git repository", "observed now by running git locally", "regardless of Git state"} {
		if !strings.Contains(out, want) {
			t.Errorf("list output missing %q:\n%s", want, out)
		}
	}
	code, out, _ = runCLI(t, "list", "-json")
	if code != ExitOK || !strings.Contains(out, `"collection"`) || !strings.Contains(out, `"status": "ok"`) || !strings.Contains(out, `"status": "not_git"`) {
		t.Fatalf("list -json:\n%s", out)
	}
	code, out, _ = runCLI(t, "list", "-no-git")
	if code != ExitOK || !strings.Contains(out, "not observed") || strings.Contains(out, "main") {
		t.Fatalf("list -no-git:\n%s", out)
	}
}
