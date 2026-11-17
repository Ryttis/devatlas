// Package stack collects technology facts from a checkout by reading a fixed
// set of declarative files: Composer and npm manifests and lock files,
// Dockerfiles, and Compose files. It never executes project code, package
// managers, or install scripts, never follows symbolic links, never opens
// .env files, and bounds the size of everything it reads.
//
// A declared version (a constraint in a manifest, an image tag) and a locked
// version (an entry in a lock file) are kept apart, and neither is presented
// as the version that actually runs somewhere.
package stack

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Limits.
const (
	MaxLockBytes     = 32 << 20
	MaxManifestBytes = 2 << 20
	MaxPackages      = 30000
	maxWalkEntries   = 8000
	maxWalkDepth     = 3
	maxInfraFiles    = 40
)

// Areas a gap applies to.
const (
	AreaFreshness = "freshness"
	AreaSecurity  = "security"
	AreaBoth      = "both"
)

// Package origins. Only public packages may be named to external services.
const (
	OriginPublic  = "public"
	OriginPrivate = "private"
	OriginUnknown = "unknown"
)

// Version bases: how a technology version was established.
const (
	BasisLocked     = "locked"      // exact version in a lock file
	BasisImageTag   = "image-tag"   // tag declared in a Dockerfile or Compose file
	BasisArgDefault = "arg-default" // Dockerfile tag resolved through an ARG default
	BasisConstraint = "constraint"  // version constraint in a manifest
	BasisPlatform   = "platform"    // Composer config.platform override
)

// FileRef identifies one file that was read, by content hash.
type FileRef struct {
	Path   string `json:"path"`
	Kind   string `json:"kind"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// Ecosystem is one dependency ecosystem detected in the checkout.
type Ecosystem struct {
	Name      string `json:"name"`
	Supported bool   `json:"supported"`
	Manifest  string `json:"manifest,omitempty"`
	Lock      string `json:"lock,omitempty"`
	// Checked is true when the manifest and lock file were both parsed.
	Checked bool   `json:"checked"`
	Reason  string `json:"reason,omitempty"`
}

// Gap is a part of the stated scope that could not be checked.
type Gap struct {
	Area   string `json:"area"`
	Scope  string `json:"scope"`
	Reason string `json:"reason"`
}

// Technology is one declared or locked runtime, framework, database, or
// other infrastructure component.
type Technology struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Product is the endoflife.date product; empty when not recognised.
	Product string `json:"product,omitempty"`
	Kind    string `json:"kind"`
	Source  string `json:"source"`
	// Declared is the raw declaration: a constraint, an image reference, or
	// a locked version.
	Declared string `json:"declared"`
	// Version is a concrete version derived from Declared without guessing;
	// empty when the declaration does not determine one.
	Version string `json:"version,omitempty"`
	Basis   string `json:"basis"`
	// Reason explains an empty Version or an unrecognised component.
	Reason string `json:"reason,omitempty"`
}

// Package is one dependency.
type Package struct {
	ID        string `json:"id"`
	Ecosystem string `json:"ecosystem"`
	Name      string `json:"name"`
	// Version is the locked version; empty when there is no lock entry.
	Version string `json:"version,omitempty"`
	// Constraint is the declared constraint of a direct dependency.
	Constraint string `json:"constraint,omitempty"`
	Direct     bool   `json:"direct"`
	Dev        bool   `json:"dev"`
	Origin     string `json:"origin"`
	// Checkable is false when the locked version is not a release version
	// (for example a branch alias), so it cannot be matched to advisories.
	Checkable bool `json:"checkable"`
}

// Facts is everything collected from one checkout.
type Facts struct {
	Description  string       `json:"description,omitempty"`
	Files        []FileRef    `json:"files"`
	FilesHash    string       `json:"files_hash"`
	Ecosystems   []Ecosystem  `json:"ecosystems"`
	Technologies []Technology `json:"technologies"`
	Packages     []Package    `json:"packages"`
	Gaps         []Gap        `json:"gaps"`
}

// Errors of the bounded reader.
var (
	errSymlink  = errors.New("simbolinė nuoroda arba ne paprastas failas")
	errTooLarge = errors.New("failas viršija dydžio ribą")
)

type collector struct {
	root  string
	facts Facts
	pkgs  map[string]int
}

// Collect reads the facts of the checkout at root. root must be a canonical
// directory path. Problems with individual files become gaps; only an
// unreadable root is an error.
func Collect(root string) (Facts, error) {
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() {
		return Facts{}, fmt.Errorf("checkout directory cannot be read")
	}
	c := &collector{root: root, pkgs: map[string]int{}}
	c.facts = Facts{Files: []FileRef{}, Ecosystems: []Ecosystem{}, Technologies: []Technology{}, Packages: []Package{}, Gaps: []Gap{}}

	rootNames, sub := c.discover()
	c.composer(rootNames)
	c.npm(rootNames)
	c.unsupported(rootNames)
	c.infra(rootNames, sub.infra)
	for _, p := range sub.manifests {
		c.gap(AreaBoth, p, "priklausomybių manifestas pakatalogyje: ši versija tikrina tik checkout šaknies manifestus")
	}
	if sub.truncated {
		c.gap(AreaBoth, "pakatalogiai", "pakatalogių peržiūra nutraukta pasiekus įrašų ribą; gali būti nerastų manifestų")
	}
	if len(c.facts.Ecosystems) == 0 {
		c.gap(AreaSecurity, "priklausomybės", "priklausomybių manifestų ir lock failų šaknyje nerasta")
		if len(c.facts.Technologies) == 0 {
			c.gap(AreaFreshness, "technologijos", "manifestų, Dockerfile ar Compose failų nerasta")
		}
	}

	sort.Slice(c.facts.Files, func(i, j int) bool { return c.facts.Files[i].Path < c.facts.Files[j].Path })
	h := sha256.New()
	for _, f := range c.facts.Files {
		fmt.Fprintf(h, "%s\x00%s\n", f.Path, f.SHA256)
	}
	c.facts.FilesHash = hex.EncodeToString(h.Sum(nil))
	sort.SliceStable(c.facts.Packages, func(i, j int) bool {
		a, b := c.facts.Packages[i], c.facts.Packages[j]
		if a.Ecosystem != b.Ecosystem {
			return a.Ecosystem < b.Ecosystem
		}
		if a.Direct != b.Direct {
			return a.Direct
		}
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		return a.Version < b.Version
	})
	for i := range c.facts.Packages {
		c.facts.Packages[i].ID = fmt.Sprintf("P%d", i+1)
	}
	for i := range c.facts.Technologies {
		c.facts.Technologies[i].ID = fmt.Sprintf("T%d", i+1)
	}
	return c.facts, nil
}

func (c *collector) gap(area, scope, reason string) {
	c.facts.Gaps = append(c.facts.Gaps, Gap{Area: area, Scope: scope, Reason: reason})
}

func (c *collector) tech(t Technology) {
	for _, e := range c.facts.Technologies {
		if e.Name == t.Name && e.Source == t.Source && e.Declared == t.Declared {
			return
		}
	}
	c.facts.Technologies = append(c.facts.Technologies, t)
}

func (c *collector) addPackage(p Package) {
	key := p.Ecosystem + "\x00" + p.Name + "\x00" + p.Version
	if i, ok := c.pkgs[key]; ok {
		e := &c.facts.Packages[i]
		e.Direct = e.Direct || p.Direct
		e.Dev = e.Dev && p.Dev
		if e.Constraint == "" {
			e.Constraint = p.Constraint
		}
		if e.Origin != p.Origin && p.Origin != OriginPublic {
			e.Origin = p.Origin
		}
		return
	}
	c.pkgs[key] = len(c.facts.Packages)
	c.facts.Packages = append(c.facts.Packages, p)
}

// read returns the content of a regular file below the root. Every path
// component is checked with Lstat so a symbolic link anywhere is rejected,
// and the opened file is compared with the checked one.
func (c *collector) read(rel, kind string, limit int64) ([]byte, error) {
	rel = filepath.Clean(rel)
	if rel == "." || filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil, errSymlink
	}
	cur := c.root
	parts := strings.Split(rel, string(filepath.Separator))
	var info os.FileInfo
	for i, part := range parts {
		cur = filepath.Join(cur, part)
		var err error
		info, err = os.Lstat(cur)
		if err != nil {
			return nil, err
		}
		if i < len(parts)-1 && !info.IsDir() {
			return nil, errSymlink
		}
	}
	if !info.Mode().IsRegular() {
		return nil, errSymlink
	}
	if info.Size() > limit {
		return nil, errTooLarge
	}
	f, err := os.Open(cur)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return nil, errSymlink
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errTooLarge
	}
	sum := sha256.Sum256(data)
	c.facts.Files = append(c.facts.Files, FileRef{Path: filepath.ToSlash(rel), Kind: kind, Size: int64(len(data)), SHA256: hex.EncodeToString(sum[:])})
	return data, nil
}

// readReason turns a read error into display text without file content.
func readReason(err error) string {
	switch {
	case errors.Is(err, errSymlink):
		return "failas atmestas: " + errSymlink.Error()
	case errors.Is(err, errTooLarge):
		return "failas atmestas: " + errTooLarge.Error()
	case errors.Is(err, os.ErrNotExist):
		return "failo nėra"
	default:
		return "failo nepavyko perskaityti"
	}
}

type subFiles struct {
	manifests []string
	infra     []string
	truncated bool
}

var skipDirs = map[string]bool{"node_modules": true, "vendor": true, "var": true}

var subManifestNames = map[string]bool{
	"composer.json": true, "package.json": true, "pyproject.toml": true, "go.mod": true,
	"Gemfile": true, "Cargo.toml": true, "pom.xml": true, "Pipfile": true,
}

// discover lists root entry names and, to a bounded depth, files in
// subdirectories that matter: infrastructure declarations (parsed) and
// dependency manifests (reported as not checked).
func (c *collector) discover() (map[string]os.FileMode, subFiles) {
	root := map[string]os.FileMode{}
	var sub subFiles
	entries, err := os.ReadDir(c.root)
	if err != nil {
		return root, sub
	}
	visited := 0
	var walk func(rel string, depth int)
	walk = func(rel string, depth int) {
		es, err := os.ReadDir(filepath.Join(c.root, rel))
		if err != nil {
			return
		}
		for _, e := range es {
			if visited++; visited > maxWalkEntries {
				sub.truncated = true
				return
			}
			name := e.Name()
			p := filepath.Join(rel, name)
			switch {
			case e.Type()&os.ModeSymlink != 0:
			case e.IsDir():
				if depth < maxWalkDepth && !skipDirs[name] && !strings.HasPrefix(name, ".") {
					walk(p, depth+1)
				}
			case !e.Type().IsRegular():
			case subManifestNames[name]:
				sub.manifests = append(sub.manifests, filepath.ToSlash(p))
			case isDockerfile(name) || isCompose(name):
				sub.infra = append(sub.infra, p)
			}
		}
	}
	for _, e := range entries {
		root[e.Name()] = e.Type()
		if e.IsDir() && !skipDirs[e.Name()] && !strings.HasPrefix(e.Name(), ".") {
			walk(e.Name(), 1)
		}
	}
	sort.Strings(sub.manifests)
	sort.Strings(sub.infra)
	return root, sub
}

func has(names map[string]os.FileMode, name string) bool {
	_, ok := names[name]
	return ok
}

// unsupported records ecosystems this version detects but does not check.
func (c *collector) unsupported(names map[string]os.FileMode) {
	add := func(name, manifest, reason string) {
		c.facts.Ecosystems = append(c.facts.Ecosystems, Ecosystem{Name: name, Manifest: manifest, Reason: reason})
		c.gap(AreaBoth, name+" ("+manifest+")", reason)
	}
	if has(names, "yarn.lock") {
		add("yarn", "yarn.lock", "Yarn lock failas šioje versijoje netikrinamas")
	}
	if has(names, "pnpm-lock.yaml") {
		add("pnpm", "pnpm-lock.yaml", "pnpm lock failas šioje versijoje netikrinamas")
	}
	var py []string
	for n := range names {
		if n == "pyproject.toml" || n == "Pipfile" || n == "Pipfile.lock" || n == "poetry.lock" || n == "uv.lock" || n == "setup.py" ||
			(strings.HasPrefix(n, "requirements") && strings.HasSuffix(n, ".txt")) {
			py = append(py, n)
		}
	}
	if len(py) > 0 {
		sort.Strings(py)
		add("python", strings.Join(py, ", "), "Python priklausomybės šioje versijoje netikrinamos")
	}
	for _, e := range []struct{ file, name, label string }{
		{"go.mod", "go", "Go moduliai"}, {"Gemfile", "ruby", "Ruby priklausomybės"}, {"Cargo.toml", "rust", "Rust priklausomybės"},
		{"pom.xml", "maven", "Maven priklausomybės"}, {"build.gradle", "gradle", "Gradle priklausomybės"}, {"build.gradle.kts", "gradle", "Gradle priklausomybės"},
	} {
		if has(names, e.file) {
			add(e.name, e.file, e.label+" šioje versijoje netikrinamos")
		}
	}
	for n := range names {
		if strings.HasSuffix(n, ".csproj") || strings.HasSuffix(n, ".sln") {
			add("dotnet", n, ".NET priklausomybės šioje versijoje netikrinamos")
			break
		}
	}
	sort.SliceStable(c.facts.Ecosystems, func(i, j int) bool { return c.facts.Ecosystems[i].Name < c.facts.Ecosystems[j].Name })
}
