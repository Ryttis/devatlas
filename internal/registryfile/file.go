// Package registryfile persists the registration model as a versioned YAML
// file in the user's configuration directory, outside every inventoried
// repository. Writes are atomic (temp file + rename) and serialised across
// processes with a lock file, so concurrent `devatlas add` invocations cannot
// lose each other's updates. A malformed file is reported and never rewritten.
package registryfile

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/Ryttis/devatlas/internal/registry"
)

// File name and location defaults.
const (
	DirName  = "devatlas"
	FileName = "registrations.yaml"

	// EnvConfigPath overrides the registration file path when the -config
	// flag is not given.
	EnvConfigPath = "DEVATLAS_CONFIG"

	lockTimeout = 10 * time.Second
)

// ErrMalformed wraps every parse or validation failure of the file. The file
// is left untouched in that case.
var ErrMalformed = errors.New("malformed registration file")

// ErrUnsupportedSchema is returned when the file declares a schema version
// this build does not understand.
var ErrUnsupportedSchema = errors.New("unsupported registration file schema version")

// DefaultPath returns the registration file path without overrides:
// <user config dir>/devatlas/registrations.yaml, where the user config dir
// is $XDG_CONFIG_HOME or ~/.config on Linux and WSL.
func DefaultPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("determine user configuration directory: %w", err)
	}
	return filepath.Join(dir, DirName, FileName), nil
}

// ResolvePath picks the registration file path: the explicit flag value,
// else the DEVATLAS_CONFIG environment variable, else DefaultPath. The
// result is made absolute so log output and errors are unambiguous.
func ResolvePath(flagValue string) (string, error) {
	p := flagValue
	if p == "" {
		p = os.Getenv(EnvConfigPath)
	}
	if p == "" {
		var err error
		p, err = DefaultPath()
		if err != nil {
			return "", err
		}
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", fmt.Errorf("resolve registration file path %q: %w", p, err)
	}
	return configPath(abs)
}

// Resolve directory aliases even before the configuration directory exists.
// File aliases are rejected: rename would detach a symlink or hard link from
// its target, splitting one apparent registry into independently locked files.
func configPath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	dir, err := resolveDir(filepath.Dir(abs))
	if err != nil {
		return "", fmt.Errorf("resolve configuration directory: %w", err)
	}
	path = filepath.Join(dir, filepath.Base(abs))
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return path, nil
	}
	if err != nil {
		return "", err
	}
	if err := regularFile(info); err != nil {
		return "", fmt.Errorf("registration file %s: %w", path, err)
	}
	return path, nil
}

func resolveDir(path string) (string, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err == nil {
		return resolved, nil
	}
	// Only recurse through absent components, never through a dangling link
	// or an inaccessible component (which must remain an error).
	_, statErr := os.Lstat(path)
	if statErr == nil {
		// The component appeared between the two calls, typically because a
		// concurrent devatlas process created the configuration directory.
		// Resolve again instead of reporting the stale absence.
		return filepath.EvalSymlinks(path)
	}
	if !errors.Is(statErr, os.ErrNotExist) {
		return "", err
	}
	parent := filepath.Dir(path)
	if parent == path {
		return "", err
	}
	resolved, err = resolveDir(parent)
	if err != nil {
		return "", err
	}
	return filepath.Join(resolved, filepath.Base(path)), nil
}

// Store reads and writes one registration file.
type Store struct {
	path string
	err  error
}

// NewStore resolves the configuration path and checks file metadata. No file
// contents are read and nothing is created until Load or Update is called.
// Resolution errors are returned by Load and Update.
func NewStore(path string) *Store {
	resolved, err := configPath(path)
	if err != nil {
		return &Store{path: path, err: err}
	}
	return &Store{path: resolved}
}

// Path returns the registration file path.
func (s *Store) Path() string { return s.path }

// fileDoc is the on-disk document. Field names are the file format; keep
// them stable and bump SchemaVersion for incompatible changes.
type fileDoc struct {
	SchemaVersion int            `yaml:"schema_version"`
	Projects      []fileProject  `yaml:"projects"`
	Checkouts     []fileCheckout `yaml:"checkouts"`
}

type fileProject struct {
	ID           string    `yaml:"id"`
	Name         string    `yaml:"name"`
	RegisteredAt time.Time `yaml:"registered_at"`
}

type fileCheckout struct {
	ID           string    `yaml:"id"`
	ProjectID    string    `yaml:"project_id"`
	Path         string    `yaml:"path"`
	RegisteredAt time.Time `yaml:"registered_at"`
}

const fileHeader = `# DevAtlas registration file. Managed by "devatlas add" and "devatlas remove".
# Paths are machine-specific; keep this file out of version control.
`

// Load reads the file. A missing file yields an empty registry; a file that
// cannot be parsed or fails validation yields an error wrapping ErrMalformed
// and is never modified.
func (s *Store) Load() (*registry.Registry, error) {
	if s.err != nil {
		return nil, s.err
	}
	path, err := configPath(s.path)
	if err != nil {
		return nil, err
	}
	if path != s.path {
		return nil, errors.New("configuration directory changed; reopen the store")
	}
	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return &registry.Registry{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read registration file %s: %w", s.path, err)
	}
	return decode(s.path, data)
}

func decode(path string, data []byte) (*registry.Registry, error) {
	var doc fileDoc
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("%w %s: invalid YAML or field type", ErrMalformed, path)
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); err != io.EOF {
		return nil, fmt.Errorf("%w %s: expected exactly one YAML document", ErrMalformed, path)
	}
	if doc.SchemaVersion != registry.SchemaVersion {
		return nil, fmt.Errorf("%w: %s declares schema_version %d, this build supports %d", ErrUnsupportedSchema, path, doc.SchemaVersion, registry.SchemaVersion)
	}
	reg := &registry.Registry{}
	for _, p := range doc.Projects {
		reg.Projects = append(reg.Projects, registry.Project{ID: p.ID, Name: p.Name, RegisteredAt: p.RegisteredAt})
	}
	for _, c := range doc.Checkouts {
		reg.Checkouts = append(reg.Checkouts, registry.Checkout{ID: c.ID, ProjectID: c.ProjectID, Path: c.Path, RegisteredAt: c.RegisteredAt})
	}
	if err := reg.Validate(); err != nil {
		return nil, fmt.Errorf("%w %s: inconsistent registration model", ErrMalformed, path)
	}
	return reg, nil
}

func encode(reg *registry.Registry) ([]byte, error) {
	doc := fileDoc{SchemaVersion: registry.SchemaVersion, Projects: []fileProject{}, Checkouts: []fileCheckout{}}
	for _, p := range reg.Projects {
		doc.Projects = append(doc.Projects, fileProject{ID: p.ID, Name: p.Name, RegisteredAt: p.RegisteredAt.UTC()})
	}
	for _, c := range reg.Checkouts {
		doc.Checkouts = append(doc.Checkouts, fileCheckout{ID: c.ID, ProjectID: c.ProjectID, Path: c.Path, RegisteredAt: c.RegisteredAt.UTC()})
	}
	var buf bytes.Buffer
	buf.WriteString(fileHeader)
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		return nil, fmt.Errorf("encode registrations: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("encode registrations: %w", err)
	}
	return buf.Bytes(), nil
}

// Update applies fn to the current registry under an exclusive lock and
// writes the result atomically. If fn returns an error nothing is written.
// The read-modify-write runs entirely inside the lock, so two concurrent
// updates are applied one after the other instead of the last one winning.
func (s *Store) Update(fn func(reg *registry.Registry) error) error {
	if s.err != nil {
		return s.err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return fmt.Errorf("create configuration directory: %w", err)
	}
	unlock, err := lock(s.path+".lock", lockTimeout)
	if err != nil {
		return err
	}
	defer unlock()

	reg, err := s.Load()
	if err != nil {
		return err
	}
	if err := fn(reg); err != nil {
		return err
	}
	if err := reg.Validate(); err != nil {
		return fmt.Errorf("refusing to write an invalid registry: %w", err)
	}
	data, err := encode(reg)
	if err != nil {
		return err
	}
	return writeAtomic(s.path, data)
}

// writeAtomic writes data to a temporary file in the target directory, syncs
// it, and renames it over the target so readers see either the old or the
// new complete file, never a partial one.
func writeAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temporary file: %w", err)
	}
	tmpPath := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpPath) }
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		cleanup()
		return fmt.Errorf("set permissions on temporary file: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		cleanup()
		return fmt.Errorf("write temporary file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		cleanup()
		return fmt.Errorf("sync temporary file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("close temporary file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		cleanup()
		return fmt.Errorf("replace registration file: %w", err)
	}
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("registration replaced, but open directory for sync failed: %w", err)
	}
	syncErr := d.Sync()
	closeErr := d.Close()
	if err := errors.Join(syncErr, closeErr); err != nil {
		return fmt.Errorf("registration replaced, but directory durability check failed: %w", err)
	}
	return nil
}
