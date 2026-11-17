package assess

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Ryttis/devatlas/internal/collect/git"
	"github.com/Ryttis/devatlas/internal/collect/stack"
	"github.com/Ryttis/devatlas/internal/config"
	"github.com/Ryttis/devatlas/internal/sources/sourcestest"
)

const apiKey = "sk-test-KEY-do-not-leak-42"

type fakeGit struct{ head string }

func (g fakeGit) Observe(context.Context, string) git.Observation {
	return git.Observation{Status: git.StatusOK, Source: git.Source,
		Repository: &git.Repository{Head: git.Head{Branch: "main", Commit: g.head}, Changes: git.Changes{Tracked: 1}}}
}

// fakeAI is an OpenAI-compatible endpoint with a scripted answer.
type fakeAI struct {
	mu      sync.Mutex
	calls   int
	bodies  []string
	auth    []string
	status  int
	content string
	srv     *httptest.Server
}

func newFakeAI() *fakeAI {
	a := &fakeAI{status: 200}
	a.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		a.mu.Lock()
		defer a.mu.Unlock()
		a.calls++
		a.bodies = append(a.bodies, string(body))
		a.auth = append(a.auth, r.Header.Get("Authorization"))
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		if a.status != 200 {
			// Echo the key the way a careless provider might.
			http.Error(w, "bad key "+r.Header.Get("Authorization"), a.status)
			return
		}
		content := a.content
		if content == "" {
			// Explain every finding that was sent.
			var req struct {
				Messages []struct{ Content string }
			}
			_ = json.Unmarshal(body, &req)
			var in aiInput
			_ = json.Unmarshal([]byte(req.Messages[1].Content), &in)
			var out struct {
				Findings []map[string]string `json:"findings"`
			}
			for _, f := range in.Findings {
				out.Findings = append(out.Findings, map[string]string{"id": f.ID, "explanation": "Paaiškinimas " + f.ID, "action": "Veiksmas " + f.ID})
			}
			b, _ := json.Marshal(out)
			content = string(b)
		}
		resp := map[string]any{"choices": []any{map[string]any{"message": map[string]string{"role": "assistant", "content": content}}}}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	return a
}

func (a *fakeAI) set(status int, content string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.status, a.content = status, content
}

func (a *fakeAI) count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.calls
}

func (a *fakeAI) allBodies() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return strings.Join(a.bodies, "\n")
}

type fixture struct {
	t      *testing.T
	root   string
	fake   *sourcestest.Fake
	ai     *fakeAI
	engine *Engine
	logs   *bytes.Buffer
	now    time.Time
	mu     sync.Mutex
}

func (fx *fixture) clock() time.Time {
	fx.mu.Lock()
	defer fx.mu.Unlock()
	return fx.now
}

func (fx *fixture) advance(d time.Duration) {
	fx.mu.Lock()
	defer fx.mu.Unlock()
	fx.now = fx.now.Add(d)
}

func writeFile(t *testing.T, root, rel, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, rel), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// newFixture builds a project with a vulnerable public package, a private
// one, and an image, plus fake sources and a fake AI endpoint.
func newFixture(t *testing.T, withAI bool) *fixture {
	t.Helper()
	fx := &fixture{t: t, root: t.TempDir(), fake: sourcestest.New(), logs: &bytes.Buffer{}, now: today}
	t.Cleanup(fx.fake.Server.Close)
	writeFile(t, fx.root, "composer.json", `{"description":"Fixture","require":{"acme/lib":"^1.0","corp/secret-sdk":"^2.0"}}`)
	writeFile(t, fx.root, "composer.lock", `{"packages":[
	 {"name":"acme/lib","version":"1.0.0","notification-url":"https://packagist.org/downloads/"},
	 {"name":"corp/secret-sdk","version":"2.0.0","dist":{"url":"https://satis.corp.internal/dist/sdk.zip"}}],"packages-dev":[]}`)
	writeFile(t, fx.root, "Dockerfile", "FROM php:8.2-fpm\n")
	writeFile(t, fx.root, ".env", "DB_PASSWORD=ENV_SECRET_MARKER\n")

	fx.fake.Vulns["Packagist|acme/lib|1.0.0"] = []string{"GHSA-aaaa-bbbb-cccc"}
	fx.fake.Details["GHSA-aaaa-bbbb-cccc"] = sourcestest.VulnDoc("GHSA-aaaa-bbbb-cccc", "acme/lib", "Packagist", "CRITICAL", "", "1.0.5")
	fx.fake.Packagist["acme/lib"] = `{"packages":{"acme/lib":[{"name":"acme/lib","version":"1.0.5"},{"version":"1.0.0"}]}}`
	fx.fake.EOL["php"] = sourcestest.EOLDoc("PHP", [3]string{"8.4", "2028-12-31", ""}, [3]string{"8.2", "2026-12-31", "2024-12-31"})

	fx.engine = &Engine{Sources: fx.fake.Client(), Store: NewStore(filepath.Join(t.TempDir(), "assessments")),
		Git: fakeGit{head: "0123456789abcdef0123456789abcdef01234567"}, Log: slog.New(slog.NewTextHandler(fx.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Now: fx.clock, Collect: stack.Collect}
	if withAI {
		fx.ai = newFakeAI()
		t.Cleanup(fx.ai.srv.Close)
		fx.useModel("test-model")
	}
	return fx
}

func (fx *fixture) useModel(model string) {
	c := NewAIClient(config.AI{BaseURL: fx.ai.srv.URL + "/v1", Model: model, APIKey: config.NewSecret(apiKey)})
	c.Backoff = time.Millisecond
	fx.engine.AI = c
}

func (fx *fixture) run() *Record {
	fx.t.Helper()
	fx.engine.Run(context.Background(), Job{ProjectID: "demo", CheckoutID: "abc123", Path: fx.root})
	rec, err := fx.engine.Store.Load("demo")
	if err != nil || rec == nil {
		fx.t.Fatalf("record not stored: %v", err)
	}
	return rec
}

func TestAssessmentStoresRuleResultWithEvidenceAndKeepsPrivateDataLocal(t *testing.T) {
	fx := newFixture(t, true)
	rec := fx.run()
	res := rec.Result
	if !rec.LastAttempt.OK || res == nil {
		t.Fatalf("attempt failed: %+v", rec.LastAttempt)
	}
	if res.Security.Status != StatusRed || !res.Security.Partial {
		t.Errorf("security must be red with partial coverage (private package unchecked): %+v", res.Security)
	}
	if res.Freshness.Status != StatusYellow || !res.Freshness.Partial {
		t.Errorf("freshness must be yellow (PHP 8.2 ends within 180 days) and partial: %+v", res.Freshness)
	}
	ck := res.Checkout
	if ck.Path != fx.root || ck.ID != "abc123" || ck.Head == "" || ck.Dirty == nil || !*ck.Dirty || ck.FilesHash == "" || ck.Branch != "main" {
		t.Errorf("result is not tied to the assessed checkout: %+v", ck)
	}
	if res.AI.Status != AIOK || len(res.AI.Texts) == 0 || res.AI.GeneratedAt.IsZero() || !res.SourcesCheckedAt.Equal(today) {
		t.Errorf("AI result or times: %+v", res.AI)
	}

	// Nothing private leaves the machine.
	for name, sent := range map[string]string{"sources": fx.fake.Seen(), "AI": fx.ai.allBodies()} {
		for _, private := range []string{"corp/secret-sdk", "secret-sdk", "satis.corp.internal", "ENV_SECRET_MARKER", fx.root, "composer.lock"} {
			if strings.Contains(sent, private) {
				t.Errorf("%s received %q", name, private)
			}
		}
	}
	if !strings.Contains(fx.fake.Seen(), "acme/lib") {
		t.Error("the public package was not checked")
	}

	// The key goes into the Authorization header and nowhere else.
	if fx.ai.auth[0] != "Bearer "+apiKey {
		t.Errorf("Authorization header = %q", fx.ai.auth[0])
	}
	stored, _ := os.ReadFile(filepath.Join(fx.engine.Store.Dir(), "demo.json"))
	for name, text := range map[string]string{"record": string(stored), "logs": fx.logs.String(), "AI bodies": fx.ai.allBodies(), "sources": fx.fake.Seen()} {
		if strings.Contains(text, apiKey) {
			t.Errorf("%s contains the API key", name)
		}
	}
	if strings.Contains(string(stored), "ENV_SECRET_MARKER") {
		t.Error(".env content reached the record")
	}
	info, _ := os.Stat(filepath.Join(fx.engine.Store.Dir(), "demo.json"))
	if info.Mode().Perm() != 0o600 {
		t.Errorf("record permissions = %v", info.Mode().Perm())
	}
}

func TestAIFailureOrRejectionNeverHidesRuleResults(t *testing.T) {
	for name, tc := range map[string]struct {
		status  int
		content string
		want    string
	}{
		"provider error":    {401, "", AIFailed},
		"server error":      {500, "", AIFailed},
		"not json":          {200, "I think everything is fine.", AIRejected},
		"tries to go green": {200, `{"status":"green","findings":[]}`, AIRejected},
		"invents a finding": {200, `{"findings":[{"id":"F99","explanation":"x","action":"y"}]}`, AIRejected},
	} {
		fx := newFixture(t, true)
		fx.ai.set(tc.status, tc.content)
		rec := fx.run()
		res := rec.Result
		if !rec.LastAttempt.OK || res == nil {
			t.Fatalf("%s: an AI problem must not fail the assessment: %+v", name, rec.LastAttempt)
		}
		if res.AI.Status != tc.want || len(res.AI.Texts) != 0 || res.AI.Reason == "" {
			t.Errorf("%s: AI = %+v", name, res.AI)
		}
		if res.Security.Status != StatusRed || len(res.Findings) < 2 || res.Findings[0].Action == "" {
			t.Errorf("%s: rule results lost: %s, %d findings", name, res.Security.Status, len(res.Findings))
		}
		stored, _ := os.ReadFile(filepath.Join(fx.engine.Store.Dir(), "demo.json"))
		if strings.Contains(string(stored), apiKey) || strings.Contains(fx.logs.String(), apiKey) {
			t.Errorf("%s: a provider error echoed the key into the record or the log", name)
		}
	}
}

func TestWithoutAIConfigurationRulesStillProduceResults(t *testing.T) {
	fx := newFixture(t, false)
	res := fx.run().Result
	if res == nil || res.AI.Status != AIDisabled || res.Security.Status != StatusRed {
		t.Fatalf("%+v", res)
	}
}

func TestExternalDataIsRefreshedAndAIReusedOnlyForIdenticalInput(t *testing.T) {
	fx := newFixture(t, true)
	first := fx.run().Result
	fx.advance(2 * time.Hour)
	second := fx.run().Result

	if fx.fake.Count("/v1/querybatch") != 2 {
		t.Errorf("OSV must be queried on every run even when files did not change, got %d", fx.fake.Count("/v1/querybatch"))
	}
	if !second.SourcesCheckedAt.After(first.SourcesCheckedAt) {
		t.Error("the sources date must advance")
	}
	if fx.ai.count() != 1 || !second.AI.Reused || !second.AI.GeneratedAt.Equal(first.AI.GeneratedAt) || second.AI.Key != first.AI.Key {
		t.Errorf("identical input must reuse the explanation and keep its date: calls=%d %+v", fx.ai.count(), second.AI)
	}

	// A new external conclusion changes the key.
	fx.fake.Set(func(f *sourcestest.Fake) {
		f.Vulns["Packagist|acme/lib|1.0.0"] = []string{"GHSA-aaaa-bbbb-cccc", "GHSA-dddd-eeee-ffff"}
		f.Details["GHSA-dddd-eeee-ffff"] = sourcestest.VulnDoc("GHSA-dddd-eeee-ffff", "acme/lib", "Packagist", "LOW", "", "1.0.5")
	})
	fx.advance(time.Hour)
	third := fx.run().Result
	if fx.ai.count() != 2 || third.AI.Reused || third.AI.Key == first.AI.Key {
		t.Errorf("changed findings must regenerate: calls=%d", fx.ai.count())
	}
	// A different model changes the key.
	fx.useModel("other-model")
	fx.advance(time.Hour)
	if fourth := fx.run().Result; fx.ai.count() != 3 || fourth.AI.Model != "other-model" {
		t.Errorf("a different model must regenerate: calls=%d", fx.ai.count())
	}
	// A changed project file changes the key.
	writeFile(t, fx.root, "Dockerfile", "FROM php:8.2-fpm\n# comment\n")
	fx.advance(time.Hour)
	if fx.run(); fx.ai.count() != 4 {
		t.Errorf("changed files must regenerate: calls=%d", fx.ai.count())
	}
}

func TestSupportStatusFollowsTheDateOfTheRun(t *testing.T) {
	fx := newFixture(t, false)
	if res := fx.run().Result; res.Freshness.Status != StatusYellow {
		t.Fatalf("before the end of support: %+v", res.Freshness)
	}
	fx.advance(60 * 24 * time.Hour) // past 2026-12-31, same files, same cached source data
	if res := fx.run().Result; res.Freshness.Status != StatusRed {
		t.Fatalf("after the end of support the same data must give red: %+v", res.Freshness)
	}
}

func TestFailedAttemptKeepsPreviousResultAndItsDate(t *testing.T) {
	fx := newFixture(t, false)
	ok := fx.run()
	fx.fake.Set(func(f *sourcestest.Fake) { f.Down = true })
	fx.advance(3 * time.Hour)
	rec := fx.run()
	if rec.LastAttempt.OK || !strings.Contains(rec.LastAttempt.Error, "OSV") || !rec.LastAttempt.At.Equal(today.Add(3*time.Hour)) {
		t.Errorf("last attempt: %+v", rec.LastAttempt)
	}
	if rec.Result == nil || !rec.Result.SourcesCheckedAt.Equal(ok.Result.SourcesCheckedAt) || rec.Result.Security.Status != StatusRed {
		t.Errorf("the previous result or its date changed: %+v", rec.Result)
	}

	// With no previous result there is nothing to show but the failure.
	fresh := newFixture(t, false)
	fresh.fake.Set(func(f *sourcestest.Fake) { f.Down = true })
	rec = fresh.run()
	if rec.Result != nil || rec.LastAttempt.OK || rec.LastAttempt.Error == "" {
		t.Errorf("a failed first attempt must not create a result: %+v", rec)
	}

	// A missing checkout directory is a failed attempt too.
	gone := newFixture(t, false)
	gone.root = filepath.Join(gone.root, "removed")
	if rec := gone.run(); rec.Result != nil || rec.LastAttempt.OK {
		t.Errorf("a missing directory must fail the attempt: %+v", rec)
	}
}

func TestStaleness(t *testing.T) {
	dirty := true
	res := &Result{RulesVersion: RulesVersion, SourcesCheckedAt: today,
		Checkout: CheckoutRef{GitStatus: "ok", Head: "aaa", Dirty: &dirty, FilesHash: "h1"}}
	cur := Current{Registered: true, GitOK: true, Head: "aaa", Dirty: true, FilesHash: "h1"}
	if s := Staleness(res, today.Add(time.Hour), cur); len(s) != 0 {
		t.Errorf("fresh result marked stale: %v", s)
	}
	for name, tc := range map[string]struct {
		now time.Time
		cur Current
	}{
		"age":          {today.Add(25 * time.Hour), cur},
		"head":         {today, Current{Registered: true, GitOK: true, Head: "bbb", Dirty: true, FilesHash: "h1"}},
		"dirty":        {today, Current{Registered: true, GitOK: true, Head: "aaa", Dirty: false, FilesHash: "h1"}},
		"files":        {today, Current{Registered: true, GitOK: true, Head: "aaa", Dirty: true, FilesHash: "h2"}},
		"unregistered": {today, Current{}},
	} {
		if s := Staleness(res, tc.now, tc.cur); len(s) != 1 {
			t.Errorf("%s: want exactly one reason, got %v", name, s)
		}
	}
	old := *res
	old.RulesVersion = "older"
	if s := Staleness(&old, today, cur); len(s) != 1 {
		t.Errorf("rules version: %v", s)
	}
}

func TestQueueAllowsOneJobPerProjectAndIsBounded(t *testing.T) {
	fx := newFixture(t, false)
	release := make(chan struct{})
	started := make(chan string, 8)
	fx.engine.Collect = func(root string) (stack.Facts, error) {
		started <- root
		<-release
		return stack.Collect(root)
	}
	svc := NewService(fx.engine)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { svc.Run(ctx); close(done) }()

	job := Job{ProjectID: "demo", CheckoutID: "c1", Path: fx.root}
	if err := svc.Enqueue(job); err != nil {
		t.Fatal(err)
	}
	<-started
	if svc.State("demo") != JobRunning || !svc.Active() {
		t.Errorf("state = %q", svc.State("demo"))
	}
	if err := svc.Enqueue(job); err != ErrAlreadyScheduled {
		t.Errorf("second job for a running project: %v", err)
	}
	other := Job{ProjectID: "other", CheckoutID: "c2", Path: fx.root}
	if err := svc.Enqueue(other); err != nil {
		t.Fatal(err)
	}
	if err := svc.Enqueue(other); err != ErrAlreadyScheduled || svc.State("other") != JobQueued {
		t.Errorf("second job for a queued project: %v", err)
	}
	// Only one job runs at a time: the second has not started.
	select {
	case <-started:
		t.Fatal("two jobs ran concurrently")
	case <-time.After(50 * time.Millisecond):
	}
	full := 0
	for i := 0; i < DefaultQueueSize+5; i++ {
		if svc.Enqueue(Job{ProjectID: "p" + string(rune('a'+i%26)) + string(rune('a'+i/26)), Path: fx.root}) == ErrQueueFull {
			full++
		}
	}
	if full == 0 {
		t.Error("the queue must be bounded")
	}
	close(release)
	deadline := time.After(10 * time.Second)
	for svc.State("demo") != JobIdle {
		select {
		case <-deadline:
			t.Fatal("job did not finish")
		case <-time.After(5 * time.Millisecond):
		}
	}
	if err := svc.Enqueue(job); err != ErrCooldown {
		t.Errorf("an immediate repeat must be refused: %v", err)
	}
	cancel()
	<-done
	if err := svc.Enqueue(Job{ProjectID: "late", Path: fx.root}); err != ErrStopped {
		t.Errorf("after shutdown: %v", err)
	}
}
