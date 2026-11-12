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
		b, _ = json.Marshal(map[string]any{"verdict": "changes_requested", "summary": "<script>alert(1)</script> password=SECRET_SENTINEL", "findings": []string{"[P2] docs/AGENT_SMOKE_TEST.md:30–32 — 32 lines, limit 30"}, "extra": "RAW_FIELD_SENTINEL"})
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
