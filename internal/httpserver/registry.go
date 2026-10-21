package httpserver

import (
	"encoding/json"
	"net/http"
	"os"
	"time"

	"github.com/Ryttis/devatlas/internal/registry"
)

// RegistrySource provides the current registrations. The file store
// implements it; the server reloads on every request so CLI changes are
// visible without a restart.
type RegistrySource interface {
	Load() (*registry.Registry, error)
	Path() string
}

// Status vocabulary from the brief. Only "unknown" is produced in this build
// because no runtime collection exists yet.
const (
	StatusUnknown = "unknown"

	// NotCollectedReason explains every "unknown" status in this build.
	NotCollectedReason = "not collected: runtime status collection (Git, Docker, ports, HTTP checks) is not implemented in this build"
)

// ProjectsResponse is the body of GET /api/projects.
type ProjectsResponse struct {
	SchemaVersion int          `json:"schema_version"`
	GeneratedAt   time.Time    `json:"generated_at"`
	RegistryFile  string       `json:"registry_file"`
	RuntimeStatus StatusNote   `json:"runtime_status"`
	Projects      []ProjectDTO `json:"projects"`
}

// StatusNote states a status together with why it was assigned.
type StatusNote struct {
	Status string `json:"status"`
	Reason string `json:"reason"`
}

// ProjectDTO is a project with its checkouts.
type ProjectDTO struct {
	ID           string        `json:"id"`
	Name         string        `json:"name"`
	RegisteredAt time.Time     `json:"registered_at"`
	Checkouts    []CheckoutDTO `json:"checkouts"`
}

// CheckoutDTO is one registered directory. PathExists is the only observed
// fact in this build: a stat of the directory at GeneratedAt.
type CheckoutDTO struct {
	ID           string     `json:"id"`
	Path         string     `json:"path"`
	RegisteredAt time.Time  `json:"registered_at"`
	PathExists   bool       `json:"path_exists"`
	CheckedAt    time.Time  `json:"checked_at"`
	Runtime      StatusNote `json:"runtime"`
}

// BuildProjectsResponse converts registry views into the API shape. It is
// shared by the JSON endpoint, the HTML page, and `devatlas list -json`.
func BuildProjectsResponse(views []registry.ProjectView, registryFile string, now time.Time) ProjectsResponse {
	now = now.UTC().Truncate(time.Second)
	resp := ProjectsResponse{
		SchemaVersion: registry.SchemaVersion,
		GeneratedAt:   now,
		RegistryFile:  registryFile,
		RuntimeStatus: StatusNote{Status: StatusUnknown, Reason: NotCollectedReason},
		Projects:      []ProjectDTO{},
	}
	for _, v := range views {
		p := ProjectDTO{ID: v.ID, Name: v.Name, RegisteredAt: v.RegisteredAt, Checkouts: []CheckoutDTO{}}
		for _, c := range v.Checkouts {
			p.Checkouts = append(p.Checkouts, CheckoutDTO{
				ID:           c.ID,
				Path:         c.Path,
				RegisteredAt: c.RegisteredAt,
				PathExists:   dirExists(c.Path),
				CheckedAt:    now,
				Runtime:      StatusNote{Status: StatusUnknown, Reason: NotCollectedReason},
			})
		}
		resp.Projects = append(resp.Projects, p)
	}
	return resp
}

func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

type errorResponse struct {
	Error string `json:"error"`
}

func (s *Server) handleProjects(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")

	reg, err := s.registry.Load()
	if err != nil {
		s.log.Warn("load registrations", "err", err)
		w.WriteHeader(http.StatusInternalServerError)
		_ = enc.Encode(errorResponse{Error: "cannot load registrations: " + err.Error()})
		return
	}
	resp := BuildProjectsResponse(reg.Views(), s.registry.Path(), time.Now())
	if err := enc.Encode(resp); err != nil {
		s.log.Warn("write projects response", "err", err)
	}
}
