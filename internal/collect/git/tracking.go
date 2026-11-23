package git

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// TrackingBasis is how every comparison in Tracking was obtained. Nothing is
// fetched: a comparison is only as current as the last "git fetch" somebody
// ran in the repository.
const TrackingBasis = "last_fetched_refs"

// Default branch sources.
const (
	// DefaultFromRemoteHead means refs/remotes/<remote>/HEAD names the branch.
	DefaultFromRemoteHead = "remote_head"
	// DefaultFromConventionalName means <remote>/HEAD is not set locally and
	// the branch was picked because <remote>/main or <remote>/master exists.
	DefaultFromConventionalName = "conventional_name"
)

// Tracking compares HEAD with remote-tracking refs that already exist in the
// local repository. It never contacts a remote.
type Tracking struct {
	// Basis is always TrackingBasis.
	Basis string `json:"basis"`
	// Upstream compares HEAD with the upstream of the current branch.
	Upstream Divergence `json:"upstream"`
	// DefaultBranch is nil when no default branch could be determined.
	DefaultBranch *DefaultBranch `json:"default_branch,omitempty"`
	// Default compares HEAD with the remote default branch.
	Default Divergence `json:"default"`
	// FetchHeadUpdatedAt is the modification time of a non-empty FETCH_HEAD,
	// the file Git writes during a fetch or pull. It is evidence that a
	// fetch wrote refs at that time, not proof that the fetch succeeded.
	// Nil when there is no such file.
	FetchHeadUpdatedAt *time.Time `json:"fetch_head_updated_at,omitempty"`
}

// Divergence is the number of commits HEAD is ahead of and behind Ref. When
// Known is false the counts are meaningless and Reason says why.
type Divergence struct {
	// Ref is the short remote-tracking ref, for example "origin/main".
	Ref    string `json:"ref"`
	Known  bool   `json:"known"`
	Ahead  int    `json:"ahead"`
	Behind int    `json:"behind"`
	Reason string `json:"reason,omitempty"`
}

// DefaultBranch is the default branch of a remote as recorded locally.
type DefaultBranch struct {
	Remote string `json:"remote"`
	Branch string `json:"branch"`
	// Ref is "<remote>/<branch>".
	Ref    string `json:"ref"`
	Source string `json:"source"`
}

func unknown(ref, reason string) Divergence {
	return Divergence{Ref: ref, Reason: reason}
}

// parseUpstream reads the upstream headers of "git status --porcelain=v2
// --branch" output. Git prints branch.ab only when the upstream ref exists.
func parseUpstream(out []byte, head Head) Divergence {
	var ref, ab string
	for _, rec := range strings.Split(string(out), "\x00") {
		if !strings.HasPrefix(rec, "# ") {
			if rec != "" {
				break // headers come first
			}
			continue
		}
		if v, ok := strings.CutPrefix(rec, "# branch.upstream "); ok {
			ref = v
		}
		if v, ok := strings.CutPrefix(rec, "# branch.ab "); ok {
			ab = v
		}
	}
	switch {
	case head.Detached:
		return unknown("", "detached HEAD has no upstream")
	case ref == "":
		return unknown("", "no upstream is configured for this branch")
	case ab == "":
		return unknown(ref, "upstream ref is not present locally")
	}
	a, b, ok := strings.Cut(ab, " ")
	ahead, errA := strconv.Atoi(strings.TrimPrefix(a, "+"))
	behind, errB := strconv.Atoi(strings.TrimPrefix(b, "-"))
	if !ok || errA != nil || errB != nil || !strings.HasPrefix(a, "+") || !strings.HasPrefix(b, "-") {
		return unknown(ref, "ahead/behind counts could not be interpreted")
	}
	return Divergence{Ref: ref, Known: true, Ahead: ahead, Behind: behind}
}

// defaultRemote picks the remote whose default branch is used: "origin" when
// configured, otherwise the only remote. Several remotes without an "origin"
// are ambiguous and yield "".
func defaultRemote(remotes []Remote) string {
	names := map[string]bool{}
	for _, r := range remotes {
		names[r.Name] = true
	}
	if names["origin"] {
		return "origin"
	}
	if len(names) == 1 {
		for n := range names {
			return n
		}
	}
	return ""
}

// tracking fills the comparisons. Every step is optional: a failure leaves
// the affected value unknown and never fails the observation.
func (c *Collector) tracking(ctx context.Context, path string, repo *Repository, statusOut []byte) Tracking {
	t := Tracking{Basis: TrackingBasis, Upstream: parseUpstream(statusOut, repo.Head)}
	t.FetchHeadUpdatedAt = fetchHeadUpdated(repo.GitDir, repo.CommonDir)

	remote := defaultRemote(repo.Remotes)
	switch {
	case len(repo.Remotes) == 0:
		t.Default = unknown("", "no remote is configured")
		return t
	case remote == "":
		t.Default = unknown("", "several remotes and none is named origin")
		return t
	}
	prefix := "refs/remotes/" + remote + "/"
	var branch, source string
	if out, err := c.run(ctx, path, "symbolic-ref", "--quiet", prefix+"HEAD"); err == nil {
		if lines := splitLines(out); len(lines) == 1 {
			if b, ok := strings.CutPrefix(lines[0], prefix); ok && b != "" {
				branch, source = b, DefaultFromRemoteHead
			}
		}
	}
	if branch == "" && ctx.Err() == nil {
		if out, err := c.run(ctx, path, "for-each-ref", "--format=%(refname)", prefix+"main", prefix+"master"); err == nil {
			found := map[string]bool{}
			for _, l := range splitLines(out) {
				found[l] = true
			}
			for _, b := range []string{"main", "master"} {
				if found[prefix+b] {
					branch, source = b, DefaultFromConventionalName
					break
				}
			}
		}
	}
	if branch == "" {
		t.Default = unknown("", "no default branch ref for remote "+remote+" is present locally")
		return t
	}
	ref := remote + "/" + branch
	t.DefaultBranch = &DefaultBranch{Remote: remote, Branch: branch, Ref: ref, Source: source}
	if repo.Head.Unborn {
		t.Default = unknown(ref, "current branch has no commits")
		return t
	}
	out, err := c.run(ctx, path, "rev-list", "--left-right", "--count", "HEAD..."+prefix+branch, "--")
	if err != nil {
		t.Default = unknown(ref, "default branch ref is not present locally or could not be compared")
		return t
	}
	fields := strings.Fields(string(out))
	if len(fields) != 2 {
		t.Default = unknown(ref, "ahead/behind counts could not be interpreted")
		return t
	}
	ahead, errA := strconv.Atoi(fields[0])
	behind, errB := strconv.Atoi(fields[1])
	if errA != nil || errB != nil {
		t.Default = unknown(ref, "ahead/behind counts could not be interpreted")
		return t
	}
	t.Default = Divergence{Ref: ref, Known: true, Ahead: ahead, Behind: behind}
	return t
}

// fetchHeadUpdated returns the newest modification time of a non-empty
// FETCH_HEAD. Git truncates the file when a fetch starts, so an empty file
// is ignored; a non-empty one still does not establish that the fetch
// completed successfully.
func fetchHeadUpdated(dirs ...string) *time.Time {
	var newest *time.Time
	seen := map[string]bool{}
	for _, d := range dirs {
		if d == "" || seen[d] {
			continue
		}
		seen[d] = true
		info, err := os.Stat(filepath.Join(d, "FETCH_HEAD"))
		if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
			continue
		}
		mt := info.ModTime().UTC().Truncate(time.Second)
		if newest == nil || mt.After(*newest) {
			newest = &mt
		}
	}
	return newest
}
