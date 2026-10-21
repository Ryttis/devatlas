// Package git observes the state of registered checkouts by running the local
// git command. It is strictly read-only: it never fetches, checks out, stages,
// commits, or changes configuration, and it asks Git not to take optional
// locks or refresh the index. Every observation records when it was collected
// and from which source, and a failure of one checkout never affects another.
package git

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"sync"
	"time"
)

// Status of one observation.
type Status string

// Observation statuses. Exactly one applies.
const (
	// StatusOK means every Git command succeeded and Repository is set.
	StatusOK Status = "ok"
	// StatusNotRepository means the directory exists but is not inside a
	// Git work tree.
	StatusNotRepository Status = "not_git"
	// StatusMissing means the registered path does not exist or is not a
	// directory, so Git was not run.
	StatusMissing Status = "missing"
	// StatusUnavailable means the git executable could not be found or run.
	StatusUnavailable Status = "unavailable"
	// StatusTimeout means a Git command exceeded the collection deadline.
	StatusTimeout Status = "timeout"
	// StatusError means Git ran but failed, or the request was cancelled.
	StatusError Status = "error"
)

// Source identifies how observations are produced.
const Source = "git-cli"

// Observation is the result of observing one checkout path at one moment.
type Observation struct {
	Status      Status    `json:"status"`
	Source      string    `json:"source"`
	CollectedAt time.Time `json:"collected_at"`
	DurationMS  int64     `json:"duration_ms"`
	// Reason explains every non-ok status. It is safe to display: it never
	// contains raw command output, remote URLs, or credentials.
	Reason string `json:"reason,omitempty"`
	// Repository is set only when Status is StatusOK.
	Repository *Repository `json:"repository,omitempty"`
}

// Repository is what Git reported for a checkout.
type Repository struct {
	// RepositoryRoot is the main work tree when the common Git directory is
	// a ".git" directory, otherwise the common Git directory itself (bare
	// repositories with linked worktrees).
	RepositoryRoot string `json:"repository_root"`
	// WorktreeRoot is the top level of the work tree containing the
	// registered path. For a Git worktree it is that worktree's directory.
	WorktreeRoot string `json:"worktree_root"`
	GitDir       string `json:"git_dir"`
	CommonDir    string `json:"common_dir"`
	// LinkedWorktree is true when the checkout is a worktree created with
	// "git worktree add" (its Git directory differs from the common one).
	LinkedWorktree bool `json:"linked_worktree"`
	// PathIsWorktreeRoot is false when the registered directory is a
	// subdirectory of the work tree rather than its top level.
	PathIsWorktreeRoot bool    `json:"path_is_worktree_root"`
	Head               Head    `json:"head"`
	Changes            Changes `json:"changes"`
	// Remotes lists every configured remote URL with credentials removed.
	Remotes []Remote `json:"remotes"`
}

// Head describes what HEAD points to.
type Head struct {
	// Branch is the current branch name, empty when detached.
	Branch string `json:"branch"`
	// Detached is true when HEAD points directly at a commit.
	Detached bool `json:"detached"`
	// Unborn is true when the branch has no commits yet; Commit is then empty.
	Unborn bool `json:"unborn"`
	// Commit is the full SHA of HEAD.
	Commit string `json:"commit"`
}

// Changes counts entries of "git status --porcelain=v2 --untracked-files=all".
//
//   - Tracked is the number of tracked paths with any staged, unstaged, or
//     unmerged change. A path that is both staged and modified again counts
//     once; a staged rename counts once.
//   - Staged and Unstaged count tracked paths with an index change
//     respectively a work-tree change; one path can appear in both.
//   - Conflicted counts unmerged paths (also included in Tracked).
//   - Untracked counts untracked files individually. Ignored files are never
//     counted; submodules follow Git's defaults.
type Changes struct {
	Tracked    int `json:"tracked"`
	Staged     int `json:"staged"`
	Unstaged   int `json:"unstaged"`
	Conflicted int `json:"conflicted"`
	Untracked  int `json:"untracked"`
}

// Remote is one configured remote URL after sanitisation.
type Remote struct {
	Name string `json:"name"`
	// Role is "fetch" for remote.<name>.url and "push" for remote.<name>.pushurl.
	Role string `json:"role"`
	// URL has credentials, query, and fragment removed. See Sanitize.
	URL string `json:"url"`
	// Host is the lowercase host name, empty for local paths.
	Host string `json:"host"`
	// Provider is "github", "gitlab", or "bitbucket" for the public hosts
	// of those services, "local" for paths and file URLs, and "unknown" for
	// every other host, including self-hosted instances. No API is queried.
	Provider string `json:"provider"`
	// Sanitized is true when something was removed from the configured URL.
	Sanitized bool `json:"sanitized"`
}

// Options configure a Collector. Zero values take the defaults below.
type Options struct {
	// GitPath is the git executable; empty means look it up in PATH.
	GitPath string
	// CommandTimeout bounds each git command.
	CommandTimeout time.Duration
	// CheckoutTimeout bounds all commands for one checkout together.
	CheckoutTimeout time.Duration
	// Concurrency bounds how many checkouts are observed at once.
	Concurrency int
	// MaxOutputBytes bounds the stdout of one git command; larger output
	// fails the observation instead of being truncated silently.
	MaxOutputBytes int64
	Logger         *slog.Logger
}

// Default limits.
const (
	DefaultCommandTimeout  = 10 * time.Second
	DefaultCheckoutTimeout = 20 * time.Second
	DefaultConcurrency     = 4
	DefaultMaxOutputBytes  = 8 << 20
)

// Collector observes checkouts. It is safe for concurrent use.
type Collector struct {
	opts    Options
	gitPath string
	lookErr error
}

// NewCollector resolves the git executable once and returns a collector.
// A missing executable is not an error here; observations then report
// StatusUnavailable.
func NewCollector(opts Options) *Collector {
	if opts.CommandTimeout <= 0 {
		opts.CommandTimeout = DefaultCommandTimeout
	}
	if opts.CheckoutTimeout <= 0 {
		opts.CheckoutTimeout = DefaultCheckoutTimeout
	}
	if opts.Concurrency <= 0 {
		opts.Concurrency = DefaultConcurrency
	}
	if opts.MaxOutputBytes <= 0 {
		opts.MaxOutputBytes = DefaultMaxOutputBytes
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	}
	c := &Collector{opts: opts}
	path := opts.GitPath
	if path == "" {
		path = "git"
	}
	resolved, err := exec.LookPath(path)
	if err != nil {
		c.lookErr = err
	} else {
		c.gitPath = resolved
	}
	return c
}

// CollectAll observes every path with bounded concurrency and returns the
// observations keyed by path. Failures are recorded per path; the map always
// has one entry per distinct input path.
func (c *Collector) CollectAll(ctx context.Context, paths []string) map[string]Observation {
	out := make(map[string]Observation, len(paths))
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, c.opts.Concurrency)
	seen := make(map[string]bool, len(paths))
	for _, p := range paths {
		if seen[p] {
			continue
		}
		seen[p] = true
		wg.Add(1)
		go func(p string) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				mu.Lock()
				out[p] = c.cancelled(time.Now(), ctx)
				mu.Unlock()
				return
			}
			obs := c.Observe(ctx, p)
			mu.Lock()
			out[p] = obs
			mu.Unlock()
		}(p)
	}
	wg.Wait()
	return out
}

// Observe runs the read-only Git commands for one directory.
func (c *Collector) Observe(ctx context.Context, path string) Observation {
	start := time.Now()
	obs := Observation{Status: StatusOK, Source: Source, CollectedAt: start.UTC().Truncate(time.Second)}
	finish := func(o Observation) Observation {
		o.DurationMS = time.Since(start).Milliseconds()
		return o
	}
	fail := func(status Status, reason string) Observation {
		obs.Status, obs.Reason, obs.Repository = status, reason, nil
		return finish(obs)
	}

	info, err := os.Stat(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return fail(StatusMissing, "directory does not exist")
	case err != nil:
		return fail(StatusError, "directory cannot be inspected")
	case !info.IsDir():
		return fail(StatusMissing, "path is not a directory")
	}
	if c.lookErr != nil {
		return fail(StatusUnavailable, "git executable not found")
	}
	if ctx.Err() != nil {
		return finish(c.cancelled(start, ctx))
	}

	ctx, cancel := context.WithTimeout(ctx, c.opts.CheckoutTimeout)
	defer cancel()

	repo, rerr := c.layout(ctx, path)
	if rerr != nil {
		return finish(c.failure(obs, rerr))
	}
	if err := c.status(ctx, path, repo); err != nil {
		return finish(c.failure(obs, err))
	}
	remotes, err := c.remotes(ctx, path)
	if err != nil {
		return finish(c.failure(obs, err))
	}
	repo.Remotes = remotes
	obs.Repository = repo
	return finish(obs)
}

func (c *Collector) cancelled(start time.Time, ctx context.Context) Observation {
	o := Observation{Source: Source, CollectedAt: start.UTC().Truncate(time.Second)}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		o.Status, o.Reason = StatusTimeout, "collection deadline exceeded before git ran"
	} else {
		o.Status, o.Reason = StatusError, "collection cancelled"
	}
	return o
}

// failure maps a command error to a status and a safe reason.
func (c *Collector) failure(obs Observation, err error) Observation {
	var ce *commandError
	obs.Repository = nil
	switch {
	case errors.As(err, &ce):
		obs.Status, obs.Reason = ce.status, ce.reason
		if ce.stderr != "" {
			c.opts.Logger.Warn("git command failed", "subcommand", ce.subcommand, "status", string(ce.status), "exit", ce.exitCode, "stderr", ce.stderr)
		}
	default:
		obs.Status, obs.Reason = StatusError, "git output could not be interpreted"
		c.opts.Logger.Warn("git output could not be interpreted", "err", err.Error())
	}
	return obs
}

func (c *Collector) layout(ctx context.Context, path string) (*Repository, error) {
	out, err := c.run(ctx, path, "rev-parse", "--path-format=absolute", "--show-toplevel", "--git-dir", "--git-common-dir")
	if err != nil {
		return nil, err
	}
	lines := splitLines(out)
	if len(lines) != 3 {
		return nil, fmt.Errorf("rev-parse: expected 3 lines, got %d", len(lines))
	}
	repo := &Repository{WorktreeRoot: lines[0], GitDir: lines[1], CommonDir: lines[2]}
	repo.LinkedWorktree = repo.GitDir != repo.CommonDir
	repo.RepositoryRoot = repositoryRoot(repo.CommonDir)
	repo.PathIsWorktreeRoot = sameDir(path, repo.WorktreeRoot)
	return repo, nil
}

func (c *Collector) status(ctx context.Context, path string, repo *Repository) error {
	out, err := c.run(ctx, path, "status", "--porcelain=v2", "-z", "--branch", "--untracked-files=all")
	if err != nil {
		return err
	}
	head, changes, err := parseStatusV2(out)
	if err != nil {
		return err
	}
	repo.Head, repo.Changes = head, changes
	return nil
}

func (c *Collector) remotes(ctx context.Context, path string) ([]Remote, error) {
	out, err := c.run(ctx, path, "config", "-z", "--get-regexp", `^remote\..+\.(url|pushurl)$`)
	if err != nil {
		var ce *commandError
		// Exit status 1 means "no matching keys": a repository without remotes.
		if errors.As(err, &ce) && ce.exitCode == 1 && ce.status == StatusError {
			return []Remote{}, nil
		}
		return nil, err
	}
	return parseRemoteConfig(out), nil
}
