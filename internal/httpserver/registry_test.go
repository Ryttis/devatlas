package httpserver

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Ryttis/devatlas/internal/registry"
	"github.com/Ryttis/devatlas/internal/registryfile"
)

func TestNewRejectsNilRegistrySource(t *testing.T) {
	if _, err := New(DefaultConfig(), nil, testLogger()); err == nil {
		t.Fatal("expected an error for a nil registry source")
	}
}

// testClient disables keep-alives: with them on, Go's Transport can leave a
// speculative connection open that the server counts as "new" rather than
// idle, which delays Shutdown by up to 5 seconds.
var testClient = &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}

func getBody(t *testing.T, url string) (int, string, http.Header) {
	t.Helper()
	resp, err := testClient.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(body), resp.Header
}

func TestProjectsAPIAndHomeReflectRegistrationsWithoutRestart(t *testing.T) {
	store := testStore(t)
	base, stop := startTestServerWith(t, store)
	defer func() {
		if err := stop(); err != nil {
			t.Errorf("stop: %v", err)
		}
	}()

	// Empty registry: file does not exist yet.
	code, body, hdr := getBody(t, base+"/api/projects")
	if code != http.StatusOK || !strings.HasPrefix(hdr.Get("Content-Type"), "application/json") {
		t.Fatalf("status %d, content-type %q", code, hdr.Get("Content-Type"))
	}
	var resp ProjectsResponse
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("decode: %v\n%s", err, body)
	}
	if len(resp.Projects) != 0 || resp.SchemaVersion != registry.SchemaVersion || resp.RuntimeStatus.Status != StatusUnknown {
		t.Fatalf("unexpected empty response: %s", body)
	}
	if !strings.Contains(resp.RuntimeStatus.Reason, "not collected") {
		t.Errorf("runtime status reason must say it was not collected: %q", resp.RuntimeStatus.Reason)
	}
	_, html, _ := getBody(t, base+"/")
	if !strings.Contains(html, "No projects registered yet") {
		t.Errorf("home page should say no projects are registered")
	}

	// Register through the store while the server runs (what the CLI does).
	dir := t.TempDir()
	hostile := `<script>alert("x")</script> & "quotes"`
	err := store.Update(func(reg *registry.Registry) error {
		_, err := reg.Add(registry.AddRequest{ProjectID: "evil", Name: hostile, Path: dir})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}

	code, body, _ = getBody(t, base+"/api/projects")
	if code != http.StatusOK {
		t.Fatalf("status %d: %s", code, body)
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Projects) != 1 || len(resp.Projects[0].Checkouts) != 1 {
		t.Fatalf("expected one project with one checkout: %s", body)
	}
	ck := resp.Projects[0].Checkouts[0]
	if ck.ID != registry.CheckoutID(ck.Path) || (ck.PathExists == nil || !*ck.PathExists) || ck.Runtime.Status != StatusUnknown || ck.CheckedAt.IsZero() {
		t.Errorf("unexpected checkout DTO: %+v", ck)
	}
	if resp.Projects[0].Name != hostile {
		t.Errorf("JSON must carry the raw name, got %q", resp.Projects[0].Name)
	}
	if resp.RegistryFile != store.Path() {
		t.Errorf("registry_file = %q, want %q", resp.RegistryFile, store.Path())
	}

	// HTML must escape the name and show the checkout without a restart.
	code, html, _ = getBody(t, base+"/")
	if code != http.StatusOK {
		t.Fatalf("home status %d", code)
	}
	if strings.Contains(html, "<script>alert") {
		t.Fatalf("project name was not HTML-escaped:\n%s", html)
	}
	for _, want := range []string{"&lt;script&gt;alert", "(evil)", ck.ID, "present", "unknown", "Runtime status has not been collected"} {
		if !strings.Contains(html, want) {
			t.Errorf("home page missing %q", want)
		}
	}

	// Mutation endpoints do not exist.
	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		req, _ := http.NewRequest(m, base+"/api/projects", strings.NewReader("{}"))
		r, err := testClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		if r.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("%s /api/projects status = %d, want 405", m, r.StatusCode)
		}
	}
}

func TestMalformedRegistryIsReportedNotOverwritten(t *testing.T) {
	path := filepath.Join(t.TempDir(), "registrations.yaml")
	bad := []byte("schema_version: 1\nprojects: [\n")
	if err := os.WriteFile(path, bad, 0o600); err != nil {
		t.Fatal(err)
	}
	store := registryfile.NewStore(path)
	base, stop := startTestServerWith(t, store)
	defer func() { _ = stop() }()

	code, body, _ := getBody(t, base+"/api/projects")
	if code != http.StatusInternalServerError || !strings.Contains(body, "cannot load registrations") {
		t.Fatalf("status %d body %s", code, body)
	}
	code, html, _ := getBody(t, base+"/")
	if code != http.StatusInternalServerError || !strings.Contains(html, "could not be loaded and was left untouched") {
		t.Fatalf("home status %d, body:\n%s", code, html)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(bad) {
		t.Fatalf("malformed file was modified:\n%s", after)
	}
}

func TestRegistryFailureAfterSuccessIsSafeAndRecovers(t *testing.T) {
	store := testStore(t)
	dir := t.TempDir()
	if err := store.Update(func(r *registry.Registry) error {
		_, err := r.Add(registry.AddRequest{ProjectID: "p", Name: "P", Path: dir})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	good, err := os.ReadFile(store.Path())
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	srv, err := New(DefaultConfig(), store, slog.New(slog.NewTextHandler(&logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	handler := srv.routes()
	const secret = "SENSITIVE_TEST_TOKEN"
	for _, bad := range []string{"schema_version: " + secret + "\n", "schema_version: 1\nprojects: [{id: '" + secret + "', name: P}]\n"} {
		for _, path := range []string{"/api/projects", "/"} {
			for _, broken := range []bool{false, true, false} {
				data := good
				want := http.StatusOK
				if broken {
					data = []byte(bad)
					want = http.StatusInternalServerError
				}
				if err := os.WriteFile(store.Path(), data, 0600); err != nil {
					t.Fatal(err)
				}
				recorder := httptest.NewRecorder()
				handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
				if recorder.Code != want {
					t.Fatalf("%s: status %d body %s", path, recorder.Code, recorder.Body.String())
				}
				if strings.Contains(recorder.Body.String(), secret) || strings.Contains(logs.String(), secret) {
					t.Fatal("secret leaked to HTTP or log")
				}
				if broken && (!strings.Contains(recorder.Body.String(), "cannot load registrations") || strings.Contains(recorder.Body.String(), "No projects registered yet")) {
					t.Fatal("failure shown as empty/success")
				}
			}
		}
	}
}

func TestDirectoryObservationPreservesRegistrations(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission fixture requires unprivileged user")
	}
	root := t.TempDir()
	parent := filepath.Join(root, "private")
	dir := filepath.Join(parent, "repo")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	store := testStore(t)
	if err := store.Update(func(r *registry.Registry) error {
		_, err := r.Add(registry.AddRequest{ProjectID: "p", Name: "P", Path: dir})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	srv, err := New(DefaultConfig(), store, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	handler := srv.routes()
	for _, state := range []string{"present", "error", "missing", "not a directory"} {
		switch state {
		case "error":
			if err := os.Chmod(parent, 0); err != nil {
				t.Fatal(err)
			}
			defer os.Chmod(parent, 0700)
		case "missing":
			if err := os.Chmod(parent, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(dir); err != nil {
				t.Fatal(err)
			}
		case "not a directory":
			if err := os.WriteFile(dir, nil, 0600); err != nil {
				t.Fatal(err)
			}
		}
		api := httptest.NewRecorder()
		handler.ServeHTTP(api, httptest.NewRequest(http.MethodGet, "/api/projects", nil))
		var response ProjectsResponse
		if err := json.Unmarshal(api.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		if api.Code != 200 || len(response.Projects) != 1 || len(response.Projects[0].Checkouts) != 1 {
			t.Fatalf("registration lost: %s", api.Body.String())
		}
		c := response.Projects[0].Checkouts[0]
		if c.PathStatus != state || (state == "error") != (c.PathExists == nil) {
			t.Fatalf("incorrect observation: %+v", c)
		}
		if c.PathExists != nil && *c.PathExists != (state == "present") {
			t.Fatalf("incorrect existence: %+v", c)
		}
		html := httptest.NewRecorder()
		handler.ServeHTTP(html, httptest.NewRequest(http.MethodGet, "/", nil))
		if html.Code != 200 || !strings.Contains(html.Body.String(), "<td>"+state+"</td>") {
			t.Fatalf("HTML disagrees: %s", html.Body.String())
		}
	}
}
