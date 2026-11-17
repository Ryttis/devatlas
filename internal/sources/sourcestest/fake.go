// Package sourcestest provides an in-process stand-in for OSV,
// endoflife.date, the npm registry, and Packagist, for tests.
package sourcestest

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"time"

	"github.com/Ryttis/devatlas/internal/sources"
)

// Fake serves canned answers and records every request it receives.
type Fake struct {
	mu sync.Mutex
	// Vulns maps "Ecosystem|name|version" to vulnerability identifiers.
	Vulns map[string][]string
	// Details maps a vulnerability identifier to its OSV JSON document.
	Details map[string]string
	// EOL maps a product slug to its endoflife.date v1 JSON document.
	EOL map[string]string
	// NPM maps "name/latest" or "name/version" to a manifest document.
	NPM map[string]string
	// Packagist maps "vendor/package" to its p2 metadata document.
	Packagist map[string]string
	// Fail maps a path prefix to how many requests still answer 500.
	Fail map[string]int
	// Down makes every request answer 503.
	Down bool
	// PageSize splits OSV results into pages when positive.
	PageSize int

	seen   []string
	Server *httptest.Server
}

// New starts a fake. Close it with Server.Close.
func New() *Fake {
	f := &Fake{Vulns: map[string][]string{}, Details: map[string]string{}, EOL: map[string]string{},
		NPM: map[string]string{}, Packagist: map[string]string{}, Fail: map[string]int{}}
	f.Server = httptest.NewServer(http.HandlerFunc(f.serve))
	return f
}

// Client returns a sources client pointed at the fake, with fast retries.
func (f *Fake) Client() *sources.Client {
	c := sources.NewClient("devatlas-test")
	c.OSV, c.EOL, c.NPM, c.Packagist = f.Server.URL, f.Server.URL, f.Server.URL, f.Server.URL
	c.Backoff = time.Millisecond
	return c
}

// Seen returns everything received: "METHOD path" lines and request bodies.
func (f *Fake) Seen() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.Join(f.seen, "\n")
}

// Count returns how many requests had a path with the given prefix.
func (f *Fake) Count(prefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, s := range f.seen {
		if _, path, ok := strings.Cut(s, " "); ok && strings.HasPrefix(path, prefix) {
			n++
		}
	}
	return n
}

// Set changes the fake under its lock.
func (f *Fake) Set(fn func(*Fake)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

func (f *Fake) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seen = append(f.seen, r.Method+" "+r.URL.Path)
	if len(body) > 0 {
		f.seen = append(f.seen, string(body))
	}
	if f.Down {
		http.Error(w, "down", http.StatusServiceUnavailable)
		return
	}
	for prefix, n := range f.Fail {
		if n > 0 && strings.HasPrefix(r.URL.Path, prefix) {
			f.Fail[prefix] = n - 1
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
	}
	reply := func(doc string, ok bool) {
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, doc)
	}
	p := r.URL.Path
	switch {
	case p == "/v1/querybatch":
		var req struct {
			Queries []struct {
				Package   struct{ Name, Ecosystem string }
				Version   string
				PageToken string `json:"page_token"`
			}
		}
		_ = json.Unmarshal(body, &req)
		type vuln struct {
			ID string `json:"id"`
		}
		type result struct {
			Vulns         []vuln `json:"vulns,omitempty"`
			NextPageToken string `json:"next_page_token,omitempty"`
		}
		out := struct {
			Results []result `json:"results"`
		}{Results: []result{}}
		for _, q := range req.Queries {
			ids := f.Vulns[q.Package.Ecosystem+"|"+q.Package.Name+"|"+q.Version]
			var res result
			if f.PageSize > 0 && len(ids) > f.PageSize {
				if q.PageToken == "" {
					ids, res.NextPageToken = ids[:f.PageSize], "next"
				} else {
					ids = ids[f.PageSize:]
				}
			}
			for _, id := range ids {
				res.Vulns = append(res.Vulns, vuln{ID: id})
			}
			out.Results = append(out.Results, res)
		}
		data, _ := json.Marshal(out)
		reply(string(data), true)
	case strings.HasPrefix(p, "/v1/vulns/"):
		doc, ok := f.Details[strings.TrimPrefix(p, "/v1/vulns/")]
		reply(doc, ok)
	case strings.HasPrefix(p, "/api/v1/products/"):
		doc, ok := f.EOL[strings.TrimPrefix(p, "/api/v1/products/")]
		reply(doc, ok)
	case strings.HasPrefix(p, "/p2/"):
		doc, ok := f.Packagist[strings.TrimSuffix(strings.TrimPrefix(p, "/p2/"), ".json")]
		reply(doc, ok)
	default:
		doc, ok := f.NPM[strings.TrimPrefix(p, "/")]
		reply(doc, ok)
	}
}

// EOLDoc builds an endoflife.date v1 document from release tuples:
// name, eolFrom ("" for null), eoasFrom ("" for null).
func EOLDoc(label string, releases ...[3]string) string {
	type rel struct {
		Name     string  `json:"name"`
		IsEOL    bool    `json:"isEol"`
		EOLFrom  *string `json:"eolFrom"`
		IsEOAS   *bool   `json:"isEoas"`
		EOASFrom *string `json:"eoasFrom"`
		Latest   any     `json:"latest"`
	}
	var doc struct {
		Result struct {
			Label    string `json:"label"`
			Releases []rel  `json:"releases"`
		} `json:"result"`
	}
	doc.Result.Label = label
	for _, r := range releases {
		x := rel{Name: r[0], Latest: map[string]string{"name": r[0] + ".9"}}
		if r[1] != "" {
			v := r[1]
			x.EOLFrom = &v
		}
		if r[2] != "" {
			v := r[2]
			x.EOASFrom = &v
		}
		doc.Result.Releases = append(doc.Result.Releases, x)
	}
	data, _ := json.Marshal(doc)
	return string(data)
}

// VulnDoc builds an OSV record. severity is the database label ("" for
// none); vector is a CVSS vector with its type inferred from its prefix.
func VulnDoc(id, pkg, ecosystem, severity, vector, fixed string, aliases ...string) string {
	doc := map[string]any{"id": id, "summary": "Summary of " + id, "aliases": aliases,
		"affected": []any{map[string]any{
			"package": map[string]string{"name": pkg, "ecosystem": ecosystem},
			"ranges":  []any{map[string]any{"type": "ECOSYSTEM", "events": []any{map[string]string{"introduced": "0"}, map[string]string{"fixed": fixed}}}},
		}}}
	if severity != "" {
		doc["database_specific"] = map[string]string{"severity": severity}
	}
	if vector != "" {
		typ := "CVSS_V3"
		if strings.HasPrefix(vector, "CVSS:4") {
			typ = "CVSS_V4"
		}
		doc["severity"] = []any{map[string]string{"type": typ, "score": vector}}
	}
	data, _ := json.Marshal(doc)
	return string(data)
}
