package httpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Ryttis/devatlas/internal/collect/git"
	"github.com/Ryttis/devatlas/internal/registry"
)

func gitSetup(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@x.invalid", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@x.invalid", "GIT_CONFIG_GLOBAL=/dev/null")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func TestGitObservationsInAPIAndHTML(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	store := testStore(t)
	repo := filepath.Join(t.TempDir(), "repo")
	os.Mkdir(repo, 0o755)
	gitSetup(t, repo, "-c", "init.defaultBranch=feature/<b>x</b>", "init", "-q")
	os.WriteFile(filepath.Join(repo, "f.txt"), []byte("x"), 0o644)
	gitSetup(t, repo, "add", "f.txt")
	gitSetup(t, repo, "commit", "-q", "-m", "init")
	gitSetup(t, repo, "remote", "add", "origin", "https://user:s3cret@gitlab.com/g/r.git")
	os.WriteFile(filepath.Join(repo, "new.txt"), []byte("y"), 0o644)
	plain := t.TempDir()

	err := store.Update(func(reg *registry.Registry) error {
		if _, err := reg.Add(registry.AddRequest{ProjectID: "p", Name: "P", Path: repo}); err != nil {
			return err
		}
		_, err := reg.Add(registry.AddRequest{ProjectID: "p", Path: plain})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	base, stop := startTestServerWith(t, store)
	defer func() {
		if err := stop(); err != nil {
			t.Errorf("stop: %v", err)
		}
	}()

	code, body, _ := getBody(t, base+"/api/projects")
	if code != http.StatusOK {
		t.Fatalf("status %d: %s", code, body)
	}
	if strings.Contains(body, "s3cret") {
		t.Fatal("credential leaked into the API body")
	}
	var resp ProjectsResponse
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Collection.Mode != CollectionModeCurrentRequest || resp.Collection.Note == "" {
		t.Errorf("collection note: %+v", resp.Collection)
	}
	byPath := map[string]CheckoutDTO{}
	for _, c := range resp.Projects[0].Checkouts {
		byPath[c.Path] = c
	}
	r := byPath[mustCanonical(t, repo)]
	if r.Git.Status != git.StatusOK || r.Git.Source != git.Source || r.Git.CollectedAt.IsZero() || r.Git.Repository == nil {
		t.Fatalf("repo observation: %+v", r.Git)
	}
	if r.Git.Repository.Head.Branch != "feature/<b>x</b>" || len(r.Git.Repository.Head.Commit) != 40 || r.Git.Repository.Changes.Untracked != 1 {
		t.Errorf("repo details: %+v", r.Git.Repository)
	}
	if rem := r.Git.Repository.Remotes; len(rem) != 1 || rem[0].URL != "https://gitlab.com/g/r.git" || rem[0].Provider != "gitlab" || !rem[0].Sanitized {
		t.Errorf("remotes: %+v", rem)
	}
	if r.Runtime.Status != StatusUnknown {
		t.Errorf("runtime must stay unknown after a successful git observation: %+v", r.Runtime)
	}
	p := byPath[mustCanonical(t, plain)]
	if p.Git.Status != git.StatusNotRepository || p.Git.Repository != nil || p.Git.Reason == "" {
		t.Errorf("plain dir observation: %+v", p.Git)
	}

	code, html, _ := getBody(t, base+"/projects")
	if code != http.StatusOK {
		t.Fatalf("home status %d", code)
	}
	if strings.Contains(html, "s3cret") || strings.Contains(html, "<b>x</b>") {
		t.Fatalf("home page leaked a credential or did not escape the branch name:\n%s", html)
	}
	for _, want := range []string{"feature/&lt;b&gt;x&lt;/b&gt;", "1 uncommitted", "Not a Git repository", "Based on last fetched refs", "Unknown", mustCanonical(t, repo)} {
		if !strings.Contains(html, want) {
			t.Errorf("home page missing %q", want)
		}
	}
	code, html, _ = getBody(t, base+"/projects/p")
	if code != http.StatusOK {
		t.Fatalf("project status %d", code)
	}
	if strings.Contains(html, "s3cret") || strings.Contains(html, "<b>x</b>") {
		t.Fatalf("project page leaked a credential or did not escape the branch name:\n%s", html)
	}
	for _, want := range []string{"feature/&lt;b&gt;x&lt;/b&gt;", r.Git.Repository.Head.Commit[:12], "1 untracked", "gitlab.com", "[gitlab]", "not_git", "Not a Git repository", "Based on last fetched refs", "no upstream is configured for this branch", "FETCH_HEAD last updated"} {
		if !strings.Contains(html, want) {
			t.Errorf("project page missing %q", want)
		}
	}
}

// A clone that is ahead of and behind its remote shows the counts from the
// local remote-tracking refs, on the dashboard and on the project page.
func TestAheadBehindInAPIAndHTML(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	root := t.TempDir()
	origin := filepath.Join(root, "origin")
	clone := filepath.Join(root, "clone")
	os.Mkdir(origin, 0o755)
	gitSetup(t, origin, "-c", "init.defaultBranch=main", "init", "-q")
	commit := func(dir, name string) {
		os.WriteFile(filepath.Join(dir, name), []byte(name), 0o644)
		gitSetup(t, dir, "add", name)
		gitSetup(t, dir, "commit", "-q", "-m", name)
	}
	commit(origin, "a")
	gitSetup(t, root, "clone", "-q", origin, clone)
	commit(origin, "b")
	commit(origin, "c")
	gitSetup(t, clone, "fetch", "-q")
	gitSetup(t, clone, "checkout", "-q", "-b", "work")
	commit(clone, "d")

	store := testStore(t)
	if err := store.Update(func(reg *registry.Registry) error {
		_, err := reg.Add(registry.AddRequest{ProjectID: "p", Name: "P", Path: clone})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	base, stop := startTestServerWith(t, store)
	defer func() { _ = stop() }()

	_, body, _ := getBody(t, base+"/api/projects")
	var resp ProjectsResponse
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatal(err)
	}
	tr := resp.Projects[0].Checkouts[0].Git.Repository.Tracking
	if tr.Basis != git.TrackingBasis || tr.DefaultBranch == nil || tr.DefaultBranch.Ref != "origin/main" || tr.DefaultBranch.Source != git.DefaultFromRemoteHead {
		t.Fatalf("default branch: %+v %+v", tr, tr.DefaultBranch)
	}
	if d := tr.Default; !d.Known || d.Ahead != 1 || d.Behind != 2 || d.Ref != "origin/main" {
		t.Errorf("default divergence: %+v", d)
	}
	if tr.Upstream.Known || tr.Upstream.Reason == "" {
		t.Errorf("a branch without upstream must be unknown: %+v", tr.Upstream)
	}
	if tr.FetchHeadUpdatedAt == nil || time.Since(*tr.FetchHeadUpdatedAt) > time.Hour {
		t.Errorf("last fetch: %v", tr.FetchHeadUpdatedAt)
	}
	_, home, _ := getBody(t, base+"/")
	for _, want := range []string{"<code>work</code>", "clean", "1 ahead, 2 behind <code>origin/main</code>", "Based on last fetched refs"} {
		if !strings.Contains(home, want) {
			t.Errorf("home page missing %q", want)
		}
	}
	_, card, _ := getBody(t, base+"/projects/p")
	for _, want := range []string{"1 ahead, 2 behind <code>origin/main</code>", "(from <code>origin/HEAD</code>)", "no upstream is configured for this branch", "FETCH_HEAD", "Based on last fetched refs"} {
		if !strings.Contains(card, want) {
			t.Errorf("project page missing %q", want)
		}
	}
}

// blockingObserver waits until the request context is cancelled, like a
// git command that never returns; it reports whether cancellation arrived.
type blockingObserver struct {
	entered  chan struct{}
	released chan struct{}
}

func (b *blockingObserver) CollectAll(ctx context.Context, paths []string) map[string]git.Observation {
	close(b.entered)
	<-ctx.Done()
	close(b.released)
	out := map[string]git.Observation{}
	for _, p := range paths {
		out[p] = git.Observation{Status: git.StatusError, Source: git.Source, Reason: "collection cancelled"}
	}
	return out
}

func TestClientDisconnectCancelsGitCollection(t *testing.T) {
	store := testStore(t)
	dir := t.TempDir()
	if err := store.Update(func(reg *registry.Registry) error {
		_, err := reg.Add(registry.AddRequest{ProjectID: "p", Name: "P", Path: dir})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig()
	cfg.Port = 0
	cfg.ShutdownTimeout = 5 * time.Second
	srv, err := New(cfg, store, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	obs := &blockingObserver{entered: make(chan struct{}), released: make(chan struct{})}
	srv.SetGitObserver(obs)
	ln, err := srv.Listen()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx, ln) }()

	client := &http.Client{Timeout: 300 * time.Millisecond, Transport: &http.Transport{DisableKeepAlives: true}}
	if _, err := client.Get("http://" + ln.Addr().String() + "/api/projects"); err == nil {
		t.Fatal("expected the client to time out while collection blocks")
	}
	select {
	case <-obs.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("collection never started")
	}
	select {
	case <-obs.released: // the request context was cancelled when the client went away
	case <-time.After(5 * time.Second):
		t.Fatal("client disconnect did not cancel git collection")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("serve: %v", err)
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
