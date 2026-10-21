package git

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// commandError carries a classified failure of one git command. reason is
// safe to show; stderr is for logs only.
type commandError struct {
	subcommand string
	status     Status
	reason     string
	exitCode   int
	stderr     string
}

func (e *commandError) Error() string {
	return fmt.Sprintf("git %s: %s (status %s)", e.subcommand, e.reason, e.status)
}

// Environment variables that would redirect Git to another repository or
// index. They are removed so an inherited environment cannot change what is
// observed.
var strippedEnv = []string{
	"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_COMMON_DIR",
	"GIT_OBJECT_DIRECTORY", "GIT_ALTERNATE_OBJECT_DIRECTORIES",
	"GIT_NAMESPACE", "GIT_PREFIX",
}

const maxStderrBytes = 2048

// run executes one git subcommand in dir with a per-command timeout and a
// bounded stdout. Arguments are passed as separate strings; no shell is
// involved. Git is told not to prompt, not to take optional locks, and not
// to start a file-system monitor.
func (c *Collector) run(ctx context.Context, dir string, args ...string) ([]byte, error) {
	sub := args[0]
	ctx, cancel := context.WithTimeout(ctx, c.opts.CommandTimeout)
	defer cancel()

	full := append([]string{
		"-C", dir,
		"--no-optional-locks",
		"-c", "core.fsmonitor=false",
		"-c", "core.hooksPath=/dev/null",
	}, args...)
	cmd := exec.CommandContext(ctx, c.gitPath, full...)
	cmd.Env = append(filteredEnv(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_OPTIONAL_LOCKS=0",
		"GIT_ASKPASS=",
		"SSH_ASKPASS=",
		"LC_ALL=C",
	)
	cmd.Stdin = nil // the null device
	cmd.WaitDelay = time.Second
	isolateProcess(cmd)
	var stderr cappedBuffer
	stderr.limit = maxStderrBytes
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, &commandError{subcommand: sub, status: StatusUnavailable, reason: "git could not be started"}
	}
	if err := cmd.Start(); err != nil {
		if errors.Is(err, exec.ErrNotFound) || errors.Is(err, os.ErrNotExist) {
			return nil, &commandError{subcommand: sub, status: StatusUnavailable, reason: "git executable not found"}
		}
		return nil, &commandError{subcommand: sub, status: StatusUnavailable, reason: "git could not be started", stderr: err.Error()}
	}

	out, readErr := readCapped(stdout, c.opts.MaxOutputBytes)
	if readErr != nil {
		killProcess(cmd)
	}
	waitErr := cmd.Wait()

	if errors.Is(readErr, errOutputLimit) {
		return nil, &commandError{subcommand: sub, status: StatusError, reason: fmt.Sprintf("git %s produced more than %d bytes of output", sub, c.opts.MaxOutputBytes)}
	}
	if waitErr == nil && readErr == nil {
		return out, nil
	}
	if waitErr == nil {
		waitErr = readErr
	}
	return nil, c.classify(ctx, sub, waitErr, stderr.String())
}

func (c *Collector) classify(ctx context.Context, sub string, err error, stderr string) *commandError {
	ce := &commandError{subcommand: sub, status: StatusError, stderr: strings.TrimSpace(stderr)}
	if ctxErr := ctx.Err(); ctxErr != nil {
		if errors.Is(ctxErr, context.DeadlineExceeded) {
			ce.status, ce.reason = StatusTimeout, fmt.Sprintf("git %s did not finish within the collection deadline", sub)
		} else {
			ce.reason = "collection cancelled"
		}
		ce.stderr = ""
		return ce
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		ce.exitCode = exitErr.ExitCode()
		lower := strings.ToLower(stderr)
		switch {
		case strings.Contains(lower, "not a git repository"):
			ce.status, ce.reason, ce.stderr = StatusNotRepository, "not a Git repository", ""
		case strings.Contains(lower, "dubious ownership"):
			ce.reason = "repository is owned by another user; see git config safe.directory"
		case strings.Contains(lower, "must be run in a work tree"):
			ce.reason = "bare repository without a work tree"
		case strings.Contains(lower, "unknown option") || strings.Contains(lower, "usage:"):
			ce.reason = fmt.Sprintf("git %s rejected an option; Git 2.31 or newer is required", sub)
		default:
			ce.reason = fmt.Sprintf("git %s exited with status %d", sub, ce.exitCode)
		}
		return ce
	}
	ce.reason = fmt.Sprintf("git %s failed to run", sub)
	return ce
}

var errOutputLimit = errors.New("output limit exceeded")

// readCapped reads at most limit bytes; more is an error, and the caller
// kills the process so it cannot block on a full pipe.
func readCapped(r io.Reader, limit int64) ([]byte, error) {
	var buf bytes.Buffer
	n, err := io.CopyN(&buf, r, limit+1)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if n > limit {
		return nil, errOutputLimit
	}
	return buf.Bytes(), nil
}

// cappedBuffer keeps the first limit bytes and silently drops the rest.
type cappedBuffer struct {
	buf   bytes.Buffer
	limit int
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	if room := b.limit - b.buf.Len(); room > 0 {
		if len(p) > room {
			p = p[:room]
		}
		b.buf.Write(p)
	}
	return len(p), nil
}

func (b *cappedBuffer) String() string { return b.buf.String() }

func filteredEnv() []string {
	env := os.Environ()
	out := env[:0:0]
	for _, kv := range env {
		key, _, _ := strings.Cut(kv, "=")
		skip := false
		for _, s := range strippedEnv {
			if key == s {
				skip = true
				break
			}
		}
		if !skip {
			out = append(out, kv)
		}
	}
	return out
}

func splitLines(b []byte) []string {
	s := strings.TrimRight(string(b), "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

// repositoryRoot derives the main work tree from the common Git directory.
func repositoryRoot(commonDir string) string {
	if filepath.Base(commonDir) == ".git" {
		return filepath.Dir(commonDir)
	}
	return commonDir
}

func sameDir(a, b string) bool {
	ra, errA := filepath.EvalSymlinks(a)
	rb, errB := filepath.EvalSymlinks(b)
	if errA != nil || errB != nil {
		return filepath.Clean(a) == filepath.Clean(b)
	}
	return ra == rb
}
