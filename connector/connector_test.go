package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMCPReadOnlyHandoff(t *testing.T) {
	const sha = "cca346b3d110e29c477acac5e185799963bd8245"
	const id = "run-abcdefgh"
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, id), 0o700); err != nil {
		t.Fatal(err)
	}
	state := `{"run_id":"` + id + `","base":"` + sha + `","head":"` + sha +
		`","stage":"changes_requested","updated_at":"2026-11-10T20:00:00+00:00","error":"PRIVATE_MARKER"}`
	if err := os.WriteFile(filepath.Join(dir, id, "state.json"), []byte(state), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, id), filepath.Join(dir, "run-ijklmnop")); err != nil {
		t.Fatal(err)
	}
	const resource = "https://connector.example/mcp"
	const issuer = "https://identity.example"
	idp := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, secret, ok := r.BasicAuth()
		if !ok || user != "connector" || secret != "test-secret" || r.Method != http.MethodPost {
			http.Error(w, "bad introspection request", http.StatusUnauthorized)
			return
		}
		claims := map[string]any{"active": true, "iss": issuer, "sub": "owner", "aud": []string{resource},
			"scope": readScope, "token_type": "Bearer", "exp": time.Now().Add(time.Minute).Unix()}
		switch r.FormValue("token") {
		case "valid":
		case "other_user":
			claims["sub"] = "other"
		case "wrong_resource":
			claims["aud"] = "https://other.example/mcp"
		case "no_scope":
			claims["scope"] = "other:read"
		case "expired":
			claims["exp"] = time.Now().Add(-time.Hour).Unix()
		default:
			claims["active"] = false
		}
		_ = json.NewEncoder(w).Encode(claims)
	}))
	defer idp.Close()
	h := connector(runStore{dir}, accessConfig{resource, issuer, idp.URL, "connector", "test-secret", "owner"}, idp.Client())
	call := func(token, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:18643/mcp", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Accept", "application/json, text/event-stream")
		r.Header.Set("MCP-Protocol-Version", "2025-11-25")
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	const toolsList = `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`
	for token, want := range map[string]int{"": 401, "other_user": 401, "wrong_resource": 401, "expired": 401, "no_scope": 403} {
		w := call(token, toolsList)
		if w.Code != want || !strings.Contains(w.Header().Get("WWW-Authenticate"), metadataPath) {
			t.Errorf("%q: got %d %s", token, w.Code, w.Body.String())
		}
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, metadataPath, nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), resource) || !strings.Contains(w.Body.String(), issuer) {
		t.Fatalf("discovery: %d %s", w.Code, w.Body.String())
	}
	w = call("valid", `{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "devatlas-agent-status") {
		t.Fatalf("initialize: %d %s", w.Code, w.Body.String())
	}
	w = call("valid", toolsList)
	var catalog struct {
		Result struct {
			Tools []struct{ Name string } `json:"tools"`
		} `json:"result"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &catalog) != nil || len(catalog.Result.Tools) != 2 {
		t.Fatalf("tools/list: %d %s", w.Code, w.Body.String())
	}
	w = call("valid", `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"list_agent_runs","arguments":{"limit":1}}}`)
	var listed struct {
		Result struct {
			Structured runList `json:"structuredContent"`
		} `json:"result"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &listed) != nil || strings.Contains(w.Body.String(), "PRIVATE_MARKER") {
		t.Fatalf("tools/call: %d %s", w.Code, w.Body.String())
	}
	got := listed.Result.Structured
	if len(got.Runs) != 1 || got.Runs[0].ID != id || got.Runs[0].GoChecks != "passed" ||
		got.Runs[0].Review != "changes_requested" || got.Skipped != 1 {
		t.Fatalf("unexpected runs: %+v", got)
	}
	w = call("valid", `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"get_agent_run","arguments":{"run_id":"../secret"}}}`)
	if !strings.Contains(w.Body.String(), `"isError":true`) {
		t.Fatalf("invalid run ID: %d %s", w.Code, w.Body.String())
	}
}
