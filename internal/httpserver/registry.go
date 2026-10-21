package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"time"

	"github.com/Ryttis/devatlas/internal/collect/git"
	"github.com/Ryttis/devatlas/internal/registry"
)

// RegistrySource provides the current registrations. The file store
// implements it; the server reloads on every request so CLI changes are
// visible without a restart.
type RegistrySource interface {
	Load() (*registry.Registry, error)
	Path() string
}

// GitObserver collects Git observations for checkout paths. The real
// implementation is git.Collector; tests substitute fakes.
type GitObserver interface {
	CollectAll(ctx context.Context, paths []string) map[string]git.Observation
}

// Status vocabulary from the brief. Only "unknown" is produced for runtime
// status in this build because no Docker, port, or HTTP collection exists.
const (
	StatusUnknown = "unknown"

	// NotCollectedReason explains every "unknown" runtime status in this
	// build. A successful Git observation says nothing about containers or
	// application health.
	NotCollectedReason = "not collected: runtime status collection (Docker, ports, HTTP checks) is not implemented in this build; Git state does not imply runtime health"

	// CollectionModeCurrentRequest marks observations as produced for the
	// response that carries them.
	CollectionModeCurrentRequest = "current_request"
	collectionNote               = "Git observations were collected by running git locally while producing this response; nothing is cached, stored, or refreshed in the background"
)

// ProjectsResponse is the body of GET /api/projects.
type ProjectsResponse struct {
	SchemaVersion int            `json:"schema_version"`
	GeneratedAt   time.Time      `json:"generated_at"`
	RegistryFile  string         `json:"registry_file"`
	Collection    CollectionNote `json:"collection"`
	RuntimeStatus StatusNote     `json:"runtime_status"`
	Projects      []ProjectDTO   `json:"projects"`
}

// CollectionNote states how the observations in a response were obtained.
type CollectionNote struct {
	Mode string `json:"mode"`
	Note string `json:"note"`
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

// CheckoutDTO is one registered directory with the facts observed for this
// response: a stat of the directory (PathExists is nil when stat fails;
// PathStatus distinguishes absence, a non-directory, and errors) and a Git
// observation. Runtime stays unknown regardless of the Git result.
type CheckoutDTO struct {
	ID           string          `json:"id"`
	Path         string          `json:"path"`
	RegisteredAt time.Time       `json:"registered_at"`
	PathExists   *bool           `json:"path_exists"`
	PathStatus   string          `json:"path_status"`
	CheckedAt    time.Time       `json:"checked_at"`
	Git          git.Observation `json:"git"`
	Runtime      StatusNote      `json:"runtime"`
}

// ObserveProjects collects Git observations for every checkout in views and
// builds the response. ctx bounds the collection: cancelling it (for example
// when the HTTP client disconnects) stops running git commands. It is shared
// by the JSON endpoint, the HTML page, and `devatlas list`.
func ObserveProjects(ctx context.Context, views []registry.ProjectView, registryFile string, now time.Time, observer GitObserver) ProjectsResponse {
	var paths []string
	for _, v := range views {
		for _, c := range v.Checkouts {
			paths = append(paths, c.Path)
		}
	}
	var observations map[string]git.Observation
	if observer != nil && len(paths) > 0 {
		observations = observer.CollectAll(ctx, paths)
	}
	return BuildProjectsResponse(views, registryFile, now, observations)
}

// BuildProjectsResponse converts registry views and already collected Git
// observations (keyed by checkout path) into the API shape. A checkout
// without an observation is reported as not observed.
func BuildProjectsResponse(views []registry.ProjectView, registryFile string, now time.Time, observations map[string]git.Observation) ProjectsResponse {
	now = now.UTC().Truncate(time.Second)
	resp := ProjectsResponse{
		SchemaVersion: registry.SchemaVersion,
		GeneratedAt:   now,
		RegistryFile:  registryFile,
		Collection:    CollectionNote{Mode: CollectionModeCurrentRequest, Note: collectionNote},
		RuntimeStatus: StatusNote{Status: StatusUnknown, Reason: NotCollectedReason},
		Projects:      []ProjectDTO{},
	}
	for _, v := range views {
		p := ProjectDTO{ID: v.ID, Name: v.Name, RegisteredAt: v.RegisteredAt, Checkouts: []CheckoutDTO{}}
		for _, c := range v.Checkouts {
			exists, status := observeDir(c.Path)
			obs, ok := observations[c.Path]
			if !ok {
				obs = git.Observation{Status: git.StatusError, Source: git.Source, CollectedAt: now, Reason: "not observed"}
			}
			p.Checkouts = append(p.Checkouts, CheckoutDTO{
				ID:           c.ID,
				Path:         c.Path,
				RegisteredAt: c.RegisteredAt,
				PathExists:   exists,
				PathStatus:   status,
				CheckedAt:    now,
				Git:          obs,
				Runtime:      StatusNote{Status: StatusUnknown, Reason: NotCollectedReason},
			})
		}
		resp.Projects = append(resp.Projects, p)
	}
	return resp
}

// observeDir distinguishes absence from a failed observation. No error text
// from the filesystem is needed in the public response.
func observeDir(path string) (*bool, string) {
	info, err := os.Stat(path)
	exists := false
	if errors.Is(err, os.ErrNotExist) {
		return &exists, "missing"
	}
	if err != nil {
		return nil, "error"
	}
	if !info.IsDir() {
		return &exists, "not a directory"
	}
	exists = true
	return &exists, "present"
}

const registryLoadError = "cannot load registrations: configuration is invalid or unreadable; inspect the registration file locally"

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
		s.log.Warn(registryLoadError)
		w.WriteHeader(http.StatusInternalServerError)
		_ = enc.Encode(errorResponse{Error: registryLoadError})
		return
	}
	resp := ObserveProjects(r.Context(), reg.Views(), s.registry.Path(), time.Now(), s.git)
	if err := enc.Encode(resp); err != nil {
		s.log.Warn("write projects response", "err", err)
	}
}
