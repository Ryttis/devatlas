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
	for _, uri := range []string{
		"ssh://alice:demo-pass@example.invalid/repo",
		"git+ssh://alice:demo-pass@example.invalid/repo.git",
		"mongodb+srv://alice:demo-pass@example.invalid/db",
		"mongodb://alice:demo-pass@example.invalid:27017/db?authSource=admin",
		"SFTP://alice:demo-pass@example.invalid",
		"x-custom.scheme-1://alice:demo-pass@example.invalid/x",
		"alice:demo-pass@example.invalid",
		"//alice:demo-pass@example.invalid/share",
	} {
		got := redact("see docs/check.go:42 (" + uri + ") then retry")
		for _, secret := range []string{"alice", "demo-pass"} {
			if strings.Contains(got, secret) {
				t.Errorf("%s leaked %q: %s", uri, secret, got)
			}
		}
		if !strings.Contains(got, "docs/check.go:42") || !strings.Contains(got, "then retry") {
			t.Errorf("%s: surrounding prose lost: %s", uri, got)
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

func putRaw(t *testing.T, root, id, file, doc string) {
	t.Helper()
	dir := filepath.Join(root, "agent-runs", id)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, file), []byte(doc), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestReviewerAndTesterContractsAreSeparate(t *testing.T) {
	root := t.TempDir()
	s := Store{Dir: root}
	id := "run-aaaaaa"
	reports := func(stage, review, tester string) (Report, Report) {
		t.Helper()
		put(t, root, "agent-runs", id, "state.json", state(id, stage))
		putRaw(t, root, id, "review.json", review)
		putRaw(t, root, id, "tester.json", tester)
		d, err := s.Get(id)
		if err != nil || len(d.Reports) != 2 {
			t.Fatalf("detail: %+v %v", d, err)
		}
		return d.Reports[0], d.Reports[1]
	}
	rejected := func(r Report) bool {
		return r.Unavailable && r.Verdict == "not_available" && r.Summary == "" && r.Findings == nil
	}
	const (
		reviewPass    = `{"verdict":"pass","summary":"Looks right","findings":[]}`
		reviewChanges = `{"verdict":"changes_requested","summary":"One issue","findings":["[P2] a.go:1 — issue"]}`
		testerPass    = `{"verdict":"PASS","summary":"Checks passed"}`
	)

	review, tester := reports("pass", reviewPass, testerPass)
	if review.Unavailable || review.Verdict != "pass" || review.Summary != "Looks right" || review.Findings != nil {
		t.Fatalf("valid pass review: %+v", review)
	}
	if tester.Unavailable || tester.Verdict != "PASS" || tester.Summary != "Checks passed" || tester.Findings != nil {
		t.Fatalf("valid tester: %+v", tester)
	}
	review, _ = reports("changes_requested", reviewChanges, testerPass)
	if review.Unavailable || review.Verdict != "changes_requested" || len(review.Findings) != 1 || review.Findings[0] != "[P2] a.go:1 — issue" {
		t.Fatalf("valid changes review: %+v", review)
	}
	for _, verdict := range []string{"FAIL", "BLOCKED"} {
		if _, tester = reports("pass", reviewPass, `{"verdict":"`+verdict+`","summary":"Recorded"}`); tester.Unavailable || tester.Verdict != verdict {
			t.Fatalf("valid tester %s: %+v", verdict, tester)
		}
	}

	for name, doc := range map[string]string{
		"injected findings":    `{"verdict":"PASS","summary":"ok","findings":["RAW_FIELD_SENTINEL"]}`,
		"empty findings":       `{"verdict":"PASS","summary":"ok","findings":[]}`,
		"null findings":        `{"verdict":"PASS","summary":"ok","findings":null}`,
		"unknown field":        `{"verdict":"PASS","summary":"ok","extra":"x"}`,
		"reviewer verdict":     `{"verdict":"pass","summary":"ok"}`,
		"numeric verdict":      `{"verdict":1,"summary":"ok"}`,
		"null verdict":         `{"verdict":null,"summary":"ok"}`,
		"missing verdict":      `{"summary":"ok"}`,
		"null summary":         `{"verdict":"PASS","summary":null}`,
		"blank summary":        `{"verdict":"PASS","summary":" \n\t"}`,
		"missing summary":      `{"verdict":"PASS"}`,
		"array summary":        `{"verdict":"PASS","summary":["ok"]}`,
		"null document":        `null`,
		"array document":       `[{"verdict":"PASS","summary":"ok"}]`,
		"string document":      `"PASS"`,
		"trailing document":    `{"verdict":"PASS","summary":"ok"}{"verdict":"FAIL","summary":"no"}`,
		"trailing garbage":     `{"verdict":"PASS","summary":"ok"} x`,
		"truncated document":   `{"verdict":"PASS","summary":"ok"`,
		"empty file":           ``,
		"empty object":         `{}`,
		"bool verdict summary": `{"verdict":true,"summary":false}`,
	} {
		review, tester = reports("pass", reviewPass, doc)
		if !rejected(tester) {
			t.Errorf("tester %s accepted: %+v", name, tester)
		}
		// A bad tester document must not hide the independent, valid review.
		if review.Unavailable || review.Verdict != "pass" {
			t.Errorf("tester %s affected review: %+v", name, review)
		}
	}

	many := `["x"` + strings.Repeat(`,"x"`, 50) + `]`
	for name, c := range map[string]struct{ stage, doc string }{
		"null findings":             {"changes_requested", `{"verdict":"changes_requested","summary":"s","findings":null}`},
		"null findings on pass":     {"pass", `{"verdict":"pass","summary":"s","findings":null}`},
		"missing findings":          {"pass", `{"verdict":"pass","summary":"s"}`},
		"empty finding":             {"changes_requested", `{"verdict":"changes_requested","summary":"s","findings":[""]}`},
		"blank finding":             {"changes_requested", `{"verdict":"changes_requested","summary":"s","findings":["real"," \n\t "]}`},
		"null finding":              {"changes_requested", `{"verdict":"changes_requested","summary":"s","findings":["real",null]}`},
		"numeric finding":           {"changes_requested", `{"verdict":"changes_requested","summary":"s","findings":[1]}`},
		"object finding":            {"changes_requested", `{"verdict":"changes_requested","summary":"s","findings":[{"text":"x"}]}`},
		"nested finding":            {"changes_requested", `{"verdict":"changes_requested","summary":"s","findings":[["x"]]}`},
		"string findings":           {"changes_requested", `{"verdict":"changes_requested","summary":"s","findings":"x"}`},
		"object findings":           {"changes_requested", `{"verdict":"changes_requested","summary":"s","findings":{"0":"x"}}`},
		"too many findings":         {"changes_requested", `{"verdict":"changes_requested","summary":"s","findings":` + many + `}`},
		"changes without findings":  {"changes_requested", `{"verdict":"changes_requested","summary":"s","findings":[]}`},
		"pass with findings":        {"pass", `{"verdict":"pass","summary":"s","findings":["x"]}`},
		"verdict differs (pass)":    {"pass", reviewChanges},
		"verdict differs (changes)": {"changes_requested", reviewPass},
		"tester verdict":            {"pass", `{"verdict":"PASS","summary":"s","findings":[]}`},
		"null verdict":              {"pass", `{"verdict":null,"summary":"s","findings":[]}`},
		"numeric verdict":           {"pass", `{"verdict":1,"summary":"s","findings":[]}`},
		"null summary":              {"pass", `{"verdict":"pass","summary":null,"findings":[]}`},
		"blank summary":             {"pass", `{"verdict":"pass","summary":"  ","findings":[]}`},
		"numeric summary":           {"pass", `{"verdict":"pass","summary":7,"findings":[]}`},
		"unknown field":             {"pass", `{"verdict":"pass","summary":"s","findings":[],"extra":"x"}`},
		"null document":             {"pass", `null`},
		"array document":            {"pass", `[]`},
		"trailing document":         {"pass", reviewPass + reviewPass},
		"empty file":                {"pass", ``},
	} {
		review, tester = reports(c.stage, c.doc, testerPass)
		if !rejected(review) {
			t.Errorf("review %s accepted: %+v", name, review)
		}
		if tester.Unavailable || tester.Verdict != "PASS" {
			t.Errorf("review %s affected tester: %+v", name, tester)
		}
	}
}
