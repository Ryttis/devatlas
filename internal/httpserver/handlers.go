package httpserver

import (
	"embed"
	"encoding/json"
	"html/template"
	"net/http"
	"time"

	"github.com/Ryttis/devatlas/internal/version"
)

//go:embed templates/*.html
var templateFS embed.FS

var homeTemplate = template.Must(template.ParseFS(templateFS, "templates/home.html"))

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
// rendered on the home page so the UI never implies unimplemented features.
type Scope struct {
	Slice       string
	Implemented []string
	Deferred    []string
}

// CurrentScope returns the implementation status of this build.
func CurrentScope() Scope {
	return Scope{
		Slice: "03 — read-only Git checkout observations",
		Implemented: []string{
			"devatlas CLI: version, serve, add, list, remove",
			"loopback-only HTTP server with configurable port and graceful shutdown",
			"GET /health JSON endpoint",
			"registration of existing directories as checkouts of explicitly named projects, persisted in a YAML file under the user configuration directory",
			"GET /api/projects (read-only) and the registration table on this page, reloaded from the file on every request",
			"read-only Git observation per checkout, collected for each request by running the local git command: worktree root, branch or detached HEAD, commit, change counts, sanitised remotes",
		},
		Deferred: []string{
			"SQLite index and observation history",
			"project-registry.yaml manifest loading and directory scanning",
			"ahead/behind information against remote refs",
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

type homeData struct {
	Version  version.Info
	Scope    Scope
	BindAddr string
	Now      time.Time
	// Registrations is nil when loading failed; RegistryError then explains why.
	Registrations *ProjectsResponse
	RegistryError string
	RegistryFile  string
}

func (s *Server) handleHome(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	data := homeData{
		Version:      version.Current(),
		Scope:        CurrentScope(),
		BindAddr:     s.cfg.Addr(),
		Now:          now.UTC(),
		RegistryFile: s.registry.Path(),
	}
	if reg, err := s.registry.Load(); err != nil {
		s.log.Warn(registryLoadError)
		data.RegistryError = registryLoadError
	} else {
		resp := ObserveProjects(r.Context(), reg.Views(), s.registry.Path(), now, s.git)
		data.Registrations = &resp
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if data.RegistryError != "" {
		w.WriteHeader(http.StatusInternalServerError)
	}
	if err := homeTemplate.Execute(w, data); err != nil {
		s.log.Warn("render home page", "err", err)
	}
}
