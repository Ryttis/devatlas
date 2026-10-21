// Package registry holds the in-memory registration model of DevAtlas: logical
// projects and the local checkouts (repositories or worktrees) that belong to
// them. It performs validation and identity rules only; persistence lives in
// package registryfile and runtime observation is not implemented yet.
package registry

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// SchemaVersion is the version of the registration model. It is written to
// the registration file and checked on load.
const SchemaVersion = 1

// Project is a logical system. One project may have several checkouts; it is
// identified by an explicit, user-chosen ID, never by a directory name or a
// Git remote.
type Project struct {
	ID           string
	Name         string
	RegisteredAt time.Time
}

// Checkout is one local directory that holds a copy of a project: a clone, a
// Git worktree, or a plain directory. Its path is canonical (absolute, with
// symlinks resolved) so the same directory can be registered only once.
type Checkout struct {
	ID           string
	ProjectID    string
	Path         string
	RegisteredAt time.Time
}

// Registry is the complete set of registrations. It is a plain value with no
// locking; package registryfile serialises concurrent mutations.
type Registry struct {
	Projects  []Project
	Checkouts []Checkout
}

// Errors returned by Registry operations. Callers use errors.Is to map them
// to exit codes or HTTP responses.
var (
	ErrDuplicatePath       = errors.New("directory is already registered")
	ErrUnknownCheckout     = errors.New("unknown checkout ID")
	ErrProjectNameMismatch = errors.New("project already exists with a different name")
	ErrProjectNameRequired = errors.New("a project name is required for a new project")
	ErrInvalidProjectID    = errors.New("invalid project ID")
	ErrInvalidProjectName  = errors.New("invalid project name")
	ErrInvalidPath         = errors.New("invalid checkout path")
	ErrInvalid             = errors.New("invalid registry")
)

var projectIDPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,62}[a-z0-9])?$`)

// ValidateProjectID enforces the project ID format: 1-64 characters of
// lowercase letters, digits, and hyphens, not starting or ending with a
// hyphen. IDs appear in URLs, file names, and CLI arguments, so they are
// deliberately restrictive.
func ValidateProjectID(id string) error {
	if !projectIDPattern.MatchString(id) {
		return fmt.Errorf("%w %q: use 1-64 lowercase letters, digits, or hyphens, not starting or ending with a hyphen", ErrInvalidProjectID, id)
	}
	return nil
}

// MaxProjectNameLength bounds the display name.
const MaxProjectNameLength = 200

// ValidateProjectName enforces a non-empty, single-line display name.
func ValidateProjectName(name string) error {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return fmt.Errorf("%w: name is empty", ErrInvalidProjectName)
	}
	if trimmed != name {
		return fmt.Errorf("%w: name has leading or trailing whitespace", ErrInvalidProjectName)
	}
	if len(name) > MaxProjectNameLength {
		return fmt.Errorf("%w: name is longer than %d bytes", ErrInvalidProjectName, MaxProjectNameLength)
	}
	if strings.ContainsAny(name, "\r\n\t") {
		return fmt.Errorf("%w: name contains control characters", ErrInvalidProjectName)
	}
	return nil
}

// CanonicalDir resolves path to an absolute directory path with symlinks
// evaluated, and checks that it exists and is a directory. Two spellings of
// the same directory (relative, via a symlink, with a trailing slash) yield
// the same result, which is what makes duplicate detection reliable.
func CanonicalDir(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", fmt.Errorf("%w: path is empty", ErrInvalidPath)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrInvalidPath, err)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("%w: %s does not exist", ErrInvalidPath, abs)
		}
		return "", fmt.Errorf("%w: %v", ErrInvalidPath, err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrInvalidPath, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%w: %s is not a directory", ErrInvalidPath, resolved)
	}
	return filepath.Clean(resolved), nil
}

// CheckoutID derives the stable checkout ID for a canonical path: the first
// 12 hex characters of the SHA-256 of the path. The same directory always
// gets the same ID, across restarts and reinstalls; a moved directory gets a
// new one.
func CheckoutID(canonicalPath string) string {
	sum := sha256.Sum256([]byte(canonicalPath))
	return hex.EncodeToString(sum[:])[:12]
}

// AddRequest describes a registration to perform.
type AddRequest struct {
	ProjectID string
	// Name is required when ProjectID is new. When the project already
	// exists it must be empty or equal to the stored name.
	Name string
	// Path is the directory to register; it is canonicalised by Add.
	Path string
	Now  time.Time
}

// AddResult reports what Add did.
type AddResult struct {
	Project        Project
	Checkout       Checkout
	ProjectCreated bool
}

// Add registers a directory as a checkout of a project, creating the project
// when it does not exist yet. Rules:
//
//   - The directory must exist. It is stored by its canonical path, and a
//     directory already registered under any project is rejected with
//     ErrDuplicatePath; the existing checkout is named in the error.
//   - Distinct directories are distinct checkouts even for the same project,
//     which is how Git worktrees of one repository are kept separate.
//   - A new project needs a name. An existing project's name is kept; passing
//     a different name is an error rather than a silent rename.
//
// Add never touches the directory's contents.
func (r *Registry) Add(req AddRequest) (AddResult, error) {
	var res AddResult
	if err := ValidateProjectID(req.ProjectID); err != nil {
		return res, err
	}
	canonical, err := CanonicalDir(req.Path)
	if err != nil {
		return res, err
	}
	if existing, ok := r.CheckoutByPath(canonical); ok {
		return res, fmt.Errorf("%w: %s is checkout %s of project %s", ErrDuplicatePath, canonical, existing.ID, existing.ProjectID)
	}
	now := req.Now
	if now.IsZero() {
		now = time.Now()
	}
	now = now.UTC().Truncate(time.Second)

	idx := r.projectIndex(req.ProjectID)
	if idx < 0 {
		if req.Name == "" {
			return res, fmt.Errorf("%w: project %q does not exist yet", ErrProjectNameRequired, req.ProjectID)
		}
		if err := ValidateProjectName(req.Name); err != nil {
			return res, err
		}
		r.Projects = append(r.Projects, Project{ID: req.ProjectID, Name: req.Name, RegisteredAt: now})
		idx = len(r.Projects) - 1
		res.ProjectCreated = true
	} else if req.Name != "" && req.Name != r.Projects[idx].Name {
		return res, fmt.Errorf("%w: project %s is named %q, not %q", ErrProjectNameMismatch, req.ProjectID, r.Projects[idx].Name, req.Name)
	}

	ck := Checkout{ID: CheckoutID(canonical), ProjectID: req.ProjectID, Path: canonical, RegisteredAt: now}
	r.Checkouts = append(r.Checkouts, ck)
	r.sort()
	res.Project = r.Projects[r.projectIndex(req.ProjectID)]
	res.Checkout = ck
	return res, nil
}

// RemoveResult reports what Remove did.
type RemoveResult struct {
	Checkout Checkout
	// ProjectRemoved is true when the checkout was the last one of its
	// project, in which case the project record was removed as well.
	ProjectRemoved bool
	Project        Project
}

// Remove deletes a checkout registration by ID. An unknown ID is an error and
// leaves the registry unchanged. When the removed checkout was the last one
// of its project, the project record is removed too: in this version a
// project holds nothing but its ID and name, which are supplied again on the
// next Add, so an empty project would only be clutter. Remove changes the
// registry only; it never touches the directory, its files, or any
// containers.
func (r *Registry) Remove(checkoutID string) (RemoveResult, error) {
	var res RemoveResult
	idx := -1
	for i, c := range r.Checkouts {
		if c.ID == checkoutID {
			idx = i
			break
		}
	}
	if idx < 0 {
		return res, fmt.Errorf("%w: %q", ErrUnknownCheckout, checkoutID)
	}
	res.Checkout = r.Checkouts[idx]
	r.Checkouts = append(r.Checkouts[:idx:idx], r.Checkouts[idx+1:]...)

	if len(r.checkoutsOf(res.Checkout.ProjectID)) == 0 {
		pi := r.projectIndex(res.Checkout.ProjectID)
		if pi >= 0 {
			res.Project = r.Projects[pi]
			res.ProjectRemoved = true
			r.Projects = append(r.Projects[:pi:pi], r.Projects[pi+1:]...)
		}
	}
	return res, nil
}

// CheckoutByPath finds a checkout by canonical path.
func (r *Registry) CheckoutByPath(canonical string) (Checkout, bool) {
	for _, c := range r.Checkouts {
		if c.Path == canonical {
			return c, true
		}
	}
	return Checkout{}, false
}

// Validate checks internal consistency: unique and well-formed IDs, every
// checkout referencing an existing project, unique canonical paths. It is
// run after loading a file so that a hand-edited or corrupted registry is
// reported instead of being used or silently rewritten.
func (r *Registry) Validate() error {
	projects := make(map[string]bool, len(r.Projects))
	for i, p := range r.Projects {
		if err := ValidateProjectID(p.ID); err != nil {
			return fmt.Errorf("%w: projects[%d]: %v", ErrInvalid, i, err)
		}
		if err := ValidateProjectName(p.Name); err != nil {
			return fmt.Errorf("%w: project %s: %v", ErrInvalid, p.ID, err)
		}
		if projects[p.ID] {
			return fmt.Errorf("%w: project ID %q appears more than once", ErrInvalid, p.ID)
		}
		projects[p.ID] = true
	}
	ids := make(map[string]bool, len(r.Checkouts))
	paths := make(map[string]bool, len(r.Checkouts))
	for i, c := range r.Checkouts {
		if c.ID == "" {
			return fmt.Errorf("%w: checkouts[%d] has an empty ID", ErrInvalid, i)
		}
		if ids[c.ID] {
			return fmt.Errorf("%w: checkout ID %q appears more than once", ErrInvalid, c.ID)
		}
		ids[c.ID] = true
		if !projects[c.ProjectID] {
			return fmt.Errorf("%w: checkout %s references unknown project %q", ErrInvalid, c.ID, c.ProjectID)
		}
		if c.Path == "" || !filepath.IsAbs(c.Path) {
			return fmt.Errorf("%w: checkout %s has a non-absolute path %q", ErrInvalid, c.ID, c.Path)
		}
		if paths[c.Path] {
			return fmt.Errorf("%w: path %q appears more than once", ErrInvalid, c.Path)
		}
		paths[c.Path] = true
	}
	return nil
}

// ProjectView is a project with its checkouts, the shape shared by the CLI
// list output, the JSON API, and the HTML page.
type ProjectView struct {
	Project
	Checkouts []Checkout
}

// Views returns all projects with their checkouts, sorted by project ID and
// then by checkout path.
func (r *Registry) Views() []ProjectView {
	r.sort()
	views := make([]ProjectView, 0, len(r.Projects))
	for _, p := range r.Projects {
		views = append(views, ProjectView{Project: p, Checkouts: r.checkoutsOf(p.ID)})
	}
	return views
}

func (r *Registry) projectIndex(id string) int {
	for i, p := range r.Projects {
		if p.ID == id {
			return i
		}
	}
	return -1
}

func (r *Registry) checkoutsOf(projectID string) []Checkout {
	var out []Checkout
	for _, c := range r.Checkouts {
		if c.ProjectID == projectID {
			out = append(out, c)
		}
	}
	return out
}

func (r *Registry) sort() {
	sort.Slice(r.Projects, func(i, j int) bool { return r.Projects[i].ID < r.Projects[j].ID })
	sort.Slice(r.Checkouts, func(i, j int) bool {
		if r.Checkouts[i].ProjectID != r.Checkouts[j].ProjectID {
			return r.Checkouts[i].ProjectID < r.Checkouts[j].ProjectID
		}
		return r.Checkouts[i].Path < r.Checkouts[j].Path
	})
}
