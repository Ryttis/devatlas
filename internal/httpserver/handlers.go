package httpserver

import (
	"embed"
	"encoding/json"
	"html/template"
	"net/http"
	"time"

	"github.com/Ryttis/devatlas/internal/collect/git"
	"github.com/Ryttis/devatlas/internal/version"
)

//go:embed templates/*.html
var templateFS embed.FS

// pageFuncs are the helpers shared by every page template.
var pageFuncs = template.FuncMap{
	"deref": func(b *bool) bool { return b != nil && *b },
	"basis": basisLabel,
	"short": func(sha string) string {
		if len(sha) > 12 {
			return sha[:12]
		}
		return sha
	},
	"uncommitted": func(ch git.Changes) int { return ch.Tracked + ch.Untracked },
	"ts": func(v any) string {
		switch t := v.(type) {
		case time.Time:
			return fmtTime(t)
		case *time.Time:
			if t != nil {
				return fmtTime(*t)
			}
		}
		return ""
	},
	"gitProblem": gitProblem,
}

// page parses one page together with the shared layout partials.
func page(name string) *template.Template {
	return template.Must(template.New(name).Funcs(pageFuncs).ParseFS(templateFS, "templates/layout.html", "templates/"+name))
}

var (
	homeTemplate        = page("home.html")
	assessmentsTemplate = page("assessments.html")
	instanceTemplate    = page("instance.html")
)

// gitProblem words a non-ok Git observation for display.
func gitProblem(o git.Observation) string {
	switch o.Status {
	case git.StatusNotRepository:
		return "Not a Git repository"
	case git.StatusMissing:
		return "Directory missing"
	case git.StatusUnavailable:
		return "Unknown: git is not available"
	}
	if o.Reason != "" {
		return "Unknown: " + o.Reason
	}
	return "Unknown"
}

// Health is the JSON body of GET /health.
type Health struct {
	Status  string `json:"status"`
	Service string `json:"service"`
	Version string `json:"version"`
	Commit  string `json:"commit"`
	// Modified is true when the binary was built from a working tree with
	// uncommitted changes, so Commit does not fully describe the code.
	Modified  bool      `json:"modified"`
	CheckedAt time.Time `json:"checked_at"`
	// UptimeSeconds is measured since the listener started serving.
	UptimeSeconds int64 `json:"uptime_seconds"`
}

// Scope describes what this build of DevAtlas does and does not do. It is
// rendered on the instance details page so the UI never implies
// unimplemented features.
type Scope struct {
	Slice       string
	Implemented []string
	Deferred    []string
}

// CurrentScope returns the implementation status of this build.
func CurrentScope() Scope {
	return Scope{
		Slice: "04 — technology status dashboard (first stage)",
		Implemented: []string{
			"devatlas CLI: version, serve, add, list, remove",
			"loopback-only HTTP server with configurable port and graceful shutdown",
			"GET /health JSON endpoint",
			"registration of existing directories as checkouts of explicitly named projects, persisted in a YAML file under the user configuration directory",
			"GET /api/projects (read-only), the project dashboard, and project detail pages, reloaded from the file on every request",
			"read-only Git observation per checkout, collected for each request by running the local git command: worktree root, branch or detached HEAD, commit, change counts, sanitised remotes",
			"ahead/behind counts against the branch upstream and the remote default branch, based on last fetched refs only (DevAtlas never fetches)",
			"read-only agent execution console at /runs (saved runner state; nothing can be started from the UI)",
			"technology status assessment runs (freshness and known vulnerabilities of declared technologies and locked dependencies)",
		},
		Deferred: []string{
			"SQLite index and observation history",
			"project-registry.yaml manifest loading and directory scanning",
			"live remote checks (fetching) for ahead/behind information",
			"project domain or application address configuration",
			"Docker Compose and container status collection",
			"port-conflict analysis",
			"HTTP application checks",
			"project context export for coding agents",
		},
	}
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("GET /api/projects", s.handleProjects)
	mux.HandleFunc("GET /api/projects/{id}/assessment", s.handleAssessmentAPI)
	mux.HandleFunc("GET /projects/{id}", s.handleProject)
	mux.HandleFunc("POST /projects/{id}/assess", s.handleAssess)
	mux.HandleFunc("POST /assess-all", s.handleAssessAll)
	// /runs belongs to the read-only agent execution console; the
	// technology assessments have their own path.
	mux.HandleFunc("GET /runs", s.handleRuns)
	mux.HandleFunc("GET /runs/{id}", s.handleRun)
	mux.HandleFunc("GET /assessments", s.handleAssessments)
	mux.HandleFunc("GET /projects", s.handleHome)
	mux.HandleFunc("GET /instance", s.handleInstance)
	mux.HandleFunc("GET /{$}", s.handleHome)
	return mux
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	var uptime int64
	if !s.started.IsZero() {
		uptime = int64(now.Sub(s.started).Seconds())
	}
	info := version.Current()
	body := Health{
		Status:        "ok",
		Service:       "devatlas",
		Version:       info.Version,
		Commit:        info.Commit,
		Modified:      info.Modified,
		CheckedAt:     now.UTC(),
		UptimeSeconds: uptime,
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(body); err != nil {
		s.log.Warn("write health response", "err", err)
	}
}

// dashRow is one project on the dashboard. Health (runtime and application
// availability) has no field: nothing collects it in this build, so the
// template always shows "Not checked". Fresh and Sec are the stored
// technology assessment, shown separately with their existing labels.
type dashRow struct {
	ID        string
	Name      string
	Fresh     statusView
	Sec       statusView
	Stale     bool
	Job       string
	Checkouts []CheckoutDTO
}

type homeData struct {
	Nav           string
	Now           string
	RegistryError string
	Rows          []dashRow
	Flash         string
}

// assessmentsData feeds the technology status table.
type assessmentsData struct {
	Nav           string
	RegistryError string
	Rows          []rowView
	ScopeNote     string
	Flash         string
	Enabled       bool
	Refresh       bool
	AIModel       string
}

type instanceData struct {
	Nav           string
	Version       version.Info
	Scope         Scope
	BindAddr      string
	Now           string
	Started       string
	RegistryFile  string
	RegistryError string
	Projects      int
	Checkouts     int
	Assessment    bool
	AIModel       string
}

func (s *Server) render(w http.ResponseWriter, t *template.Template, status int, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if status != http.StatusOK {
		w.WriteHeader(status)
	}
	if err := t.Execute(w, data); err != nil {
		s.log.Warn("render page", "template", t.Name(), "err", err)
	}
}

// handleHome renders the project dashboard.
func (s *Server) handleHome(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	data := homeData{Nav: "projects", Now: fmtTime(now), Flash: flashMessages[r.URL.Query().Get("msg")]}
	status := http.StatusOK
	if reg, err := s.registry.Load(); err != nil {
		s.log.Warn(registryLoadError)
		data.RegistryError = registryLoadError
		status = http.StatusInternalServerError
	} else {
		resp := ObserveProjects(r.Context(), reg.Views(), s.registry.Path(), now, s.git)
		for i, row := range s.buildRows(&resp, now) {
			p := resp.Projects[i]
			data.Rows = append(data.Rows, dashRow{ID: p.ID, Name: p.Name, Fresh: row.Fresh, Sec: row.Sec,
				Stale: len(row.Stale) > 0, Job: row.Job, Checkouts: p.Checkouts})
		}
	}
	s.render(w, homeTemplate, status, data)
}

// handleAssessments renders the stored technology status of every project
// with the controls that queue a new assessment.
func (s *Server) handleAssessments(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	data := assessmentsData{Nav: "assessments", ScopeNote: ScopeNote, Flash: flashMessages[r.URL.Query().Get("msg")]}
	status := http.StatusOK
	if reg, err := s.registry.Load(); err != nil {
		s.log.Warn(registryLoadError)
		data.RegistryError = registryLoadError
		status = http.StatusInternalServerError
	} else {
		resp := ObserveProjects(r.Context(), reg.Views(), s.registry.Path(), now, s.git)
		data.Rows = s.buildRows(&resp, now)
	}
	if s.assess != nil {
		data.Enabled, data.Refresh, data.AIModel = true, s.assess.Active(), s.assess.AIModel()
	}
	s.render(w, assessmentsTemplate, status, data)
}

// handleInstance renders build, runtime, registry, and scope information.
// It runs no git commands.
func (s *Server) handleInstance(w http.ResponseWriter, r *http.Request) {
	data := instanceData{
		Nav:          "instance",
		Version:      version.Current(),
		Scope:        CurrentScope(),
		BindAddr:     s.cfg.Addr(),
		Now:          fmtTime(time.Now()),
		Started:      fmtTime(s.started),
		RegistryFile: s.registry.Path(),
	}
	if reg, err := s.registry.Load(); err != nil {
		s.log.Warn(registryLoadError)
		data.RegistryError = registryLoadError
	} else {
		data.Projects, data.Checkouts = len(reg.Projects), len(reg.Checkouts)
	}
	if s.assess != nil {
		data.Assessment, data.AIModel = true, s.assess.AIModel()
	}
	s.render(w, instanceTemplate, http.StatusOK, data)
}
