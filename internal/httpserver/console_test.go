//go:build linux

package httpserver

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConsoleReadOnlyFlow(t *testing.T) {
	cfg := DefaultConfig()
	cfg.StateDir = t.TempDir()
	srv, err := New(cfg, testStore(t), testLogger())
	if err != nil {
		t.Fatal(err)
	}
	h := srv.routes()
	request := func(method, path string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(method, path, nil))
		return w
	}
	if w := request("GET", "/"); w.Code != 200 || !strings.Contains(w.Body.String(), "Vykdymų dar nėra") {
		t.Fatal("missing empty state")
	}
	for i := 0; i < 51; i++ {
		id := fmt.Sprintf("run-%06d", i)
		dir := filepath.Join(cfg.StateDir, "agent-runs", id)
		os.MkdirAll(dir, 0700)
		b, _ := json.Marshal(map[string]any{"run_id": id, "stage": "changes_requested", "base": strings.Repeat("a", 40), "head": strings.Repeat("b", 40), "updated_at": "2026-11-12T12:00:00Z", "error": "RAW_ERROR_SENTINEL"})
		os.WriteFile(filepath.Join(dir, "state.json"), b, 0600)
		b, _ = json.Marshal(map[string]any{"verdict": "changes_requested", "summary": "<script>alert(1)</script> password=SECRET_SENTINEL", "findings": []string{"[P2] docs/AGENT_SMOKE_TEST.md:30–32 — 32 lines, limit 30"}})
		os.WriteFile(filepath.Join(dir, "review.json"), b, 0600)
	}
	w := request("GET", "/runs")
	if w.Code != 200 || strings.Contains(w.Body.String(), "run-000050") || !strings.Contains(w.Body.String(), "Kitas") {
		t.Fatal("pagination incorrect")
	}
	w = request("GET", "/runs?page=2")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "run-000050") {
		t.Fatal("second page missing")
	}
	w = request("GET", "/runs?kind=workflow")
	if strings.Contains(w.Body.String(), "run-000000") {
		t.Fatal("filter ignored")
	}
	w = request("GET", "/runs/run-000000")
	body := w.Body.String()
	for _, want := range []string{"&lt;script&gt;", "docs/AGENT_SMOKE_TEST.md:30–32", "[paslėpta]", "changes_requested", "Praėjo pagal runnerio etapą"} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q", want)
		}
	}
	for _, bad := range []string{"<script>", "SECRET_SENTINEL", "RAW_ERROR_SENTINEL", "RAW_FIELD_SENTINEL"} {
		if strings.Contains(body, bad) {
			t.Errorf("exposed %q", bad)
		}
	}
	if w.Header().Get("Cache-Control") != "no-store" || !strings.Contains(w.Header().Get("Content-Security-Policy"), "frame-ancestors 'none'") {
		t.Fatal("missing response protection")
	}
	for _, path := range []string{"/runs/run-000000/review.json", "/runs/run-missing", "/runs?page=3"} {
		if w := request("GET", path); w.Code != 404 {
			t.Errorf("%s = %d", path, w.Code)
		}
	}
	for _, path := range []string{"/", "/runs", "/runs/run-000000"} {
		if w := request("POST", path); w.Code != 405 {
			t.Errorf("POST %s = %d", path, w.Code)
		}
	}
	for _, path := range []string{"/runs?kind=invalid", "/runs?page=-1", "/runs?page=999999999999999999999"} {
		if w := request("GET", path); w.Code != 400 {
			t.Errorf("%s = %d", path, w.Code)
		}
	}
}

func TestConsoleReportContractsAndURIRedaction(t *testing.T) {
	cfg := DefaultConfig()
	cfg.StateDir = t.TempDir()
	srv, err := New(cfg, testStore(t), testLogger())
	if err != nil {
		t.Fatal(err)
	}
	h := srv.routes()
	const unavailable = "Ataskaita nepasiekiama"
	write := func(id, file, doc string) {
		t.Helper()
		dir := filepath.Join(cfg.StateDir, "agent-runs", id)
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, file), []byte(doc), 0600); err != nil {
			t.Fatal(err)
		}
	}
	page := func(id, stage string) string {
		t.Helper()
		b, _ := json.Marshal(map[string]any{"run_id": id, "stage": stage, "base": strings.Repeat("a", 40), "head": strings.Repeat("b", 40), "updated_at": "2026-11-12T12:00:00Z"})
		write(id, "state.json", string(b))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", "/runs/"+id, nil))
		if w.Code != 200 {
			t.Fatalf("%s = %d", id, w.Code)
		}
		return w.Body.String()
	}
	absent := func(body string, values ...string) {
		t.Helper()
		for _, bad := range values {
			if strings.Contains(body, bad) {
				t.Errorf("exposed %q", bad)
			}
		}
	}

	// Valid reviewer and tester reports render; URI credentials never do.
	write("run-valid0", "review.json", `{"verdict":"changes_requested","summary":"Clone ssh://alice:demo-pass@example.invalid/repo fails","findings":["REVIEW_FINDING git+ssh://alice:demo-pass@example.invalid/repo.git","mongodb+srv://alice:demo-pass@example.invalid/db in docs/a.md:3","bare alice:demo-pass@example.invalid"]}`)
	write("run-valid0", "tester.json", `{"verdict":"FAIL","summary":"TESTER_SUMMARY redis+tls://alice:demo-pass@example.invalid:6380/0"}`)
	body := page("run-valid0", "changes_requested")
	for _, want := range []string{"REVIEW_FINDING", "docs/a.md:3", "TESTER_SUMMARY", "FAIL", "[paslėpta]"} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q", want)
		}
	}
	absent(body, "demo-pass", "alice", "example.invalid", unavailable)

	// Apostrophes are valid userinfo, including inside single-quoted prose.
	// Check reviewer summary, findings and tester summary through HTML rendering.
	for _, scheme := range []string{"ssh", "git+ssh", "mongodb+srv"} {
		for _, password := range []string{"prefix'TAIL_SENTINEL", "prefix%27TAIL_SENTINEL", "prefix!$&'()*+,;=:TAIL_SENTINEL"} {
			uri := scheme + "://alice:" + password + "@example.invalid/repo"
			review, err := json.Marshal(map[string]any{"verdict": "changes_requested", "summary": "REVIEW_SUMMARY '" + uri + "' after", "findings": []string{"REVIEW_FINDING " + uri + " docs/a.md:3"}})
			if err != nil {
				t.Fatal(err)
			}
			tester, err := json.Marshal(map[string]any{"verdict": "FAIL", "summary": "TESTER_SUMMARY " + uri + " after"})
			if err != nil {
				t.Fatal(err)
			}
			write("run-quotes", "review.json", string(review))
			write("run-quotes", "tester.json", string(tester))
			body = page("run-quotes", "changes_requested")
			absent(body, "alice", "prefix", "TAIL_SENTINEL", "example.invalid", unavailable)
			for _, want := range []string{"REVIEW_SUMMARY", "REVIEW_FINDING", "TESTER_SUMMARY", "docs/a.md:3", "[paslėpta]"} {
				if !strings.Contains(body, want) {
					t.Errorf("%s: missing %q", scheme, want)
				}
			}
		}
	}

	// A tester document carrying reviewer-only findings is rejected, not rendered.
	write("run-tester", "review.json", `{"verdict":"pass","summary":"REVIEW_SUMMARY","findings":[]}`)
	write("run-tester", "tester.json", `{"verdict":"PASS","summary":"TESTER_SUMMARY","findings":["RAW_FIELD_SENTINEL"]}`)
	body = page("run-tester", "pass")
	if !strings.Contains(body, "REVIEW_SUMMARY") || strings.Count(body, unavailable) != 1 {
		t.Error("tester document with findings was not rejected on its own")
	}
	absent(body, "RAW_FIELD_SENTINEL", "TESTER_SUMMARY", "Pastabos")

	// Reviewer documents outside the producer contract are rejected.
	for i, doc := range []string{
		`{"verdict":"changes_requested","summary":"RAW_FIELD_SENTINEL","findings":["RAW_FIELD_SENTINEL"],"extra":"x"}`,
		`{"verdict":"changes_requested","summary":"RAW_FIELD_SENTINEL","findings":null}`,
		`{"verdict":"changes_requested","summary":"RAW_FIELD_SENTINEL","findings":["RAW_FIELD_SENTINEL","  "]}`,
		`{"verdict":"changes_requested","summary":"RAW_FIELD_SENTINEL","findings":["RAW_FIELD_SENTINEL",null]}`,
		`{"verdict":"changes_requested","summary":"RAW_FIELD_SENTINEL","findings":"RAW_FIELD_SENTINEL"}`,
		`null`,
	} {
		id := fmt.Sprintf("run-bad%03d", i)
		write(id, "review.json", doc)
		body = page(id, "changes_requested")
		if !strings.Contains(body, unavailable) {
			t.Errorf("case %d accepted", i)
		}
		absent(body, "RAW_FIELD_SENTINEL", "Pastabos")
	}
}
