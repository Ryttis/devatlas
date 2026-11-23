package httpserver

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Ryttis/devatlas/internal/assess"
	"github.com/Ryttis/devatlas/internal/collect/git"
	"github.com/Ryttis/devatlas/internal/collect/stack"
	"github.com/Ryttis/devatlas/internal/config"
	"github.com/Ryttis/devatlas/internal/registry"
	"github.com/Ryttis/devatlas/internal/registryfile"
	"github.com/Ryttis/devatlas/internal/sources/sourcestest"
)

const dashKey = "sk-dashboard-KEY-must-not-appear"

type dash struct {
	t      *testing.T
	base   string
	store  *registryfile.Store
	fake   *sourcestest.Fake
	svc    *assess.Service
	dirA   string
	dirB   string
	bodies []string
}

func projectDir(t *testing.T, version string) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"composer.json": `{"description":"Demo <script>alert(1)</script>","require":{"acme/lib":"^1.0","corp/internal-sdk":"^2.0"}}`,
		"composer.lock": `{"packages":[{"name":"acme/lib","version":"` + version + `","notification-url":"https://packagist.org/downloads/"},
		  {"name":"corp/internal-sdk","version":"2.0.0","dist":{"url":"https://satis.corp.internal/x.zip"}}],"packages-dev":[]}`,
		"Dockerfile": "FROM php:8.4-fpm\n",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func newDash(t *testing.T) *dash {
	t.Helper()
	d := &dash{t: t, store: testStore(t), fake: sourcestest.New(), dirA: projectDir(t, "1.0.0"), dirB: projectDir(t, "1.0.0")}
	t.Cleanup(d.fake.Server.Close)
	d.fake.Vulns["Packagist|acme/lib|1.0.0"] = []string{"GHSA-aaaa-bbbb-cccc"}
	d.fake.Details["GHSA-aaaa-bbbb-cccc"] = sourcestest.VulnDoc("GHSA-aaaa-bbbb-cccc", "acme/lib", "Packagist", "CRITICAL", "", "1.0.5")
	d.fake.Packagist["acme/lib"] = `{"packages":{"acme/lib":[{"name":"acme/lib","version":"1.0.5"},{"version":"1.0.0"}]}}`
	d.fake.EOL["php"] = sourcestest.EOLDoc("PHP", [3]string{"8.4", "2028-12-31", ""})

	ai := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		content := `{"findings":[{"id":"F1","explanation":"DI tekstas <b>bold</b> apie radinį.","action":"DI siūlomas veiksmas."}]}`
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"content": content}}}})
	}))
	t.Cleanup(ai.Close)

	empty := t.TempDir()
	err := d.store.Update(func(reg *registry.Registry) error {
		for _, add := range []registry.AddRequest{
			{ProjectID: "demo", Name: "Demo Project", Path: d.dirA}, {ProjectID: "demo", Path: d.dirB},
			{ProjectID: "empty", Name: "Empty Project", Path: empty},
		} {
			if _, err := reg.Add(add); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	cfg := DefaultConfig()
	cfg.Port, cfg.ShutdownTimeout = 0, 2*time.Second
	srv, err := New(cfg, d.store, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	engine := &assess.Engine{
		Sources: d.fake.Client(),
		AI:      assess.NewAIClient(config.AI{BaseURL: ai.URL + "/v1", Model: "test-model", APIKey: config.NewSecret(dashKey)}),
		Store:   assess.NewStore(filepath.Join(t.TempDir(), "assessments")),
		Git:     git.NewCollector(git.Options{Logger: testLogger()}),
		Log:     testLogger(), Now: time.Now, Collect: stack.Collect,
	}
	d.svc = assess.NewService(engine)
	d.svc.Cooldown = 0
	srv.SetAssessment(d.svc)
	ln, err := srv.Listen()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx, ln) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("server did not stop")
		}
	})
	d.base = "http://" + ln.Addr().String()
	return d
}

func (d *dash) get(path string) (int, string) {
	d.t.Helper()
	code, body, _ := getBody(d.t, d.base+path)
	d.bodies = append(d.bodies, body)
	return code, body
}

// post sends a form the way a browser on the same origin would, then
// applies the header overrides.
func (d *dash) post(path string, form url.Values, headers map[string]string) (int, string) {
	d.t.Helper()
	req, _ := http.NewRequest(http.MethodPost, d.base+path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", d.base)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	for k, v := range headers {
		if k == "Host" {
			req.Host = v
		} else if v == "" {
			req.Header.Del(k)
		} else {
			req.Header.Set(k, v)
		}
	}
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		d.t.Fatal(err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, resp.Header.Get("Location")
}

func (d *dash) waitResult(project string) assess.Record {
	d.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if d.svc.State(project) == assess.JobIdle {
			code, body := d.get("/api/projects/" + project + "/assessment")
			if code == http.StatusOK {
				var rec assess.Record
				if err := json.Unmarshal([]byte(body), &rec); err != nil {
					d.t.Fatal(err)
				}
				return rec
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	d.t.Fatalf("no assessment stored for %s", project)
	return assess.Record{}
}

func checkoutID(t *testing.T, dir string) string {
	t.Helper()
	canonical, err := registry.CanonicalDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	return registry.CheckoutID(canonical)
}

func mustContain(t *testing.T, where, body string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(body, w) {
			t.Errorf("%s: missing %q", where, w)
		}
	}
}

func TestDashboardFlowTableRefreshResultCard(t *testing.T) {
	d := newDash(t)

	// 1. Table before any assessment: nothing is green.
	code, dashboard := d.get("/")
	if code != http.StatusOK {
		t.Fatalf("dashboard status %d", code)
	}
	mustContain(t, "dashboard before", dashboard, "Demo Project", "Empty Project", "Not checked", "Not configured", "⚪ Nepatikrinta", `href="/projects/demo"`, `href="/assessments"`)
	if strings.Contains(dashboard, "🟢") || strings.Contains(dashboard, "🔴") {
		t.Error("a project that was never assessed must not show an assessment status")
	}
	// /runs stays the agent execution console, not the assessment table.
	if code, runs := d.get("/runs"); code != http.StatusOK || !strings.Contains(runs, "Vykdymų konsolė") || strings.Contains(runs, "Atnaujinti visus") {
		t.Errorf("/runs must remain the agent execution console: %d", code)
	}
	if code, _ := d.get("/runs/run-missing"); code != http.StatusNotFound {
		t.Errorf("unknown run detail: %d", code)
	}
	if code, alias := d.get("/projects"); code != http.StatusOK || !strings.Contains(alias, "Demo Project") {
		t.Errorf("/projects must serve the dashboard: %d", code)
	}
	code, home := d.get("/assessments")
	if code != http.StatusOK {
		t.Fatalf("assessments status %d", code)
	}
	mustContain(t, "home before", home, "Demo Project", "Empty Project", "Nepatikrinta", "dar nevertinta", "Atnaujinti visus",
		`action="/projects/demo/assess"`, "Tai nėra visos sistemos saugumo auditas")
	if strings.Contains(home, "🟢") && strings.Contains(home, `class="status green"`) {
		t.Error("a project that was never assessed must not be green")
	}
	if code, _ := d.get("/api/projects/demo/assessment"); code != http.StatusNotFound {
		t.Errorf("assessment API before a run: %d", code)
	}

	// 2. Refresh the second checkout.
	idB := checkoutID(t, d.dirB)
	code, loc := d.post("/projects/demo/assess", url.Values{"checkout": {idB}, "return": {"project"}}, nil)
	if code != http.StatusSeeOther || loc != "/projects/demo?msg=queued" {
		t.Fatalf("POST assess: %d %q", code, loc)
	}

	// 3. Result.
	rec := d.waitResult("demo")
	res := rec.Result
	if res == nil || !rec.LastAttempt.OK {
		t.Fatalf("attempt: %+v", rec.LastAttempt)
	}
	canonB, _ := registry.CanonicalDir(d.dirB)
	if res.Checkout.ID != idB || res.Checkout.Path != canonB || res.Checkout.FilesHash == "" {
		t.Errorf("the selected checkout was not the one assessed: %+v", res.Checkout)
	}
	if res.Security.Status != assess.StatusRed || !res.Security.Partial || res.Freshness.Status != assess.StatusUnverified {
		t.Errorf("statuses: security %+v freshness %+v", res.Security, res.Freshness)
	}

	// 4. Table after the run.
	_, dashboard = d.get("/")
	// Health stays "Not checked": an assessment is not a runtime check.
	// Freshness and security appear separately with their existing labels.
	mustContain(t, "dashboard after", dashboard, `data-label="Health"><span class="na">Not checked</span>`,
		"🔴 Spręsti nedelsiant", "⚪ Nepatikrinta", "Dalinė patikra")
	_, home = d.get("/assessments")
	mustContain(t, "home after", home, "Spręsti nedelsiant", "Dalinė patikra", "PHP 8.4", canonB, "(1 iš 2 checkout)", "UTC", "test-model")
	if strings.Contains(home, "<script>alert(1)") {
		t.Error("project description is not escaped")
	}

	// 5. Card.
	code, card := d.get("/projects/demo")
	if code != http.StatusOK {
		t.Fatalf("card status %d", code)
	}
	mustContain(t, "card", card, "Demo Project", "Vertintas checkout", canonB, res.Checkout.FilesHash[:16],
		"GHSA-aaaa-bbbb-cccc", "SEC-CRITICAL", "https://osv.dev/vulnerability/GHSA-aaaa-bbbb-cccc", "Rekomenduojamas veiksmas", "1.0.5",
		"DI paaiškinimas", "DI siūlomas veiksmas.", "Nepatikrinta saugumo apimties dalis", "corp/internal-sdk",
		"Šaltiniai patikrinti ir taisyklės įvertintos", "Paskutinis bandymas", "deklaruota image žyma", "php:8.4-fpm",
		`value="`+idB+`" selected`, "Tai nėra visos sistemos saugumo auditas", "Git")
	if strings.Contains(card, "<b>bold</b>") {
		t.Error("AI text is not escaped")
	}
	if strings.Contains(card, "Rezultatas pasenęs") {
		t.Error("a fresh result is marked stale")
	}

	// 6. A changed assessed file marks the stored result stale.
	if err := os.WriteFile(filepath.Join(d.dirB, "Dockerfile"), []byte("FROM php:8.1-fpm\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, card = d.get("/projects/demo")
	mustContain(t, "stale card", card, "Rezultatas pasenęs", "pasikeitė vertinti failai")

	// 7. Refresh everything; the empty project ends up unverified, not green.
	code, loc = d.post("/assess-all", nil, nil)
	if code != http.StatusSeeOther || !strings.HasPrefix(loc, "/?msg=") {
		t.Fatalf("POST assess-all: %d %q", code, loc)
	}
	empty := d.waitResult("empty").Result
	if empty == nil || empty.Security.Status != assess.StatusUnverified || empty.Freshness.Status != assess.StatusUnverified {
		t.Errorf("a project without manifests must be unverified: %+v", empty)
	}
	d.waitResult("demo")
	if code, loc = d.post("/assess-all", url.Values{"return": {"assessments"}}, nil); code != http.StatusSeeOther || !strings.HasPrefix(loc, "/assessments?msg=") {
		t.Errorf("POST assess-all from the assessments page: %d %q", code, loc)
	}
	_, home = d.get("/assessments")
	mustContain(t, "home final", home, "manifestų")

	// 8. The key and private data never appear in any response; private
	// names never reached the sources.
	for _, body := range d.bodies {
		if strings.Contains(body, dashKey) {
			t.Fatal("a response contains the API key")
		}
	}
	if seen := d.fake.Seen(); strings.Contains(seen, "internal-sdk") || strings.Contains(seen, "satis.corp") {
		t.Error("a private package name was sent to an external source")
	}
}

func TestSourceFailureShowsAttemptButKeepsResult(t *testing.T) {
	d := newDash(t)
	if code, _ := d.post("/projects/demo/assess", nil, nil); code != http.StatusSeeOther {
		t.Fatal(code)
	}
	first := d.waitResult("demo")
	d.fake.Set(func(f *sourcestest.Fake) { f.Down = true })
	if code, _ := d.post("/projects/demo/assess", nil, nil); code != http.StatusSeeOther {
		t.Fatal(code)
	}
	deadline := time.Now().Add(15 * time.Second)
	var rec assess.Record
	for {
		rec = d.waitResult("demo")
		if !rec.LastAttempt.OK || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if rec.LastAttempt.OK || !rec.Result.SourcesCheckedAt.Equal(first.Result.SourcesCheckedAt) {
		t.Fatalf("attempt %+v, result date %v vs %v", rec.LastAttempt, rec.Result.SourcesCheckedAt, first.Result.SourcesCheckedAt)
	}
	_, home := d.get("/assessments")
	mustContain(t, "home", home, "Paskutinis bandymas", "nepavyko", "Spręsti nedelsiant")
	_, card := d.get("/projects/demo")
	mustContain(t, "card", card, "Rodomas ankstesnis sėkmingas rezultatas", "(nesėkmingas)")
}

func TestPostGuards(t *testing.T) {
	d := newDash(t)
	for name, tc := range map[string]struct {
		path    string
		form    url.Values
		headers map[string]string
		want    int
	}{
		"foreign origin":     {"/projects/demo/assess", nil, map[string]string{"Origin": "http://evil.example"}, http.StatusForbidden},
		"null origin":        {"/projects/demo/assess", nil, map[string]string{"Origin": "null"}, http.StatusForbidden},
		"cross-site fetch":   {"/projects/demo/assess", nil, map[string]string{"Origin": "", "Sec-Fetch-Site": "cross-site"}, http.StatusForbidden},
		"rebinding host":     {"/projects/demo/assess", nil, map[string]string{"Host": "evil.example", "Origin": "http://evil.example"}, http.StatusForbidden},
		"foreign port":       {"/assess-all", nil, map[string]string{"Host": "127.0.0.1:1", "Origin": "http://127.0.0.1:1"}, http.StatusForbidden},
		"json body":          {"/assess-all", nil, map[string]string{"Content-Type": "application/json"}, http.StatusForbidden},
		"text/plain form":    {"/assess-all", nil, map[string]string{"Content-Type": "text/plain"}, http.StatusForbidden},
		"unknown project":    {"/projects/nope/assess", nil, nil, http.StatusNotFound},
		"unknown checkout":   {"/projects/demo/assess", url.Values{"checkout": {"ffffffffffff"}}, nil, http.StatusBadRequest},
		"oversized form":     {"/projects/demo/assess", url.Values{"x": {strings.Repeat("a", maxFormBytes)}}, nil, http.StatusForbidden},
		"non-browser client": {"/projects/demo/assess", nil, map[string]string{"Origin": "", "Sec-Fetch-Site": ""}, http.StatusSeeOther},
	} {
		if code, _ := d.post(tc.path, tc.form, tc.headers); code != tc.want {
			t.Errorf("%s: status %d, want %d", name, code, tc.want)
		}
	}
	// A rejected request must not have started anything except the last case.
	d.waitResult("demo")
	if n := d.fake.Count("/v1/querybatch"); n != 1 {
		t.Errorf("expected exactly one assessment, got %d OSV batches", n)
	}
	resp, err := testClient.Get(d.base + "/projects/demo/assess")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET on a POST endpoint: %d", resp.StatusCode)
	}
	// One job per project: a second request while one is pending is refused.
	d.svc.Cooldown = time.Hour
	if _, loc := d.post("/projects/demo/assess", nil, nil); !strings.HasSuffix(loc, "msg=cooldown") {
		t.Errorf("repeat within the cooldown: %q", loc)
	}
}

func TestDashboardWithoutAssessmentService(t *testing.T) {
	store := testStore(t)
	dir := t.TempDir()
	if err := store.Update(func(reg *registry.Registry) error {
		_, err := reg.Add(registry.AddRequest{ProjectID: "demo", Name: "Demo", Path: dir})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	base, stop := startTestServerWith(t, store)
	defer stop()
	_, dashboard, _ := getBody(t, base+"/")
	mustContain(t, "dashboard", dashboard, "Demo", "Not checked")
	_, home, _ := getBody(t, base+"/assessments")
	mustContain(t, "home", home, "Nepatikrinta", "neįjungtas")
	if strings.Contains(home, "Atnaujinti visus") {
		t.Error("refresh buttons shown without a service")
	}
	code, card, _ := getBody(t, base+"/projects/demo")
	if code != http.StatusOK || !strings.Contains(card, "Nepatikrinta") {
		t.Errorf("card: %d", code)
	}
	if code, _, _ := getBody(t, base+"/projects/unknown"); code != http.StatusNotFound {
		t.Errorf("unknown project card: %d", code)
	}
	req, _ := http.NewRequest(http.MethodPost, base+"/assess-all", strings.NewReader(""))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := testClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("POST without a service: %d", resp.StatusCode)
	}
}
