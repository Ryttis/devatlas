package git

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestParseUpstream(t *testing.T) {
	status := func(lines ...string) []byte {
		out := ""
		for _, l := range lines {
			out += l + "\x00"
		}
		return []byte(out)
	}
	for name, tc := range map[string]struct {
		out  []byte
		head Head
		want Divergence
	}{
		"ahead and behind": {status("# branch.head main", "# branch.upstream origin/main", "# branch.ab +3 -12", "? x"), Head{Branch: "main"},
			Divergence{Ref: "origin/main", Known: true, Ahead: 3, Behind: 12}},
		"upstream gone": {status("# branch.head main", "# branch.upstream origin/main"), Head{Branch: "main"},
			Divergence{Ref: "origin/main", Reason: "upstream ref is not present locally"}},
		"no upstream": {status("# branch.head main"), Head{Branch: "main"},
			Divergence{Reason: "no upstream is configured for this branch"}},
		"detached": {status("# branch.head (detached)"), Head{Detached: true},
			Divergence{Reason: "detached HEAD has no upstream"}},
		"garbage counts": {status("# branch.upstream origin/main", "# branch.ab x y"), Head{Branch: "main"},
			Divergence{Ref: "origin/main", Reason: "ahead/behind counts could not be interpreted"}},
		// A file name must never be mistaken for a header.
		"header-like path": {status("# branch.head main", "? # branch.upstream evil", "# branch.upstream late"), Head{Branch: "main"},
			Divergence{Reason: "no upstream is configured for this branch"}},
	} {
		if got := parseUpstream(tc.out, tc.head); got != tc.want {
			t.Errorf("%s:\n got %+v\nwant %+v", name, got, tc.want)
		}
	}
}

func TestDefaultRemote(t *testing.T) {
	if got := defaultRemote([]Remote{{Name: "upstream"}, {Name: "origin"}, {Name: "origin", Role: "push"}}); got != "origin" {
		t.Errorf("origin preferred, got %q", got)
	}
	if got := defaultRemote([]Remote{{Name: "gitlab"}, {Name: "gitlab", Role: "push"}}); got != "gitlab" {
		t.Errorf("single remote, got %q", got)
	}
	if got := defaultRemote([]Remote{{Name: "a"}, {Name: "b"}}); got != "" {
		t.Errorf("ambiguous remotes, got %q", got)
	}
}

func TestTrackingAgainstLocalRefs(t *testing.T) {
	requireGit(t)
	c, _ := newCollector(t, Options{})

	// No remote at all: everything is unknown, the observation still succeeds.
	origin := initRepo(t)
	write(t, filepath.Join(origin, "a.txt"), "a")
	commitAll(t, origin, "a")
	tr := observeOK(t, c, origin).Tracking
	if tr.Basis != TrackingBasis || tr.Upstream.Known || tr.Default.Known || tr.DefaultBranch != nil || tr.FetchHeadUpdatedAt != nil {
		t.Fatalf("repository without remotes: %+v", tr)
	}
	if tr.Default.Reason != "no remote is configured" {
		t.Errorf("reason: %q", tr.Default.Reason)
	}

	clone := filepath.Join(t.TempDir(), "clone")
	run(t, filepath.Dir(clone), "clone", "-q", origin, clone)
	write(t, filepath.Join(origin, "b.txt"), "b")
	commitAll(t, origin, "b")
	write(t, filepath.Join(clone, "c.txt"), "c")
	commitAll(t, clone, "c")
	write(t, filepath.Join(clone, "d.txt"), "d")
	commitAll(t, clone, "d")

	// Before a fetch the clone cannot know about the new remote commit.
	tr = observeOK(t, c, clone).Tracking
	if want := (Divergence{Ref: "origin/main", Known: true, Ahead: 2}); tr.Upstream != want || tr.Default != want {
		t.Fatalf("before fetch: upstream %+v default %+v", tr.Upstream, tr.Default)
	}
	if tr.FetchHeadUpdatedAt != nil {
		t.Errorf("a clone that never fetched has no fetch time: %v", tr.FetchHeadUpdatedAt)
	}
	headBefore := run(t, clone, "rev-parse", "HEAD", "origin/main")

	run(t, clone, "fetch", "-q")
	tr = observeOK(t, c, clone).Tracking
	if want := (Divergence{Ref: "origin/main", Known: true, Ahead: 2, Behind: 1}); tr.Upstream != want || tr.Default != want {
		t.Fatalf("after fetch: upstream %+v default %+v", tr.Upstream, tr.Default)
	}
	if db := tr.DefaultBranch; db == nil || db.Remote != "origin" || db.Branch != "main" || db.Ref != "origin/main" || db.Source != DefaultFromRemoteHead {
		t.Errorf("default branch: %+v", db)
	}
	if tr.FetchHeadUpdatedAt == nil || time.Since(*tr.FetchHeadUpdatedAt) > time.Hour {
		t.Errorf("last fetch: %v", tr.FetchHeadUpdatedAt)
	}
	// Observing fetched nothing and moved nothing.
	if after := run(t, clone, "rev-parse", "HEAD", "origin/main"); after == headBefore {
		t.Fatal("test setup: fetch did not move origin/main")
	}
	stable := run(t, clone, "rev-parse", "HEAD", "origin/main")
	observeOK(t, c, clone)
	if again := run(t, clone, "rev-parse", "HEAD", "origin/main"); again != stable {
		t.Error("observation changed refs")
	}

	// A topic branch without upstream still compares with the default branch.
	run(t, clone, "checkout", "-q", "-b", "topic")
	tr = observeOK(t, c, clone).Tracking
	if tr.Upstream.Known || tr.Upstream.Reason != "no upstream is configured for this branch" {
		t.Errorf("topic upstream: %+v", tr.Upstream)
	}
	if !tr.Default.Known || tr.Default.Ahead != 2 || tr.Default.Behind != 1 {
		t.Errorf("topic default: %+v", tr.Default)
	}

	// Without origin/HEAD the conventional branch name is used and marked.
	run(t, clone, "symbolic-ref", "-d", "refs/remotes/origin/HEAD")
	tr = observeOK(t, c, clone).Tracking
	if db := tr.DefaultBranch; db == nil || db.Ref != "origin/main" || db.Source != DefaultFromConventionalName || !tr.Default.Known {
		t.Errorf("conventional default: %+v %+v", db, tr.Default)
	}

	// No remote-tracking refs at all: unknown, with a reason.
	run(t, clone, "update-ref", "-d", "refs/remotes/origin/main")
	tr = observeOK(t, c, clone).Tracking
	if tr.Default.Known || tr.DefaultBranch != nil || tr.Default.Reason == "" {
		t.Errorf("missing refs: %+v", tr)
	}

	// An empty FETCH_HEAD (a fetch that failed) is not a successful fetch.
	if err := os.WriteFile(filepath.Join(clone, ".git", "FETCH_HEAD"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if tr = observeOK(t, c, clone).Tracking; tr.FetchHeadUpdatedAt != nil {
		t.Errorf("empty FETCH_HEAD counted as a fetch: %v", tr.FetchHeadUpdatedAt)
	}
}
