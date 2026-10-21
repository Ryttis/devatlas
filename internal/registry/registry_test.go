package registry

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func mustAdd(t *testing.T, r *Registry, project, name, path string) AddResult {
	t.Helper()
	res, err := r.Add(AddRequest{ProjectID: project, Name: name, Path: path, Now: time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatalf("Add(%s, %s, %s): %v", project, name, path, err)
	}
	return res
}

func TestAddCreatesProjectAndCheckout(t *testing.T) {
	dir := t.TempDir()
	var r Registry
	res := mustAdd(t, &r, "demo", "Demo", dir)
	if !res.ProjectCreated || res.Project.Name != "Demo" || res.Checkout.ProjectID != "demo" {
		t.Fatalf("unexpected result: %+v", res)
	}
	canonical, _ := CanonicalDir(dir)
	if res.Checkout.Path != canonical || res.Checkout.ID != CheckoutID(canonical) || len(res.Checkout.ID) != 12 {
		t.Fatalf("unexpected checkout: %+v", res.Checkout)
	}
	if err := r.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestAddRejectsSameDirectorySpelledDifferently(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "repo")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(dir, link); err != nil {
		if runtime.GOOS == "windows" {
			t.Skip("symlinks need privileges on windows")
		}
		t.Fatal(err)
	}
	var r Registry
	first := mustAdd(t, &r, "demo", "Demo", dir)

	spellings := []string{
		dir + string(filepath.Separator),      // trailing separator
		filepath.Join(dir, "..", "repo"),      // via parent
		link,                                  // symlink to the directory
		filepath.Join(link, ".", "sub", ".."), // symlink plus dot segments
	}
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, p := range spellings {
		_, err := r.Add(AddRequest{ProjectID: "demo", Path: p})
		if !errors.Is(err, ErrDuplicatePath) {
			t.Errorf("Add(%q): err = %v, want ErrDuplicatePath", p, err)
		}
		// Also rejected under a different project: a directory belongs to one checkout.
		_, err = r.Add(AddRequest{ProjectID: "other", Name: "Other", Path: p})
		if !errors.Is(err, ErrDuplicatePath) {
			t.Errorf("Add(other, %q): err = %v, want ErrDuplicatePath", p, err)
		}
	}
	if len(r.Checkouts) != 1 || len(r.Projects) != 1 || r.Checkouts[0].ID != first.Checkout.ID {
		t.Fatalf("registry changed by rejected adds: %+v", r)
	}

	// Relative path resolution.
	wd, _ := os.Getwd()
	defer os.Chdir(wd)
	if err := os.Chdir(root); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Add(AddRequest{ProjectID: "demo", Path: "repo"}); !errors.Is(err, ErrDuplicatePath) {
		t.Errorf("relative spelling: err = %v, want ErrDuplicatePath", err)
	}
}

func TestDistinctDirectoriesAreDistinctCheckouts(t *testing.T) {
	root := t.TempDir()
	main := filepath.Join(root, "repo")
	wt := filepath.Join(root, "repo-feature") // a Git worktree of the same repo
	for _, d := range []string{main, wt} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	var r Registry
	a := mustAdd(t, &r, "demo", "Demo", main)
	b := mustAdd(t, &r, "demo", "", wt) // name omitted for an existing project
	if b.ProjectCreated || a.Checkout.ID == b.Checkout.ID {
		t.Fatalf("worktree must be a separate checkout of the same project: %+v %+v", a, b)
	}
	views := r.Views()
	if len(views) != 1 || len(views[0].Checkouts) != 2 {
		t.Fatalf("unexpected views: %+v", views)
	}
	if views[0].Checkouts[0].Path > views[0].Checkouts[1].Path {
		t.Error("checkouts should be sorted by path")
	}
}

func TestAddProjectNameRules(t *testing.T) {
	var r Registry
	dir := t.TempDir()
	if _, err := r.Add(AddRequest{ProjectID: "demo", Path: dir}); !errors.Is(err, ErrProjectNameRequired) {
		t.Errorf("new project without name: %v", err)
	}
	mustAdd(t, &r, "demo", "Demo", dir)
	other := t.TempDir()
	if _, err := r.Add(AddRequest{ProjectID: "demo", Name: "Renamed", Path: other}); !errors.Is(err, ErrProjectNameMismatch) {
		t.Errorf("different name for existing project: %v", err)
	}
	if len(r.Checkouts) != 1 {
		t.Error("rejected add must not register the checkout")
	}
	mustAdd(t, &r, "demo", "Demo", other) // same name is fine
}

func TestValidation(t *testing.T) {
	for _, id := range []string{"", "-a", "a-", "A", "a b", "a/b", "ab_c", "x" + string(make([]byte, 64))} {
		if err := ValidateProjectID(id); err == nil {
			t.Errorf("ValidateProjectID(%q) = nil, want error", id)
		}
	}
	for _, id := range []string{"a", "a1", "carrier-self-service", "x-1-y"} {
		if err := ValidateProjectID(id); err != nil {
			t.Errorf("ValidateProjectID(%q) = %v", id, err)
		}
	}
	for _, name := range []string{"", " x", "x ", "a\nb", string(make([]byte, 201))} {
		if err := ValidateProjectName(name); err == nil {
			t.Errorf("ValidateProjectName(%q) = nil, want error", name)
		}
	}
	var r Registry
	dir := t.TempDir()
	if _, err := r.Add(AddRequest{ProjectID: "Bad ID", Name: "x", Path: dir}); !errors.Is(err, ErrInvalidProjectID) {
		t.Errorf("bad id: %v", err)
	}
	if _, err := r.Add(AddRequest{ProjectID: "ok", Name: "x", Path: filepath.Join(dir, "missing")}); !errors.Is(err, ErrInvalidPath) {
		t.Errorf("missing dir: %v", err)
	}
	file := filepath.Join(dir, "file")
	os.WriteFile(file, nil, 0o600)
	if _, err := r.Add(AddRequest{ProjectID: "ok", Name: "x", Path: file}); !errors.Is(err, ErrInvalidPath) {
		t.Errorf("regular file: %v", err)
	}
	if _, err := r.Add(AddRequest{ProjectID: "ok", Name: "x", Path: ""}); !errors.Is(err, ErrInvalidPath) {
		t.Errorf("empty path: %v", err)
	}
}

func TestRemove(t *testing.T) {
	root := t.TempDir()
	a, b := filepath.Join(root, "a"), filepath.Join(root, "b")
	os.Mkdir(a, 0o755)
	os.Mkdir(b, 0o755)
	marker := filepath.Join(a, "keep.txt")
	os.WriteFile(marker, []byte("data"), 0o600)

	var r Registry
	ra := mustAdd(t, &r, "demo", "Demo", a)
	rb := mustAdd(t, &r, "demo", "Demo", b)

	if _, err := r.Remove("nope"); !errors.Is(err, ErrUnknownCheckout) {
		t.Fatalf("unknown id: %v", err)
	}
	res, err := r.Remove(ra.Checkout.ID)
	if err != nil || res.ProjectRemoved || res.Checkout.Path != ra.Checkout.Path {
		t.Fatalf("remove first: %+v %v", res, err)
	}
	if len(r.Projects) != 1 {
		t.Fatal("project must survive while it has checkouts")
	}
	res, err = r.Remove(rb.Checkout.ID)
	if err != nil || !res.ProjectRemoved || res.Project.ID != "demo" {
		t.Fatalf("remove last: %+v %v", res, err)
	}
	if len(r.Projects) != 0 || len(r.Checkouts) != 0 {
		t.Fatalf("registry not empty: %+v", r)
	}
	// Files are untouched.
	if data, err := os.ReadFile(marker); err != nil || string(data) != "data" {
		t.Fatalf("project file changed or removed: %v %q", err, data)
	}
	if _, err := os.Stat(b); err != nil {
		t.Fatal("directory removed")
	}
}

func TestValidateDetectsCorruption(t *testing.T) {
	good := Registry{
		Projects:  []Project{{ID: "p", Name: "P"}},
		Checkouts: []Checkout{{ID: "1", ProjectID: "p", Path: "/x"}},
	}
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	cases := map[string]Registry{
		"dup project":     {Projects: []Project{{ID: "p", Name: "P"}, {ID: "p", Name: "Q"}}},
		"dup checkout id": {Projects: good.Projects, Checkouts: []Checkout{{ID: "1", ProjectID: "p", Path: "/x"}, {ID: "1", ProjectID: "p", Path: "/y"}}},
		"dup path":        {Projects: good.Projects, Checkouts: []Checkout{{ID: "1", ProjectID: "p", Path: "/x"}, {ID: "2", ProjectID: "p", Path: "/x"}}},
		"orphan checkout": {Checkouts: []Checkout{{ID: "1", ProjectID: "ghost", Path: "/x"}}},
		"relative path":   {Projects: good.Projects, Checkouts: []Checkout{{ID: "1", ProjectID: "p", Path: "x"}}},
		"bad project id":  {Projects: []Project{{ID: "Bad", Name: "P"}}},
		"empty name":      {Projects: []Project{{ID: "p", Name: ""}}},
	}
	for name, r := range cases {
		if err := r.Validate(); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: err = %v, want ErrInvalid", name, err)
		}
	}
}

func TestAddCollisionLeavesModelUnchanged(t *testing.T) {
	dir := t.TempDir()
	canonical, err := CanonicalDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	// Force an occupied truncated ID without a probabilistic hash search.
	r := Registry{Projects: []Project{{ID: "old", Name: "Old"}}, Checkouts: []Checkout{{ID: CheckoutID(canonical), ProjectID: "old", Path: "/different-path"}}}
	_, err = r.Add(AddRequest{ProjectID: "new", Name: "New", Path: dir})
	if !errors.Is(err, ErrCheckoutIDCollision) {
		t.Fatalf("collision: %v", err)
	}
	if len(r.Projects) != 1 || len(r.Checkouts) != 1 || r.Checkouts[0].Path != "/different-path" {
		t.Fatalf("mutated: %+v", r)
	}
}

func TestValidateRejectsPathAliasesButPreservesUnavailablePaths(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "repo")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{dir + "/../repo", link} {
		r := Registry{Projects: []Project{{ID: "p", Name: "P"}}, Checkouts: []Checkout{{ID: "1", ProjectID: "p", Path: dir}, {ID: "2", ProjectID: "p", Path: path}}}
		if !errors.Is(r.Validate(), ErrInvalid) {
			t.Fatalf("accepted alias %s", path)
		}
	}
	r := Registry{}
	mustAdd(t, &r, "p", "P", dir)
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	if err := r.Validate(); err != nil {
		t.Fatalf("missing checkout invalidated: %v", err)
	}
	if len(r.Checkouts) != 1 {
		t.Fatal("missing checkout removed")
	}
}
