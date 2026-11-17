package assess

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/Ryttis/devatlas/internal/collect/git"
	"github.com/Ryttis/devatlas/internal/collect/stack"
	"github.com/Ryttis/devatlas/internal/sources"
)

// GitObserver is the part of the Git collector the engine needs.
type GitObserver interface {
	Observe(ctx context.Context, path string) git.Observation
}

// Job names what to assess. Path is the registered canonical path.
type Job struct {
	ProjectID  string
	CheckoutID string
	Path       string
}

// Engine runs one assessment at a time on behalf of the queue.
type Engine struct {
	Sources *sources.Client
	// AI is nil when no adapter is configured.
	AI     *AIClient
	Store  *Store
	Git    GitObserver
	Log    *slog.Logger
	Now    func() time.Time
	Budget int
	// Collect reads the local facts; replaced in tests.
	Collect func(root string) (stack.Facts, error)
}

// failure is an assessment error with display-safe text.
type failure string

func (f failure) Error() string { return string(f) }

// Run assesses one checkout and stores the outcome. A failed attempt is
// recorded as such and leaves the previous result and its dates untouched.
func (e *Engine) Run(ctx context.Context, job Job) {
	now := e.Now().UTC().Truncate(time.Second)
	rec, err := e.Store.Load(job.ProjectID)
	if err != nil || rec == nil {
		// An unreadable record is replaced by the next successful result.
		rec = &Record{ProjectID: job.ProjectID}
	}
	rec.LastAttempt = Attempt{At: now, CheckoutID: job.CheckoutID}
	res, err := e.assess(ctx, job, now, rec.Result)
	if err != nil {
		var f failure
		if !errors.As(err, &f) {
			f = "vertinimas nepavyko dėl vidinės klaidos"
			e.Log.Warn("assessment failed", "project", job.ProjectID, "err", err.Error())
		}
		rec.LastAttempt.Error = string(f)
		e.Log.Info("assessment attempt failed", "project", job.ProjectID, "reason", string(f))
	} else {
		rec.Result = res
		rec.LastAttempt.OK = true
		e.Log.Info("assessment stored", "project", job.ProjectID, "freshness", res.Freshness.Status, "security", res.Security.Status,
			"findings", len(res.Findings), "requests", res.Requests, "ai", res.AI.Status)
	}
	if err := e.Store.Save(rec); err != nil {
		e.Log.Error("assessment record could not be saved", "project", job.ProjectID, "err", err.Error())
	}
}

func sourceFailure(err error) error {
	var se *sources.Error
	if errors.As(err, &se) {
		return failure("išorinis šaltinis nepasiekiamas arba grąžino klaidą — " + se.Error())
	}
	return err
}

func (e *Engine) assess(ctx context.Context, job Job, now time.Time, prev *Result) (*Result, error) {
	obs := e.Git.Observe(ctx, job.Path)
	facts, err := e.Collect(job.Path)
	if err != nil {
		return nil, failure("checkout katalogo nepavyko perskaityti")
	}
	if ctx.Err() != nil {
		return nil, failure("vertinimas nutrauktas")
	}

	// External data is fetched on every run, whether or not the project
	// files changed: advisories and support dates change on their own.
	run := e.Sources.NewRun(e.Budget)
	ext := External{Products: map[string]*sources.Product{}, Advisories: map[string][]sources.Advisory{},
		Registry: map[string]sources.RegistryInfo{}, RegistryMissing: map[string]bool{}}
	for _, t := range facts.Technologies {
		if t.Product == "" || t.Version == "" || ext.Products[t.Product] != nil {
			continue
		}
		p, err := run.EOL(ctx, t.Product)
		if errors.Is(err, sources.ErrNotFound) {
			continue // reported by the rules as an unverifiable component
		}
		if err != nil {
			return nil, sourceFailure(err)
		}
		ext.Products[t.Product] = p
	}
	var public, direct []sources.PublicPackage
	for _, p := range facts.Packages {
		if pub, ok := PublicPackage(p); ok {
			public = append(public, pub)
			if p.Direct {
				direct = append(direct, pub)
			}
		}
	}
	if len(public) > 0 {
		advs, err := run.OSV(ctx, public)
		if err != nil {
			return nil, sourceFailure(err)
		}
		ext.Advisories = advs
	}
	for _, pub := range direct {
		info, err := run.Registry(ctx, pub)
		if errors.Is(err, sources.ErrNotFound) {
			ext.RegistryMissing[pub.Key()] = true
			continue
		}
		if err != nil {
			return nil, sourceFailure(err)
		}
		ext.Registry[pub.Key()] = info
	}
	if ctx.Err() != nil {
		return nil, failure("vertinimas nutrauktas")
	}

	ev := Evaluate(facts, ext, now)
	res := &Result{
		RulesVersion:     RulesVersion,
		Checkout:         CheckoutRef{ID: job.CheckoutID, Path: job.Path, GitStatus: string(obs.Status), FilesHash: facts.FilesHash},
		SourcesCheckedAt: now,
		Description:      facts.Description,
		Files:            facts.Files,
		Ecosystems:       facts.Ecosystems,
		Technologies:     ev.Technologies,
		Packages:         ev.Packages,
		Freshness:        ev.Freshness,
		Security:         ev.Security,
		Findings:         ev.Findings,
		Sources:          []SourceRef{},
		Requests:         run.Requests(),
	}
	if r := obs.Repository; r != nil {
		dirty := r.Changes.Tracked > 0 || r.Changes.Untracked > 0
		res.Checkout.Head, res.Checkout.Branch, res.Checkout.Dirty = r.Head.Commit, r.Head.Branch, &dirty
	}
	if len(ext.Products) > 0 {
		res.Sources = append(res.Sources, SourceRef{"endoflife.date", "https://endoflife.date", "technologijų palaikymo datos"})
	}
	if len(public) > 0 {
		res.Sources = append(res.Sources, SourceRef{"OSV", "https://osv.dev", "žinomi užrakintų viešų paketų pažeidžiamumai"})
	}
	for _, p := range direct {
		name, url := "npm registras", "https://www.npmjs.com"
		if p.Ecosystem() == sources.Composer {
			name, url = "Packagist", "https://packagist.org"
		}
		ref := SourceRef{name, url, "tiesioginių priklausomybių naujausia versija ir deprecated žyma"}
		found := false
		for _, s := range res.Sources {
			found = found || s.Name == name
		}
		if !found {
			res.Sources = append(res.Sources, ref)
		}
	}
	res.AI = e.explain(ctx, facts, ev, now, prev)
	return res, nil
}

// explain attaches AI texts. Its failure never fails the assessment: the
// rule results are stored either way.
func (e *Engine) explain(ctx context.Context, facts stack.Facts, ev Evaluated, now time.Time, prev *Result) AIResult {
	in := buildAIInput(ev.Findings)
	if e.AI == nil {
		return AIResult{Status: AIDisabled, Reason: "DI adapteris nesukonfigūruotas; rodomi taisyklių radiniai ir veiksmai"}
	}
	if len(in.Findings) == 0 {
		return AIResult{Status: AINotNeeded, Model: e.AI.Model(), Reason: "nėra radinių, kuriuos reikėtų aiškinti"}
	}
	key := aiKey(e.AI.Model(), facts.FilesHash, ev, in)
	if prev != nil && prev.AI.Status == AIOK && prev.AI.Key == key {
		out := prev.AI
		out.Reused = true
		return out
	}
	texts, dropped, err := e.AI.Explain(ctx, in)
	if err != nil {
		out := AIResult{Status: AIFailed, Model: e.AI.Model(), Reason: err.Error() + "; rodomi taisyklių radiniai ir veiksmai", Dropped: dropped}
		var ae *aiError
		if errors.As(err, &ae) && ae.rejected {
			out.Status = AIRejected
		}
		return out
	}
	return AIResult{Status: AIOK, Model: e.AI.Model(), GeneratedAt: now, Key: key, Texts: texts, Dropped: dropped}
}
