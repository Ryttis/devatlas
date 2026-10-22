package git

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func requireGit(t *testing.T) string {
	t.Helper()
	p, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not available")
	}
	return p
}

// setupGitCommand supplies an isolated identity for test setup commands.
func setupGitCommand(dir string, args ...string) *exec.Cmd {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.invalid",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.invalid",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0",
	)
	return cmd
}

// run executes git in dir for test setup only.
func run(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := setupGitCommand(dir, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func initRepo(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "repo")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	run(t, dir, "-c", "init.defaultBranch=main", "init", "-q")
	return dir
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func commitAll(t *testing.T, dir, msg string) string {
	t.Helper()
	run(t, dir, "add", "-A")
	run(t, dir, "commit", "-q", "-m", msg)
	return run(t, dir, "rev-parse", "HEAD")
}

func newCollector(t *testing.T, opts Options) (*Collector, *bytes.Buffer) {
	t.Helper()
	var logs bytes.Buffer
	opts.Logger = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return NewCollector(opts), &logs
}

func observeOK(t *testing.T, c *Collector, dir string) *Repository {
	t.Helper()
	obs := c.Observe(context.Background(), dir)
	if obs.Status != StatusOK {
		t.Fatalf("status = %s (%s), want ok", obs.Status, obs.Reason)
	}
	if obs.Source != Source || obs.CollectedAt.IsZero() || obs.Repository == nil || obs.Reason != "" {
		t.Fatalf("malformed ok observation: %+v", obs)
	}
	return obs.Repository
}

func TestUnbornRepository(t *testing.T) {
	requireGit(t)
	dir := initRepo(t)
	c, _ := newCollector(t, Options{})
	repo := observeOK(t, c, dir)
	if !repo.Head.Unborn || repo.Head.Commit != "" || repo.Head.Branch != "main" || repo.Head.Detached {
		t.Errorf("unborn head: %+v", repo.Head)
	}
	if repo.Changes != (Changes{}) {
		t.Errorf("changes on empty repo: %+v", repo.Changes)
	}
	write(t, filepath.Join(dir, "new.txt"), "x")
	repo = observeOK(t, c, dir)
	if repo.Changes.Untracked != 1 || repo.Changes.Tracked != 0 {
		t.Errorf("untracked in unborn repo: %+v", repo.Changes)
	}
	if len(repo.Remotes) != 0 {
		t.Errorf("remotes = %+v, want none", repo.Remotes)
	}
}

func TestCleanAndDirtyStates(t *testing.T) {
	requireGit(t)
	dir := initRepo(t)
	write(t, filepath.Join(dir, "a.txt"), "a")
	write(t, filepath.Join(dir, "b.txt"), "b")
	write(t, filepath.Join(dir, "c.txt"), "c")
	write(t, filepath.Join(dir, "old.txt"), "rename me, please keep this content identical")
	sha := commitAll(t, dir, "init")
	c, _ := newCollector(t, Options{})

	repo := observeOK(t, c, dir)
	if repo.Head.Commit != sha || len(sha) != 40 || repo.Head.Branch != "main" || repo.Head.Detached || repo.Head.Unborn {
		t.Errorf("clean head: %+v", repo.Head)
	}
	if repo.Changes != (Changes{}) {
		t.Errorf("clean repo changes: %+v", repo.Changes)
	}
	if !repo.PathIsWorktreeRoot || repo.LinkedWorktree || repo.WorktreeRoot != mustReal(t, dir) || repo.RepositoryRoot != mustReal(t, dir) {
		t.Errorf("layout: %+v", repo)
	}

	// staged only
	write(t, filepath.Join(dir, "a.txt"), "a2")
	run(t, dir, "add", "a.txt")
	// unstaged only
	write(t, filepath.Join(dir, "b.txt"), "b2")
	// staged and then modified again: one tracked path, counted in both
	write(t, filepath.Join(dir, "c.txt"), "c2")
	run(t, dir, "add", "c.txt")
	write(t, filepath.Join(dir, "c.txt"), "c3")
	// staged rename: one tracked path
	run(t, dir, "mv", "old.txt", "renamed.txt")
	// untracked files with awkward names, in a subdirectory too
	for _, name := range []string{"space name.txt", "quote\"d.txt", "new\nline.txt", "ünïcode.txt", "sub/dir/deep.txt"} {
		write(t, filepath.Join(dir, name), "u")
	}
	// ignored file is never counted
	write(t, filepath.Join(dir, ".gitignore"), "ignored.log\n")
	write(t, filepath.Join(dir, "ignored.log"), "zzz")

	repo = observeOK(t, c, dir)
	want := Changes{Tracked: 4, Staged: 3, Unstaged: 2, Conflicted: 0, Untracked: 6} // 5 files + .gitignore
	if repo.Changes != want {
		t.Errorf("dirty changes = %+v, want %+v", repo.Changes, want)
	}
}

func TestConflictedAndDetached(t *testing.T) {
	requireGit(t)
	dir := initRepo(t)
	write(t, filepath.Join(dir, "f.txt"), "base\n")
	base := commitAll(t, dir, "base")
	run(t, dir, "checkout", "-q", "-b", "other")
	write(t, filepath.Join(dir, "f.txt"), "other\n")
	commitAll(t, dir, "other")
	run(t, dir, "checkout", "-q", "main")
	write(t, filepath.Join(dir, "f.txt"), "main\n")
	commitAll(t, dir, "main")
	out, err := setupGitCommand(dir, "merge", "other").CombinedOutput()
	if exitErr, ok := err.(*exec.ExitError); !ok || exitErr.ExitCode() != 1 {
		t.Fatalf("merge should conflict with exit 1: %v\n%s", err, out)
	}
	t.Logf("merge output:\n%s", out)
	// A failed command alone does not prove the conflict fixture was created.
	unmerged := strings.Split(run(t, dir, "ls-files", "--unmerged"), "\n")
	if len(unmerged) != 3 {
		t.Fatalf("want three unmerged index stages for f.txt, got %q\n%s", unmerged, out)
	}
	for i, entry := range unmerged {
		if !strings.HasSuffix(entry, " "+string(rune('1'+i))+"\tf.txt") {
			t.Fatalf("unexpected unmerged index stage: %q\n%s", entry, out)
		}
	}
	c, _ := newCollector(t, Options{})
	repo := observeOK(t, c, dir)
	if repo.Changes.Conflicted != 1 || repo.Changes.Tracked != 1 {
		t.Errorf("conflict changes: %+v", repo.Changes)
	}
	run(t, dir, "merge", "--abort")

	run(t, dir, "checkout", "-q", "--detach", base)
	repo = observeOK(t, c, dir)
	if !repo.Head.Detached || repo.Head.Branch != "" || repo.Head.Commit != base {
		t.Errorf("detached head: %+v", repo.Head)
	}
}

func TestWorktreesAndSubdirectory(t *testing.T) {
	requireGit(t)
	dir := initRepo(t)
	write(t, filepath.Join(dir, "f.txt"), "x")
	write(t, filepath.Join(dir, "sub", "g.txt"), "y")
	sha := commitAll(t, dir, "init")
	wt := filepath.Join(filepath.Dir(dir), "wt-feature")
	run(t, dir, "worktree", "add", "-q", "-b", "feature", wt)
	write(t, filepath.Join(wt, "f.txt"), "changed")

	c, _ := newCollector(t, Options{})
	main := observeOK(t, c, dir)
	feat := observeOK(t, c, wt)
	sub := observeOK(t, c, filepath.Join(dir, "sub"))

	if main.Head.Branch != "main" || feat.Head.Branch != "feature" || main.Head.Commit != sha || feat.Head.Commit != sha {
		t.Errorf("branches: main=%+v feature=%+v", main.Head, feat.Head)
	}
	if main.LinkedWorktree || !feat.LinkedWorktree {
		t.Errorf("linked flags: main=%v feature=%v", main.LinkedWorktree, feat.LinkedWorktree)
	}
	if main.RepositoryRoot != feat.RepositoryRoot || main.CommonDir != feat.CommonDir {
		t.Errorf("worktrees must share repository root and common dir: %+v vs %+v", main, feat)
	}
	if feat.WorktreeRoot != mustReal(t, wt) || main.WorktreeRoot != mustReal(t, dir) || feat.GitDir == main.GitDir {
		t.Errorf("worktree roots/git dirs: %+v vs %+v", main, feat)
	}
	if main.Changes.Unstaged != 0 || feat.Changes.Unstaged != 1 {
		t.Errorf("changes are per worktree: main=%+v feature=%+v", main.Changes, feat.Changes)
	}
	if sub.PathIsWorktreeRoot || sub.WorktreeRoot != mustReal(t, dir) {
		t.Errorf("subdirectory observation: %+v", sub)
	}

	// CollectAll keys by path and isolates failures.
	notGit := t.TempDir()
	missing := filepath.Join(notGit, "nope")
	all := c.CollectAll(context.Background(), []string{dir, wt, notGit, missing, dir})
	if len(all) != 4 {
		t.Fatalf("expected 4 observations, got %d", len(all))
	}
	if all[dir].Status != StatusOK || all[wt].Status != StatusOK {
		t.Errorf("good repos failed: %+v %+v", all[dir], all[wt])
	}
	if all[notGit].Status != StatusNotRepository || all[notGit].Reason != "not a Git repository" || all[notGit].Repository != nil {
		t.Errorf("not git: %+v", all[notGit])
	}
	if all[missing].Status != StatusMissing || all[missing].Repository != nil {
		t.Errorf("missing: %+v", all[missing])
	}
}

func TestObservationDoesNotModifyRepository(t *testing.T) {
	requireGit(t)
	dir := initRepo(t)
	write(t, filepath.Join(dir, "f.txt"), "x")
	commitAll(t, dir, "init")
	write(t, filepath.Join(dir, "f.txt"), "y")
	before := snapshot(t, dir)
	time.Sleep(20 * time.Millisecond)
	c, _ := newCollector(t, Options{})
	observeOK(t, c, dir)
	if after := snapshot(t, dir); after != before {
		t.Fatalf("repository changed by observation:\n%s\n---\n%s", before, after)
	}
}

// snapshot records git's own view of the work tree (read without optional
// locks, so the snapshot itself does not refresh the index) and the
// modification times of the index, HEAD, and config.
func snapshot(t *testing.T, dir string) string {
	t.Helper()
	var sb strings.Builder
	sb.WriteString(run(t, dir, "--no-optional-locks", "status", "--porcelain"))
	for _, f := range []string{"index", "HEAD", "config"} {
		info, err := os.Stat(filepath.Join(dir, ".git", f))
		if err != nil {
			t.Fatal(err)
		}
		sb.WriteString("\n" + f + " " + info.ModTime().String())
	}
	return sb.String()
}

func TestRemotesAreSanitizedEverywhere(t *testing.T) {
	requireGit(t)
	dir := initRepo(t)
	const token = "ghp_SECRETTOKEN123"
	const pw = "hunter2pass"
	run(t, dir, "remote", "add", "origin", "https://x-access-token:"+token+"@github.com/acme/app.git")
	run(t, dir, "remote", "set-url", "--push", "origin", "git@github.com:acme/app.git")
	run(t, dir, "remote", "add", "lab", "ssh://deploy:"+pw+"@gitlab.example.com:2222/grp/app.git")
	run(t, dir, "remote", "add", "bb", "git@bitbucket.org:team/app.git")
	run(t, dir, "remote", "add", "q", "https://gitlab.com/g/p.git?private_token="+token+"#frag")

	c, logs := newCollector(t, Options{})
	obs := c.Observe(context.Background(), dir)
	if obs.Status != StatusOK {
		t.Fatalf("status %s: %s", obs.Status, obs.Reason)
	}
	js, _ := json.Marshal(obs)
	for _, secret := range []string{token, pw, "x-access-token"} {
		if strings.Contains(string(js), secret) || strings.Contains(logs.String(), secret) {
			t.Fatalf("secret %q leaked:\n%s\n%s", secret, js, logs.String())
		}
	}
	got := map[string]Remote{}
	for _, r := range obs.Repository.Remotes {
		got[r.Name+"/"+r.Role] = r
	}
	expect := map[string]Remote{
		"origin/fetch": {URL: "https://github.com/acme/app.git", Host: "github.com", Provider: "github", Sanitized: true},
		"origin/push":  {URL: "git@github.com:acme/app.git", Host: "github.com", Provider: "github"},
		"lab/fetch":    {URL: "ssh://deploy@gitlab.example.com:2222/grp/app.git", Host: "gitlab.example.com", Provider: "unknown", Sanitized: true},
		"bb/fetch":     {URL: "git@bitbucket.org:team/app.git", Host: "bitbucket.org", Provider: "bitbucket"},
		"q/fetch":      {URL: "https://gitlab.com/g/p.git", Host: "gitlab.com", Provider: "gitlab", Sanitized: true},
	}
	if len(got) != len(expect) {
		t.Fatalf("remotes = %+v", got)
	}
	for k, want := range expect {
		g := got[k]
		name, role, _ := strings.Cut(k, "/")
		want.Name, want.Role = name, role
		if g != want {
			t.Errorf("%s: got %+v, want %+v", k, g, want)
		}
	}
}

func TestSanitizeTable(t *testing.T) {
	cases := []struct {
		in   string
		want Remote
	}{
		{"https://github.com/o/r.git", Remote{URL: "https://github.com/o/r.git", Host: "github.com", Provider: "github"}},
		{"https://user:pw@GitHub.com/o/r.git", Remote{URL: "https://GitHub.com/o/r.git", Host: "github.com", Provider: "github", Sanitized: true}},
		{"https://token@gitlab.com/o/r.git", Remote{URL: "https://gitlab.com/o/r.git", Host: "gitlab.com", Provider: "gitlab", Sanitized: true}},
		{"http://user@gitlab.corp.local/o/r.git", Remote{URL: "http://gitlab.corp.local/o/r.git", Host: "gitlab.corp.local", Provider: "unknown", Sanitized: true}},
		{"ssh://git@github.com/o/r.git", Remote{URL: "ssh://git@github.com/o/r.git", Host: "github.com", Provider: "github"}},
		{"ssh://git:pw@[::1]:2222/o/r.git", Remote{URL: "ssh://git@[::1]:2222/o/r.git", Host: "::1", Provider: "unknown", Sanitized: true}},
		{"git+ssh://git@altssh.gitlab.com:443/o/r.git", Remote{URL: "git+ssh://git@altssh.gitlab.com:443/o/r.git", Host: "altssh.gitlab.com", Provider: "gitlab"}},
		{"git@github.com:o/r.git", Remote{URL: "git@github.com:o/r.git", Host: "github.com", Provider: "github"}},
		{"git:pw@bitbucket.org:o/r.git", Remote{URL: "git@bitbucket.org:o/r.git", Host: "bitbucket.org", Provider: "bitbucket", Sanitized: true}},
		{"host.example.com:o/r.git", Remote{URL: "host.example.com:o/r.git", Host: "host.example.com", Provider: "unknown"}},
		{"https://bitbucket.org/o/r.git?x=1", Remote{URL: "https://bitbucket.org/o/r.git", Host: "bitbucket.org", Provider: "bitbucket", Sanitized: true}},
		{"git://github.com/o/r.git", Remote{URL: "git://github.com/o/r.git", Host: "github.com", Provider: "github"}},
		{"file:///srv/git/r.git", Remote{URL: "file:///srv/git/r.git", Provider: "local"}},
		{"/srv/git/r.git", Remote{URL: "/srv/git/r.git", Provider: "local"}},
		{"../sibling", Remote{URL: "../sibling", Provider: "local"}},
		{"C:\\repos\\r", Remote{URL: "C:\\repos\\r", Provider: "local"}},
		{"https://%zz@github.com/x", Remote{URL: "(unparseable remote URL, hidden)", Provider: "unknown", Sanitized: true}},
		{"", Remote{Provider: "unknown"}},
	}
	for _, tc := range cases {
		if got := Sanitize(tc.in); got != tc.want {
			t.Errorf("Sanitize(%q)\n got %+v\nwant %+v", tc.in, got, tc.want)
		}
	}
}

func TestParseStatusV2Records(t *testing.T) {
	out := strings.Join([]string{
		"# branch.oid 0123456789abcdef0123456789abcdef01234567",
		"# branch.head main",
		"# branch.upstream origin/main",
		"# branch.ab +1 -0",
		"1 M. N... 100644 100644 100644 aaaa bbbb staged.txt",
		"1 .M N... 100644 100644 100644 aaaa aaaa unstaged.txt",
		"1 MM N... 100644 100644 100644 aaaa bbbb both.txt",
		"2 R. N... 100644 100644 100644 aaaa aaaa R100 new name.txt", "old name.txt",
		"u UU N... 100644 100644 100644 100644 aaaa bbbb cccc conflict.txt",
		"? untracked one.txt",
		"? untracked\ntwo.txt",
		"! ignored.txt",
	}, "\x00") + "\x00"
	head, ch, err := parseStatusV2([]byte(out))
	if err != nil {
		t.Fatal(err)
	}
	if head.Branch != "main" || head.Commit != "0123456789abcdef0123456789abcdef01234567" || head.Detached || head.Unborn {
		t.Errorf("head: %+v", head)
	}
	want := Changes{Tracked: 5, Staged: 3, Unstaged: 2, Conflicted: 1, Untracked: 2}
	if ch != want {
		t.Errorf("changes = %+v, want %+v", ch, want)
	}
	if _, _, err := parseStatusV2([]byte("garbage\x00")); err == nil {
		t.Error("expected error for unknown record")
	}
	if _, _, err := parseStatusV2(nil); err == nil {
		t.Error("expected error without branch.oid")
	}
}

// fakeGit writes an executable script used in place of git.
func fakeGit(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "git")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestUnavailableGit(t *testing.T) {
	c, _ := newCollector(t, Options{GitPath: filepath.Join(t.TempDir(), "no-such-git")})
	obs := c.Observe(context.Background(), t.TempDir())
	if obs.Status != StatusUnavailable || obs.Repository != nil || obs.Reason == "" {
		t.Fatalf("%+v", obs)
	}
	// Missing directories are reported before git is consulted.
	if obs := c.Observe(context.Background(), filepath.Join(t.TempDir(), "x")); obs.Status != StatusMissing {
		t.Fatalf("%+v", obs)
	}
	f := filepath.Join(t.TempDir(), "file")
	os.WriteFile(f, nil, 0o600)
	if obs := c.Observe(context.Background(), f); obs.Status != StatusMissing || obs.Reason != "path is not a directory" {
		t.Fatalf("%+v", obs)
	}
}

func TestTimeoutAndCancellation(t *testing.T) {
	slow := fakeGit(t, "sleep 5")
	c, _ := newCollector(t, Options{GitPath: slow, CommandTimeout: 200 * time.Millisecond})
	start := time.Now()
	obs := c.Observe(context.Background(), t.TempDir())
	if obs.Status != StatusTimeout || !strings.Contains(obs.Reason, "deadline") || obs.Repository != nil {
		t.Fatalf("%+v", obs)
	}
	if time.Since(start) > 1500*time.Millisecond {
		t.Fatalf("timeout took %s; the process group was not killed", time.Since(start))
	}

	// Caller cancellation reaches the running command.
	c2, _ := newCollector(t, Options{GitPath: slow})
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(100 * time.Millisecond); cancel() }()
	start = time.Now()
	obs = c2.Observe(ctx, t.TempDir())
	if obs.Status != StatusError || obs.Reason != "collection cancelled" || time.Since(start) > 1500*time.Millisecond {
		t.Fatalf("%+v after %s", obs, time.Since(start))
	}

	// A cancelled context makes CollectAll return every path, none hung.
	ctx, cancel = context.WithCancel(context.Background())
	cancel()
	all := c2.CollectAll(ctx, []string{t.TempDir(), t.TempDir()})
	for p, o := range all {
		if o.Status != StatusError {
			t.Errorf("%s: %+v", p, o)
		}
	}
}

func TestFailureIsolationAndOutputLimit(t *testing.T) {
	requireGit(t)
	good := initRepo(t)
	write(t, filepath.Join(good, "f"), "x")
	commitAll(t, good, "init")

	// A git that fails for one directory only.
	bad := t.TempDir()
	real, _ := exec.LookPath("git")
	script := fakeGit(t, `case "$2" in "`+bad+`") echo "fatal: boom" >&2; exit 3;; esac; exec "`+real+`" "$@"`)
	c, logs := newCollector(t, Options{GitPath: script, Concurrency: 1})
	all := c.CollectAll(context.Background(), []string{good, bad})
	if all[good].Status != StatusOK {
		t.Errorf("good: %+v", all[good])
	}
	if all[bad].Status != StatusError || all[bad].Reason != "git rev-parse exited with status 3" {
		t.Errorf("bad: %+v", all[bad])
	}
	if !strings.Contains(logs.String(), "boom") {
		t.Errorf("stderr should be logged, not shown: %s", logs.String())
	}

	// Output beyond the limit fails the observation instead of truncating.
	noisy := fakeGit(t, `head -c 5000 /dev/zero | tr '\0' 'x'`)
	c2, _ := newCollector(t, Options{GitPath: noisy, MaxOutputBytes: 1024})
	obs := c2.Observe(context.Background(), good)
	if obs.Status != StatusError || !strings.Contains(obs.Reason, "more than 1024 bytes") {
		t.Fatalf("%+v", obs)
	}

	// Classified failures carry safe reasons.
	dubious := fakeGit(t, `echo "fatal: detected dubious ownership in repository at '/x'" >&2; exit 128`)
	c3, _ := newCollector(t, Options{GitPath: dubious})
	if obs := c3.Observe(context.Background(), good); obs.Status != StatusError || !strings.Contains(obs.Reason, "safe.directory") {
		t.Fatalf("%+v", obs)
	}
}

func TestEnvironmentCannotRedirectGit(t *testing.T) {
	requireGit(t)
	a := initRepo(t)
	write(t, filepath.Join(a, "f"), "x")
	shaA := commitAll(t, a, "a")
	b := initRepo(t)
	write(t, filepath.Join(b, "g"), "y")
	commitAll(t, b, "b")
	t.Setenv("GIT_DIR", filepath.Join(b, ".git"))
	t.Setenv("GIT_WORK_TREE", b)
	c, _ := newCollector(t, Options{})
	repo := observeOK(t, c, a)
	if repo.Head.Commit != shaA || repo.WorktreeRoot != mustReal(t, a) {
		t.Fatalf("GIT_DIR redirected the observation: %+v", repo)
	}
}

func mustReal(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
