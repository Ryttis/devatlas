package assess

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/Ryttis/devatlas/internal/atomicfile"
	"github.com/Ryttis/devatlas/internal/registry"
)

// DirName is the directory of assessment records next to the registration
// file.
const DirName = "assessments"

const maxRecordBytes = 64 << 20

// Store keeps one JSON record per project, written atomically.
type Store struct{ dir string }

// NewStore returns a store rooted at dir; the directory is created on the
// first save.
func NewStore(dir string) *Store { return &Store{dir: dir} }

// Dir returns the record directory.
func (s *Store) Dir() string { return s.dir }

func (s *Store) path(projectID string) (string, error) {
	if err := registry.ValidateProjectID(projectID); err != nil {
		return "", err
	}
	return filepath.Join(s.dir, projectID+".json"), nil
}

// Load returns the record of a project, or nil when none exists.
func (s *Store) Load(projectID string) (*Record, error) {
	p, err := s.path(projectID)
	if err != nil {
		return nil, err
	}
	info, err := os.Lstat(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("assessment record of %s cannot be read", projectID)
	}
	f, err := os.Open(p)
	if err != nil {
		return nil, fmt.Errorf("assessment record of %s cannot be read", projectID)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxRecordBytes+1))
	if err != nil || len(data) > maxRecordBytes {
		return nil, fmt.Errorf("assessment record of %s cannot be read", projectID)
	}
	var rec Record
	if err := json.Unmarshal(data, &rec); err != nil || rec.SchemaVersion != SchemaVersion || rec.ProjectID != projectID {
		return nil, fmt.Errorf("assessment record of %s is malformed or from another schema version", projectID)
	}
	return &rec, nil
}

// Save writes the record atomically.
func (s *Store) Save(rec *Record) error {
	p, err := s.path(rec.ProjectID)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return fmt.Errorf("create assessment directory: %w", err)
	}
	rec.SchemaVersion = SchemaVersion
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	return atomicfile.Write(p, append(data, '\n'))
}
