// Package runhistory reads bounded, read-only projections of runner files.
// It never runs commands, reads checkout contents or serves arbitrary files.
package runhistory

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

var (
	ErrUnavailable = errors.New("record not found or unavailable")
	idRE           = regexp.MustCompile(`^(run|task)-[A-Za-z0-9_-]{6,64}$`)
	shaRE          = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

const MaxEntries = 2000

// Store.Dir is a trusted, operator-configured DevAtlas state directory.
// The final directory and all record-relative components must not be symlinks.
type Store struct{ Dir string }

func DefaultDir() (string, error) {
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		base = filepath.Join(home, ".local", "state")
	}
	if !filepath.IsAbs(base) {
		return "", errors.New("state directory must be absolute")
	}
	return filepath.Join(base, "devatlas"), nil
}

type Attempt struct {
	Slice   int    `json:"slice"`
	Attempt int    `json:"attempt"`
	RunID   string `json:"run_id"`
	Head    string `json:"head"`
	Review  string `json:"review"`
}

type Entry struct {
	ID          string
	Kind        string
	Stage       string
	UpdatedAt   time.Time
	Base        string
	Head        string
	FailedStage string
	GoChecks    string
	Review      string
	Tester      string
	Attempts    []Attempt
}

func (e Entry) URL() string { return "/runs/" + e.ID }
func (e Entry) Tone() string {
	switch e.Stage {
	case "pass", "PASSED":
		return "good"
	case "failed", "BLOCKED", "changes_requested", "PAUSED_FOR_HUMAN", "FIX_REQUIRED":
		return "bad"
	case "AWAITING_APPROVAL":
		return "wait"
	default:
		return "neutral"
	}
}
func (e Entry) Terminal() bool {
	switch e.Stage {
	case "pass", "changes_requested", "failed", "PASSED", "BLOCKED", "PAUSED_FOR_HUMAN":
		return true
	}
	return false
}

type Listing struct {
	Entries     []Entry
	Unavailable int
	Warnings    []string
}

type Report struct {
	Name        string
	Verdict     string
	Summary     string
	Findings    []string
	Unavailable bool
}
type Detail struct {
	Entry   Entry
	Reports []Report
}

func validID(id string) bool   { return idRE.MatchString(id) }
func validSHA(sha string) bool { return sha == "" || shaRE.MatchString(sha) }

func readEntry(dir *os.File, id string) (Entry, error) {
	e := Entry{ID: id, GoChecks: "not_confirmed", Review: "not_available", Tester: "not_available"}
	file := "state.json"
	if strings.HasPrefix(id, "task-") {
		file = "workflow.json"
		e.Kind = "workflow"
	} else {
		e.Kind = "run"
	}
	b, err := readJSON(dir, file, 64<<10)
	if err != nil {
		return Entry{}, ErrUnavailable
	}
	var s struct {
		RunID       string    `json:"run_id"`
		Stage       string    `json:"stage"`
		Status      string    `json:"status"`
		UpdatedAt   time.Time `json:"updated_at"`
		Base        string    `json:"base"`
		Head        string    `json:"head"`
		Candidate   string    `json:"candidate"`
		FailedStage string    `json:"failed_stage"`
		Review      string    `json:"review"`
		Tester      string    `json:"tester"`
		Slice       int       `json:"slice"`
		Attempts    []Attempt `json:"attempts"`
		Completed   []Attempt `json:"completed"`
	}
	if json.Unmarshal(b, &s) != nil || s.UpdatedAt.IsZero() || !validSHA(s.Base) || !validSHA(s.Head) || !validSHA(s.Candidate) {
		return Entry{}, ErrUnavailable
	}
	e.UpdatedAt, e.Base, e.Head = s.UpdatedAt, s.Base, s.Head
	if e.Kind == "run" {
		if s.RunID != id || s.Base == "" {
			return Entry{}, ErrUnavailable
		}
		e.Stage = s.Stage
		switch s.Stage {
		case "preparing", "coding", "verifying":
		case "reviewing", "pass", "changes_requested":
			if s.Head == "" {
				return Entry{}, ErrUnavailable
			}
			e.GoChecks = "passed"
			if s.Stage != "reviewing" {
				e.Review = s.Stage
			}
		case "failed":
			switch s.FailedStage {
			case "preparing", "coding", "verifying":
			case "reviewing":
				if s.Head == "" {
					return Entry{}, ErrUnavailable
				}
				e.GoChecks = "passed"
			default:
				return Entry{}, ErrUnavailable
			}
			e.FailedStage = s.FailedStage
		default:
			return Entry{}, ErrUnavailable
		}
	} else {
		e.Stage, e.Head = s.Status, s.Candidate
		switch s.Status {
		case "PLANNING", "AWAITING_APPROVAL", "RUNNING", "FIX_REQUIRED", "PAUSED_FOR_HUMAN", "TESTING", "NOTIFYING_ARCHITECT", "SLICE_PASSED", "PASSED", "BLOCKED":
		default:
			return Entry{}, ErrUnavailable
		}
		e.Review = allowed(s.Review, "pass", "changes_requested", "failed")
		e.Tester = allowed(s.Tester, "PASS", "FAIL", "BLOCKED")
		e.Attempts = s.Attempts
		if len(e.Attempts) == 0 {
			e.Attempts = s.Completed
		}
		// Older workflows record only the current run and completed slices.
		if s.RunID != "" {
			found := false
			for _, a := range e.Attempts {
				if a.RunID == s.RunID {
					found = true
				}
			}
			if !found {
				e.Attempts = append(e.Attempts, Attempt{Slice: s.Slice, RunID: s.RunID, Head: s.Candidate, Review: s.Review})
			}
		}
		if len(e.Attempts) > 32 {
			return Entry{}, ErrUnavailable
		}
		for i, a := range e.Attempts {
			if !validID(a.RunID) || !strings.HasPrefix(a.RunID, "run-") || !validSHA(a.Head) || a.Slice < 1 || a.Slice > 100 || a.Attempt < 0 || a.Attempt > 100 {
				return Entry{}, ErrUnavailable
			}
			e.Attempts[i].Review = allowed(a.Review, "pass", "PASS", "changes_requested", "failed")
		}
	}
	return e, nil
}

func allowed(value string, values ...string) string {
	for _, v := range values {
		if value == v {
			return v
		}
	}
	return "not_available"
}

func (s Store) List(ctx context.Context) Listing {
	out := Listing{Entries: []Entry{}}
	root, err := openDir(nil, s.Dir)
	if errors.Is(err, os.ErrNotExist) {
		return out
	}
	if err != nil {
		out.Warnings = append(out.Warnings, "Vykdymų katalogas neprieinamas.")
		return out
	}
	defer root.Close()
	for _, family := range []string{"agent-runs", "workflows"} {
		dir, err := openDir(root, family)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			out.Warnings = append(out.Warnings, family+": katalogas neprieinamas.")
			continue
		}
		entries, err := dir.ReadDir(MaxEntries + 1)
		if (err != nil && !errors.Is(err, io.EOF)) || len(entries) > MaxEntries {
			dir.Close()
			out.Warnings = append(out.Warnings, family+": nepavyko perskaityti arba viršyta 2000 įrašų riba.")
			continue
		}
		prefix := "run-"
		if family == "workflows" {
			prefix = "task-"
		}
		for _, item := range entries {
			if ctx.Err() != nil {
				dir.Close()
				out.Warnings = append(out.Warnings, "Skaitymas nutrauktas; sąrašas nepilnas.")
				return out
			}
			if !validID(item.Name()) || !strings.HasPrefix(item.Name(), prefix) {
				continue
			}
			job, err := openDir(dir, item.Name())
			if err != nil {
				out.Unavailable++
				continue
			}
			e, err := readEntry(job, item.Name())
			job.Close()
			if err != nil {
				out.Unavailable++
				continue
			}
			out.Entries = append(out.Entries, e)
		}
		dir.Close()
	}
	sort.Slice(out.Entries, func(i, j int) bool {
		if out.Entries[i].UpdatedAt.Equal(out.Entries[j].UpdatedAt) {
			return out.Entries[i].ID < out.Entries[j].ID
		}
		return out.Entries[i].UpdatedAt.After(out.Entries[j].UpdatedAt)
	})
	return out
}

func (s Store) Get(id string) (Detail, error) {
	if !validID(id) {
		return Detail{}, ErrUnavailable
	}
	root, err := openDir(nil, s.Dir)
	if err != nil {
		return Detail{}, ErrUnavailable
	}
	defer root.Close()
	family := "agent-runs"
	if strings.HasPrefix(id, "task-") {
		family = "workflows"
	}
	dir, err := openDir(root, family)
	if err != nil {
		return Detail{}, ErrUnavailable
	}
	defer dir.Close()
	job, err := openDir(dir, id)
	if err != nil {
		return Detail{}, ErrUnavailable
	}
	defer job.Close()
	e, err := readEntry(job, id)
	if err != nil {
		return Detail{}, ErrUnavailable
	}
	d := Detail{Entry: e}
	if e.Kind == "run" {
		d.Reports = append(d.Reports, readReview(job, e.Review), readTester(job))
	}
	return d, nil
}

// loadReport reads one bounded report file and decodes it strictly into dst.
// ok is false while the report is legitimately absent; unavailable marks a
// promised, unreadable or malformed report. Unknown fields, non-object
// documents and trailing data are rejected, mirroring the producer contracts.
func loadReport(dir *os.File, file string, promised bool, dst any) (ok, unavailable bool) {
	b, err := readJSON(dir, file, 64<<10)
	if errors.Is(err, os.ErrNotExist) {
		return false, promised
	}
	if err != nil {
		return false, true
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if dec.Decode(dst) != nil {
		return false, true
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return false, true
	}
	return true, false
}

// readReview projects review.json: exactly verdict, summary and findings.
// The verdict must equal the terminal run stage, and findings must be
// non-blank strings present exactly when changes are requested.
func readReview(dir *os.File, expected string) Report {
	r := Report{Name: "Reviewerio išvada", Verdict: "not_available"}
	if expected == "not_available" {
		return r
	}
	var data struct {
		Verdict  *string    `json:"verdict"`
		Summary  *string    `json:"summary"`
		Findings *[]*string `json:"findings"`
	}
	// A terminal review state promises a report; absence is inconsistent.
	ok, unavailable := loadReport(dir, "review.json", true, &data)
	if !ok {
		r.Unavailable = unavailable
		return r
	}
	r.Unavailable = true
	if data.Verdict == nil || *data.Verdict != expected || allowed(expected, "pass", "changes_requested") != expected ||
		data.Summary == nil || strings.TrimSpace(*data.Summary) == "" ||
		data.Findings == nil || len(*data.Findings) > 50 ||
		(len(*data.Findings) > 0) != (expected == "changes_requested") {
		return r
	}
	findings := make([]string, 0, len(*data.Findings))
	for _, finding := range *data.Findings {
		if finding == nil || strings.TrimSpace(*finding) == "" {
			return r
		}
		findings = append(findings, redact(*finding))
	}
	r.Unavailable, r.Verdict, r.Summary = false, *data.Verdict, redact(*data.Summary)
	if len(findings) > 0 {
		r.Findings = findings
	}
	return r
}

// readTester projects tester.json: exactly verdict and summary. Reviewer-only
// fields such as findings make the document invalid and are never projected.
func readTester(dir *os.File) Report {
	r := Report{Name: "Testerio išvada", Verdict: "not_available"}
	var data struct {
		Verdict *string `json:"verdict"`
		Summary *string `json:"summary"`
	}
	ok, unavailable := loadReport(dir, "tester.json", false, &data)
	if !ok {
		r.Unavailable = unavailable
		return r
	}
	if data.Verdict == nil || allowed(*data.Verdict, "PASS", "FAIL", "BLOCKED") == "not_available" ||
		data.Summary == nil || strings.TrimSpace(*data.Summary) == "" {
		r.Unavailable = true
		return r
	}
	r.Verdict, r.Summary = *data.Verdict, redact(*data.Summary)
	return r
}
