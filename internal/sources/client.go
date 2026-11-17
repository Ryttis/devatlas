// Package sources queries the external services DevAtlas relies on for
// facts it cannot know locally: OSV for known vulnerabilities,
// endoflife.date for support dates, and the npm and Packagist registries
// for the latest release and deprecation of direct dependencies.
//
// Nothing project-specific leaves the machine except the names and locked
// versions of packages that the lock file itself shows to come from the
// public registry. That rule is enforced by the PublicPackage type, which
// only Publishable can construct; every query function takes PublicPackage
// values or a product name from a fixed table.
package sources

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Default endpoints.
const (
	DefaultOSV       = "https://api.osv.dev"
	DefaultEOL       = "https://endoflife.date"
	DefaultNPM       = "https://registry.npmjs.org"
	DefaultPackagist = "https://repo.packagist.org"
)

// Limits.
const (
	DefaultRequestTimeout = 20 * time.Second
	DefaultRetries        = 2
	DefaultCacheTTL       = 10 * time.Minute
	DefaultRunBudget      = 1200
	maxRetryAfter         = 5 * time.Second
	maxSmallBody          = 4 << 20
	maxPackagistBody      = 24 << 20
	maxCacheEntries       = 20000
)

// ErrNotFound means the service answered that the item does not exist.
var ErrNotFound = errors.New("not found")

// Error is a failed query that says nothing about the queried item: the
// service was unreachable, answered with an error, or sent something that
// could not be interpreted. Its text is safe to display.
type Error struct {
	Source string
	Reason string
}

func (e *Error) Error() string { return e.Source + ": " + e.Reason }

// Client holds endpoints, limits, and a short-lived cache shared by runs.
type Client struct {
	HTTP      *http.Client
	UserAgent string
	OSV       string
	EOL       string
	NPM       string
	Packagist string
	Retries   int
	Backoff   time.Duration
	CacheTTL  time.Duration
	Now       func() time.Time

	mu    sync.Mutex
	cache map[string]cacheEntry
}

type cacheEntry struct {
	val any
	exp time.Time
}

// NewClient returns a client for the public endpoints. Redirects are not
// followed, so a request can never be steered to another host.
func NewClient(userAgent string) *Client {
	return &Client{
		HTTP: &http.Client{
			Timeout:       DefaultRequestTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		UserAgent: userAgent,
		OSV:       DefaultOSV, EOL: DefaultEOL, NPM: DefaultNPM, Packagist: DefaultPackagist,
		Retries: DefaultRetries, Backoff: 600 * time.Millisecond, CacheTTL: DefaultCacheTTL,
		Now:   time.Now,
		cache: map[string]cacheEntry{},
	}
}

func (c *Client) cached(key string) (any, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.cache[key]
	if !ok || !c.Now().Before(e.exp) {
		delete(c.cache, key)
		return nil, false
	}
	return e.val, true
}

func (c *Client) store(key string, val any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.cache) >= maxCacheEntries {
		c.cache = map[string]cacheEntry{}
	}
	c.cache[key] = cacheEntry{val: val, exp: c.Now().Add(c.CacheTTL)}
}

// Run is the set of queries of one assessment, with a request budget.
type Run struct {
	c         *Client
	mu        sync.Mutex
	remaining int
	requests  int
}

// NewRun starts a run that may send at most budget requests, retries
// included.
func (c *Client) NewRun(budget int) *Run {
	if budget <= 0 {
		budget = DefaultRunBudget
	}
	return &Run{c: c, remaining: budget}
}

// Requests returns how many requests the run has sent.
func (r *Run) Requests() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.requests
}

func (r *Run) spend() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.remaining <= 0 {
		return false
	}
	r.remaining--
	r.requests++
	return true
}

// do sends one request with bounded retries on transport errors, 429, and
// 5xx. It returns the body of a 200 response, ErrNotFound for 404, and an
// *Error otherwise.
func (r *Run) do(ctx context.Context, source, method, url string, body []byte, accept string, maxBody int64) ([]byte, error) {
	c := r.c
	var last string
	for attempt := 0; attempt <= c.Retries; attempt++ {
		if attempt > 0 {
			wait := c.Backoff * time.Duration(1<<(attempt-1))
			select {
			case <-ctx.Done():
				return nil, &Error{Source: source, Reason: "užklausa nutraukta"}
			case <-time.After(wait):
			}
		}
		if !r.spend() {
			return nil, &Error{Source: source, Reason: "viršyta vieno vertinimo užklausų riba"}
		}
		var rd io.Reader
		if body != nil {
			rd = bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, url, rd)
		if err != nil {
			return nil, &Error{Source: source, Reason: "netinkamas užklausos adresas"}
		}
		req.Header.Set("User-Agent", c.UserAgent)
		if accept != "" {
			req.Header.Set("Accept", accept)
		}
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := c.HTTP.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return nil, &Error{Source: source, Reason: "užklausa nutraukta arba baigėsi laikas"}
			}
			last = "tinklo klaida arba baigėsi laikas"
			continue
		}
		data, readErr := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
		resp.Body.Close()
		switch {
		case resp.StatusCode == http.StatusOK:
			if readErr != nil {
				last = "atsakymas nutrūko"
				continue
			}
			if int64(len(data)) > maxBody {
				return nil, &Error{Source: source, Reason: "atsakymas viršija dydžio ribą"}
			}
			return data, nil
		case resp.StatusCode == http.StatusNotFound:
			return nil, ErrNotFound
		case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
			last = fmt.Sprintf("HTTP %d", resp.StatusCode)
			if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && s > 0 {
				wait := time.Duration(s) * time.Second
				if wait > maxRetryAfter {
					wait = maxRetryAfter
				}
				select {
				case <-ctx.Done():
				case <-time.After(wait):
				}
			}
			continue
		default:
			return nil, &Error{Source: source, Reason: fmt.Sprintf("netikėtas atsakymas HTTP %d", resp.StatusCode)}
		}
	}
	return nil, &Error{Source: source, Reason: last + " po pakartotinių bandymų"}
}

// Ecosystems that can be queried.
const (
	Composer = "composer"
	NPM      = "npm"
)

var (
	composerNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,99}/[a-z0-9][a-z0-9_.-]{0,99}$`)
	npmNameRE      = regexp.MustCompile(`^(@[A-Za-z0-9][A-Za-z0-9._~-]{0,99}/)?[A-Za-z0-9][A-Za-z0-9._~-]{0,199}$`)
	versionRE      = regexp.MustCompile(`^v?\d+(\.\d+){0,3}([-+.][0-9A-Za-z][0-9A-Za-z.+-]{0,40})?$`)
)

// PublicPackage is a package that may be named to an external service. Its
// fields are unexported so that only Publishable can create a non-zero one.
type PublicPackage struct {
	ecosystem, name, version string
}

// Ecosystem, Name and Version return the published identity.
func (p PublicPackage) Ecosystem() string { return p.ecosystem }
func (p PublicPackage) Name() string      { return p.name }
func (p PublicPackage) Version() string   { return p.version }

// Key identifies the package in result maps.
func (p PublicPackage) Key() string { return p.ecosystem + "\x00" + p.name + "\x00" + p.version }

// Publishable decides whether a locked package may be named externally. It
// requires that the lock file itself proves a public-registry origin
// (public == true), a supported ecosystem, a well-formed name, and a
// release version. A package of unknown origin is refused: its publicity is
// never tested by asking a registry about it.
func Publishable(ecosystem, name, version string, public bool) (PublicPackage, bool) {
	if !public || !versionRE.MatchString(version) || strings.Contains(strings.ToLower(version), "dev") && ecosystem == Composer {
		return PublicPackage{}, false
	}
	switch ecosystem {
	case Composer:
		if !composerNameRE.MatchString(name) {
			return PublicPackage{}, false
		}
	case NPM:
		if !npmNameRE.MatchString(name) {
			return PublicPackage{}, false
		}
	default:
		return PublicPackage{}, false
	}
	return PublicPackage{ecosystem: ecosystem, name: name, version: version}, true
}

// cleanText makes external text safe to store and show: single line, no
// control characters, bounded length.
func cleanText(s string, max int) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	if rs := []rune(s); len(rs) > max {
		s = string(rs[:max]) + "…"
	}
	return s
}
