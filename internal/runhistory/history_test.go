//go:build linux

package runhistory

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

const testSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func put(t *testing.T, root, family, id, file string, v any) string {
	t.Helper()
	dir := filepath.Join(root, family, id)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, file)
	if err := os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}
func state(id, stage string) map[string]any {
	return map[string]any{"run_id": id, "stage": stage, "base": testSHA, "head": testSHA, "updated_at": "2026-11-12T12:00:00Z"}
}

func TestHistoryEvidenceAndWorkflowLinks(t *testing.T) {
	root := t.TempDir()
	s := Store{Dir: root}
	put(t, root, "agent-runs", "run-aaaaaa", "state.json", state("run-aaaaaa", "changes_requested"))
	put(t, root, "agent-runs", "run-aaaaaa", "review.json", map[string]any{"verdict": "changes_requested", "summary": "32 lines; maximum 30", "findings": []string{"[P2] docs/AGENT_SMOKE_TEST.md:30–32"}})
	put(t, root, "agent-runs", "run-aaaaaa", "tester.json", map[string]any{"verdict": "BLOCKED", "summary": "Review must pass first"})
	put(t, root, "workflows", "task-aaaaaa", "workflow.json", map[string]any{"status": "FIX_REQUIRED", "updated_at": "2026-11-12T13:00:00Z", "slice": 1, "run_id": "run-aaaaaa", "candidate": testSHA, "review": "changes_requested"})
	list := s.List(context.Background())
	if len(list.Entries) != 2 || list.Entries[0].ID != "task-aaaaaa" {
		t.Fatalf("list: %+v", list)
	}
	d, err := s.Get("task-aaaaaa")
	if err != nil || len(d.Entry.Attempts) != 1 || d.Entry.Attempts[0].RunID != "run-aaaaaa" {
		t.Fatalf("workflow: %+v %v", d, err)
	}
	d, err = s.Get("run-aaaaaa")
	if err != nil || d.Entry.GoChecks != "passed" || d.Reports[0].Verdict != "changes_requested" || len(d.Reports[0].Findings) != 1 || d.Reports[1].Verdict != "BLOCKED" {
		t.Fatalf("detail: %+v %v", d, err)
	}
	// Re-read each request and never trust a verdict contradicting terminal state.
	put(t, root, "agent-runs", "run-aaaaaa", "review.json", map[string]any{"verdict": "pass", "summary": "Unexpected pass", "findings": []string{}})
	d, _ = s.Get("run-aaaaaa")
	if !d.Reports[0].Unavailable || d.Reports[0].Summary != "" {
		t.Fatal("inconsistent review exposed")
	}
	for _, stage := range []string{"preparing", "coding", "verifying"} {
		put(t, root, "agent-runs", "run-aaaaaa", "state.json", state("run-aaaaaa", stage))
		d, _ = s.Get("run-aaaaaa")
		if d.Entry.GoChecks != "not_confirmed" || d.Reports[0].Summary != "" {
			t.Fatalf("premature evidence: %+v", d)
		}
	}
}

func TestRejectUnsafeAndMalformedFiles(t *testing.T) {
	for _, kind := range []string{"symlink-file", "symlink-dir", "hardlink", "fifo", "oversize", "malformed", "unknown-stage", "wrong-id"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			id := "run-aaaaaa"
			path := put(t, root, "agent-runs", id, "state.json", state(id, "pass"))
			outside := filepath.Join(t.TempDir(), "outside")
			b, _ := os.ReadFile(path)
			if err := os.WriteFile(outside, b, 0600); err != nil {
				t.Fatal(err)
			}
			var err error
			switch kind {
			case "symlink-file":
				os.Remove(path)
				err = os.Symlink(outside, path)
			case "symlink-dir":
				os.Remove(path)
				os.Remove(filepath.Dir(path))
				err = os.Symlink(filepath.Dir(outside), filepath.Dir(path))
			case "hardlink":
				os.Remove(path)
				err = os.Link(outside, path)
			case "fifo":
				os.Remove(path)
				err = syscall.Mkfifo(path, 0600)
			case "oversize":
				err = os.WriteFile(path, []byte(strings.Repeat(" ", 65537)), 0600)
			case "malformed":
				err = os.WriteFile(path, []byte("{"), 0600)
			case "unknown-stage":
				put(t, root, "agent-runs", id, "state.json", state(id, "imagined-success"))
			case "wrong-id":
				put(t, root, "agent-runs", id, "state.json", state("run-bbbbbb", "pass"))
			}
			if err != nil {
				t.Fatal(err)
			}
			s := Store{Dir: root}
			if _, err := s.Get(id); err == nil {
				t.Fatal("unsafe record accepted")
			}
			l := s.List(context.Background())
			if len(l.Entries) != 0 || l.Unavailable != 1 {
				t.Fatalf("missing warning: %+v", l)
			}
		})
	}
	s := Store{Dir: t.TempDir()}
	for _, id := range []string{"../outside", "run-aaaaaa/review.json", "task-../../x", "run-x"} {
		if _, err := s.Get(id); err == nil {
			t.Fatal("invalid id accepted")
		}
	}
}

func TestRedaction(t *testing.T) {
	raw := "docs/check.go:42 <script> password=secret123 token: \"abc def\" https://user:pass@example.test/path?key=foo Bearer abc.def.ghi /home/rytis/private/file ghp_abcdefghijklmnop"
	got := redact(raw)
	for _, secret := range []string{"secret123", "abc def", "example.test", "abc.def.ghi", "/home/rytis", "ghp_"} {
		if strings.Contains(got, secret) {
			t.Errorf("leaked %q", secret)
		}
	}
	if !strings.Contains(got, "docs/check.go:42") {
		t.Fatal("relative finding lost")
	}
	if len([]rune(redact(strings.Repeat("a", 5000)))) > 4030 {
		t.Fatal("unbounded prose")
	}
}

func TestMissingAndBoundedHistory(t *testing.T) {
	s := Store{Dir: filepath.Join(t.TempDir(), "missing")}
	if l := s.List(context.Background()); len(l.Entries) != 0 || len(l.Warnings) != 0 {
		t.Fatal(l)
	}
	s.Dir = t.TempDir()
	family := filepath.Join(s.Dir, "agent-runs")
	os.Mkdir(family, 0700)
	for i := 0; i <= MaxEntries; i++ {
		f, err := os.CreateTemp(family, "record-")
		if err != nil {
			t.Fatal(err)
		}
		f.Close()
	}
	if l := s.List(context.Background()); len(l.Warnings) != 1 {
		t.Fatal("missing overflow warning")
	}
}
