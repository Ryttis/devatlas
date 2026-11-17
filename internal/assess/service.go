package assess

import (
	"context"
	"errors"
	"sync"
	"time"
)

// Queue limits.
const (
	DefaultQueueSize  = 64
	DefaultJobTimeout = 6 * time.Minute
	DefaultCooldown   = 20 * time.Second
)

// Job states.
const (
	JobIdle    = ""
	JobQueued  = "queued"
	JobRunning = "running"
)

// Enqueue errors.
var (
	ErrAlreadyScheduled = errors.New("projekto vertinimas jau suplanuotas arba vykdomas")
	ErrQueueFull        = errors.New("vertinimų eilė pilna")
	ErrCooldown         = errors.New("projektas ką tik vertintas; pakartoti galima po trumpos pertraukos")
	ErrStopped          = errors.New("vertinimų vykdymas sustabdytas")
)

// Service owns the bounded queue and its single worker: at most one job per
// project is queued or running, and jobs run one after another.
type Service struct {
	Engine     *Engine
	JobTimeout time.Duration
	Cooldown   time.Duration

	mu       sync.Mutex
	state    map[string]string
	finished map[string]time.Time
	jobs     chan Job
	stopped  bool
}

// NewService returns a service whose worker starts with Run.
func NewService(e *Engine) *Service {
	return &Service{Engine: e, JobTimeout: DefaultJobTimeout, Cooldown: DefaultCooldown,
		state: map[string]string{}, finished: map[string]time.Time{}, jobs: make(chan Job, DefaultQueueSize)}
}

// Enqueue schedules a job unless the project already has one, the queue is
// full, or the project finished a run moments ago.
func (s *Service) Enqueue(job Job) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return ErrStopped
	}
	if s.state[job.ProjectID] != JobIdle {
		return ErrAlreadyScheduled
	}
	if t, ok := s.finished[job.ProjectID]; ok && s.Engine.Now().Sub(t) < s.Cooldown {
		return ErrCooldown
	}
	select {
	case s.jobs <- job:
		s.state[job.ProjectID] = JobQueued
		return nil
	default:
		return ErrQueueFull
	}
}

// State returns the job state of a project.
func (s *Service) State(projectID string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state[projectID]
}

// Active reports whether any job is queued or running.
func (s *Service) Active() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.state) > 0
}

// Record loads the stored record of a project.
func (s *Service) Record(projectID string) (*Record, error) {
	return s.Engine.Store.Load(projectID)
}

// AIModel returns the configured model, or "" when AI is disabled.
func (s *Service) AIModel() string {
	if s.Engine.AI == nil {
		return ""
	}
	return s.Engine.AI.Model()
}

// Run is the single worker. It returns when ctx is cancelled; a job in
// flight is cancelled with it and recorded as a failed attempt.
func (s *Service) Run(ctx context.Context) {
	defer func() {
		s.mu.Lock()
		s.stopped = true
		s.mu.Unlock()
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case job := <-s.jobs:
			s.mu.Lock()
			s.state[job.ProjectID] = JobRunning
			s.mu.Unlock()
			jobCtx, cancel := context.WithTimeout(ctx, s.JobTimeout)
			s.Engine.Run(jobCtx, job)
			cancel()
			s.mu.Lock()
			delete(s.state, job.ProjectID)
			s.finished[job.ProjectID] = s.Engine.Now()
			s.mu.Unlock()
		}
	}
}

// StaleAfter is the age after which a result is marked stale: advisories
// and support dates move even when the project does not.
const StaleAfter = 24 * time.Hour

// Current describes the checkout as it is now, for comparison with what a
// result was computed from.
type Current struct {
	Registered bool
	GitOK      bool
	Head       string
	Dirty      bool
	// FilesHash is empty when it was not recomputed.
	FilesHash string
}

// Staleness lists the reasons a stored result no longer describes the
// present. An empty list means the result is current.
func Staleness(res *Result, now time.Time, cur Current) []string {
	var out []string
	if res.RulesVersion != RulesVersion {
		out = append(out, "pasikeitė taisyklių versija")
	}
	if now.Sub(res.SourcesCheckedAt) > StaleAfter {
		out = append(out, "šaltiniai tikrinti daugiau nei prieš 24 val.")
	}
	if !cur.Registered {
		return append(out, "vertintas checkout nebėra registre")
	}
	if cur.GitOK && res.Checkout.GitStatus == "ok" {
		if cur.Head != res.Checkout.Head {
			out = append(out, "pasikeitė HEAD")
		}
		if res.Checkout.Dirty != nil && *res.Checkout.Dirty != cur.Dirty {
			out = append(out, "pasikeitė neįrašytų pakeitimų būsena")
		}
	}
	if cur.FilesHash != "" && cur.FilesHash != res.Checkout.FilesHash {
		out = append(out, "pasikeitė vertinti failai")
	}
	return out
}
