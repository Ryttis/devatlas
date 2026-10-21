package registryfile

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Ryttis/devatlas/internal/registry"
)

func newTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "cfg", "devatlas", "registrations.yaml") // nested: directory must be created
	return NewStore(path), dir
}

func add(t *testing.T, s *Store, project, name, dir string) registry.AddResult {
	t.Helper()
	var res registry.AddResult
	err := s.Update(func(reg *registry.Registry) error {
		var err error
		res, err = reg.Add(registry.AddRequest{ProjectID: project, Name: name, Path: dir})
		return err
	})
	if err != nil {
		t.Fatalf("add %s: %v", dir, err)
	}
	return res
}

func TestMissingFileIsEmptyRegistryAndNotCreatedByLoad(t *testing.T) {
	s, _ := newTestStore(t)
	reg, err := s.Load()
	if err != nil || len(reg.Projects) != 0 {
		t.Fatalf("Load: %v %+v", err, reg)
	}
	if _, err := os.Stat(s.Path()); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("Load must not create the file")
	}
}

func TestPersistsAcrossReload(t *testing.T) {
	s, root := newTestStore(t)
	d1, d2 := filepath.Join(root, "one"), filepath.Join(root, "two")
	os.Mkdir(d1, 0o755)
	os.Mkdir(d2, 0o755)
	r1 := add(t, s, "demo", "Demo", d1)
	add(t, s, "demo", "", d2)

	// A brand-new store (as after a restart) sees the same data.
	again := NewStore(s.Path())
	reg, err := again.Load()
	if err != nil {
		t.Fatal(err)
	}
	views := reg.Views()
	if len(views) != 1 || views[0].ID != "demo" || views[0].Name != "Demo" || len(views[0].Checkouts) != 2 {
		t.Fatalf("unexpected reload: %+v", views)
	}
	if views[0].Checkouts[0].ID != r1.Checkout.ID || views[0].RegisteredAt.IsZero() || views[0].Checkouts[0].RegisteredAt.IsZero() {
		t.Fatalf("ids or timestamps lost: %+v", views[0])
	}

	// File content is the documented format.
	data, err := os.ReadFile(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"schema_version: 1", "projects:", "checkouts:", "id: demo", "name: Demo", "project_id: demo", "path: " + views[0].Checkouts[0].Path, "registered_at:"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("file missing %q:\n%s", want, data)
		}
	}
	info, _ := os.Stat(s.Path())
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("file mode = %o, want 600", perm)
	}
	dinfo, _ := os.Stat(filepath.Dir(s.Path()))
	if perm := dinfo.Mode().Perm(); perm != 0o700 {
		t.Errorf("dir mode = %o, want 700", perm)
	}

	// Remove persists too, and no temp files linger.
	if err := s.Update(func(reg *registry.Registry) error { _, err := reg.Remove(r1.Checkout.ID); return err }); err != nil {
		t.Fatal(err)
	}
	reg, _ = NewStore(s.Path()).Load()
	if len(reg.Checkouts) != 1 {
		t.Fatalf("remove not persisted: %+v", reg)
	}
	entries, _ := os.ReadDir(filepath.Dir(s.Path()))
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Errorf("temporary file left behind: %s", e.Name())
		}
	}
}

func TestDuplicateAcrossProcessesIsRejected(t *testing.T) {
	s, root := newTestStore(t)
	d := filepath.Join(root, "one")
	os.Mkdir(d, 0o755)
	add(t, s, "demo", "Demo", d)
	err := NewStore(s.Path()).Update(func(reg *registry.Registry) error {
		_, err := reg.Add(registry.AddRequest{ProjectID: "other", Name: "Other", Path: d})
		return err
	})
	if !errors.Is(err, registry.ErrDuplicatePath) {
		t.Fatalf("err = %v", err)
	}
}

func TestMalformedFileIsReportedAndNeverRewritten(t *testing.T) {
	cases := map[string]string{
		"yaml syntax":    "schema_version: 1\nprojects: [\n",
		"unknown field":  "schema_version: 1\nprojects: []\ncheckouts: []\nbogus: 1\n",
		"orphan":         "schema_version: 1\nprojects: []\ncheckouts:\n  - id: abc\n    project_id: ghost\n    path: /x\n",
		"bad project id": "schema_version: 1\nprojects:\n  - id: Bad Id\n    name: X\ncheckouts: []\n",
		"wrong type":     "schema_version: one\n",
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			s, _ := newTestStore(t)
			os.MkdirAll(filepath.Dir(s.Path()), 0o700)
			if err := os.WriteFile(s.Path(), []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Load(); !errors.Is(err, ErrMalformed) || !strings.Contains(err.Error(), s.Path()) {
				t.Fatalf("Load err = %v, want ErrMalformed naming the file", err)
			}
			dir := t.TempDir()
			err := s.Update(func(reg *registry.Registry) error {
				_, err := reg.Add(registry.AddRequest{ProjectID: "demo", Name: "Demo", Path: dir})
				return err
			})
			if !errors.Is(err, ErrMalformed) {
				t.Fatalf("Update err = %v, want ErrMalformed", err)
			}
			after, _ := os.ReadFile(s.Path())
			if string(after) != content {
				t.Fatalf("file was rewritten:\n%s", after)
			}
		})
	}
}

func TestUnsupportedSchemaVersion(t *testing.T) {
	s, _ := newTestStore(t)
	os.MkdirAll(filepath.Dir(s.Path()), 0o700)
	os.WriteFile(s.Path(), []byte("schema_version: 2\nprojects: []\ncheckouts: []\n"), 0o600)
	if _, err := s.Load(); !errors.Is(err, ErrUnsupportedSchema) {
		t.Fatalf("err = %v", err)
	}
}

func TestConcurrentUpdatesAreNotLost(t *testing.T) {
	s, root := newTestStore(t)
	const n = 20
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		dir := filepath.Join(root, fmt.Sprintf("repo-%02d", i))
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		wg.Add(1)
		go func(i int, dir string) {
			defer wg.Done()
			// Each goroutine uses its own Store, like separate CLI processes.
			errs <- NewStore(s.Path()).Update(func(reg *registry.Registry) error {
				_, err := reg.Add(registry.AddRequest{ProjectID: fmt.Sprintf("p%d", i%3), Name: fmt.Sprintf("P%d", i%3), Path: dir})
				return err
			})
		}(i, dir)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent update failed: %v", err)
		}
	}
	reg, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(reg.Checkouts) != n || len(reg.Projects) != 3 {
		t.Fatalf("lost updates: %d checkouts, %d projects", len(reg.Checkouts), len(reg.Projects))
	}
}

func TestResolvePathPrecedence(t *testing.T) {
	t.Setenv(EnvConfigPath, "")
	def, err := ResolvePath("")
	if err != nil || !strings.HasSuffix(def, filepath.Join(DirName, FileName)) {
		t.Fatalf("default: %q %v", def, err)
	}
	t.Setenv(EnvConfigPath, "env.yaml")
	got, _ := ResolvePath("")
	if !filepath.IsAbs(got) || filepath.Base(got) != "env.yaml" {
		t.Fatalf("env: %q", got)
	}
	got, _ = ResolvePath("flag.yaml")
	if filepath.Base(got) != "flag.yaml" {
		t.Fatalf("flag must win: %q", got)
	}
}
