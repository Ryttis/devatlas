package stack

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func collect(t *testing.T, root string) Facts {
	t.Helper()
	f, err := Collect(root)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func pkg(f Facts, name string) *Package {
	for i := range f.Packages {
		if f.Packages[i].Name == name {
			return &f.Packages[i]
		}
	}
	return nil
}

func techs(f Facts, name string) []Technology {
	var out []Technology
	for _, t := range f.Technologies {
		if t.Name == name {
			out = append(out, t)
		}
	}
	return out
}

func hasGap(f Facts, substr string) bool {
	for _, g := range f.Gaps {
		if strings.Contains(g.Scope+" "+g.Reason, substr) {
			return true
		}
	}
	return false
}

const fixtureComposerJSON = `{"description":"Demo  app","require":{"php":"^8.2","symfony/framework-bundle":"6.4.*","acme/private":"^1.0","ext-json":"*"},"require-dev":{"phpunit/phpunit":"^10"}}`

const fixtureComposerLock = `{"packages":[
 {"name":"symfony/framework-bundle","version":"v6.4.3","notification-url":"https://packagist.org/downloads/","dist":{"url":"https://api.github.com/x"}},
 {"name":"psr/log","version":"3.0.0","notification-url":"https://packagist.org/downloads/"},
 {"name":"acme/private","version":"1.2.0","dist":{"url":"https://satis.internal.example/dist/acme.zip"}},
 {"name":"acme/branch","version":"dev-main","notification-url":"https://packagist.org/downloads/"},
 {"name":"acme/nowhere","version":"1.0.0"}],
 "packages-dev":[{"name":"phpunit/phpunit","version":"10.5.1","notification-url":"https://packagist.org/downloads/"}]}`

func TestComposerSeparatesDeclaredLockedAndOrigin(t *testing.T) {
	root := t.TempDir()
	write(t, root, "composer.json", fixtureComposerJSON)
	write(t, root, "composer.lock", fixtureComposerLock)
	f := collect(t, root)

	if f.Description != "Demo app" {
		t.Errorf("description = %q", f.Description)
	}
	php := techs(f, "PHP")
	if len(php) != 1 || php[0].Version != "" || php[0].Basis != BasisConstraint || php[0].Declared != "^8.2" || php[0].Reason == "" {
		t.Errorf("a wide constraint must not yield a version: %+v", php)
	}
	sf := techs(f, "Symfony")
	if len(sf) != 1 || sf[0].Version != "6.4.3" || sf[0].Basis != BasisLocked || sf[0].Source != "composer.lock" {
		t.Errorf("framework must come from the lock file: %+v", sf)
	}
	fb := pkg(f, "symfony/framework-bundle")
	if fb == nil || !fb.Direct || fb.Constraint != "6.4.*" || fb.Version != "v6.4.3" || fb.Origin != OriginPublic || !fb.Checkable || fb.Dev {
		t.Errorf("direct public package: %+v", fb)
	}
	if p := pkg(f, "psr/log"); p == nil || p.Direct || p.Origin != OriginPublic {
		t.Errorf("transitive package: %+v", p)
	}
	if p := pkg(f, "acme/private"); p == nil || p.Origin != OriginPrivate {
		t.Errorf("a package from another repository must be private: %+v", p)
	}
	if p := pkg(f, "acme/nowhere"); p == nil || p.Origin != OriginUnknown {
		t.Errorf("a package without origin evidence must be unknown: %+v", p)
	}
	if p := pkg(f, "acme/branch"); p == nil || p.Checkable {
		t.Errorf("a branch alias is not a checkable version: %+v", p)
	}
	if p := pkg(f, "phpunit/phpunit"); p == nil || !p.Dev || !p.Direct {
		t.Errorf("dev package: %+v", p)
	}
	if pkg(f, "ext-json") != nil || pkg(f, "php") != nil {
		t.Error("platform requirements are not packages")
	}
	if len(f.Ecosystems) != 1 || !f.Ecosystems[0].Checked || len(f.Gaps) != 0 {
		t.Errorf("ecosystems %+v gaps %+v", f.Ecosystems, f.Gaps)
	}
}

func TestPinnedConstraintYieldsVersion(t *testing.T) {
	for in, want := range map[string]string{"8.2.*": "8.2", "~8.2.0": "8.2", "8.3.1": "8.3", "^8.2": "", ">=8.1": "", "~8.2": "", "8.*": "", "^8.1 || ^8.2": "", "20": ""} {
		if got := pinnedVersion(in); got != want {
			t.Errorf("pinnedVersion(%q) = %q, want %q", in, got, want)
		}
	}
}

const packageLock = `{"lockfileVersion":3,"packages":{
 "":{"name":"app","dependencies":{"react":"^18.2.0","@acme/ui":"^1.0.0","local-lib":"*"}},
 "node_modules/react":{"version":"18.2.0","resolved":"https://registry.npmjs.org/react/-/react-18.2.0.tgz"},
 "node_modules/loose-envify":{"version":"1.4.0","resolved":"https://registry.npmjs.org/loose-envify/-/loose-envify-1.4.0.tgz"},
 "node_modules/@acme/ui":{"version":"1.3.0","resolved":"https://npm.internal.example/@acme/ui/-/ui-1.3.0.tgz"},
 "node_modules/bundled":{"version":"2.0.0","inBundle":true},
 "node_modules/eslint":{"version":"9.1.0","resolved":"https://registry.npmjs.org/eslint/-/eslint-9.1.0.tgz","dev":true},
 "node_modules/local-lib":{"resolved":"packages/local-lib","link":true},
 "packages/local-lib":{"name":"local-lib","version":"0.0.1"},
 "node_modules/a/node_modules/react":{"version":"17.0.2","resolved":"https://registry.npmjs.org/react/-/react-17.0.2.tgz"}}}`

func TestNpmLockOriginsDirectAndWorkspaces(t *testing.T) {
	root := t.TempDir()
	write(t, root, "package.json", `{"description":"UI","dependencies":{"react":"^18.2.0","@acme/ui":"^1.0.0","local-lib":"*"},"devDependencies":{"eslint":"^9"},"engines":{"node":">=20"}}`)
	write(t, root, "package-lock.json", packageLock)
	f := collect(t, root)

	var react18, react17 *Package
	for i := range f.Packages {
		p := &f.Packages[i]
		if p.Name == "react" && p.Version == "18.2.0" {
			react18 = p
		}
		if p.Name == "react" && p.Version == "17.0.2" {
			react17 = p
		}
	}
	if react18 == nil || !react18.Direct || react18.Origin != OriginPublic || react17 == nil || react17.Direct {
		t.Errorf("react entries: %+v %+v", react18, react17)
	}
	if p := pkg(f, "@acme/ui"); p == nil || p.Origin != OriginPrivate || !p.Direct {
		t.Errorf("private registry package: %+v", p)
	}
	if p := pkg(f, "bundled"); p == nil || p.Origin != OriginUnknown {
		t.Errorf("package without a resolved URL must have unknown origin: %+v", p)
	}
	if p := pkg(f, "eslint"); p == nil || !p.Dev {
		t.Errorf("dev flag lost: %+v", p)
	}
	if pkg(f, "local-lib") != nil {
		t.Error("workspace members are not registry packages")
	}
	if hasGap(f, "local-lib") {
		t.Error("a linked workspace member is not a lock mismatch")
	}
	if r := techs(f, "React"); len(r) != 1 || r[0].Version != "18.2.0" || r[0].Basis != BasisLocked {
		t.Errorf("react technology: %+v", r)
	}
	if n := techs(f, "Node.js"); len(n) != 1 || n[0].Version != "" {
		t.Errorf("engines constraint must not yield a version: %+v", n)
	}
}

func TestUnsupportedAndMixedEcosystemsStayVisible(t *testing.T) {
	root := t.TempDir()
	write(t, root, "composer.json", `{"require":{"psr/log":"^3"}}`)
	write(t, root, "composer.lock", `{"packages":[{"name":"psr/log","version":"3.0.0","notification-url":"https://packagist.org/downloads/"}],"packages-dev":[]}`)
	write(t, root, "package.json", `{"dependencies":{"react":"^19.0.0"}}`)
	write(t, root, "yarn.lock", "# yarn lockfile v1\n")
	write(t, root, "pyproject.toml", "[project]\nname='x'\n")
	write(t, root, "go.mod", "module x\n")
	write(t, root, "apps/web/composer.json", `{}`)
	write(t, root, "vendor/x/y/composer.json", `{}`)
	f := collect(t, root)

	got := map[string]bool{}
	for _, e := range f.Ecosystems {
		got[e.Name] = e.Checked
		if !e.Checked && e.Reason == "" {
			t.Errorf("unchecked ecosystem %s has no reason", e.Name)
		}
	}
	if !got["composer"] || got["yarn"] || got["python"] || got["go"] || len(got) != 4 {
		t.Errorf("ecosystems: %+v", f.Ecosystems)
	}
	for _, want := range []string{"yarn", "python", "go (go.mod)", "apps/web/composer.json"} {
		if !hasGap(f, want) {
			t.Errorf("missing gap for %s: %+v", want, f.Gaps)
		}
	}
	if hasGap(f, "vendor/") {
		t.Error("vendor directories must not be scanned")
	}
	if p := pkg(f, "react"); p == nil || p.Version != "" || p.Constraint != "^19.0.0" {
		t.Errorf("a dependency locked only by yarn.lock has no known version: %+v", p)
	}
	if r := techs(f, "React"); len(r) != 1 || r[0].Version != "" {
		t.Errorf("declared-only framework must have no version: %+v", r)
	}
}

func TestMissingLockIsAGapNotAnEmptyResult(t *testing.T) {
	root := t.TempDir()
	write(t, root, "composer.json", `{"require":{"psr/log":"^3"}}`)
	write(t, root, "package.json", `{"dependencies":{"left-pad":"1.3.0"}}`)
	write(t, root, "package-lock.json", `{"lockfileVersion":1,"dependencies":{}}`)
	f := collect(t, root)
	if !hasGap(f, "nėra composer.lock") || !hasGap(f, "lockfileVersion 1") {
		t.Errorf("gaps: %+v", f.Gaps)
	}
	for _, e := range f.Ecosystems {
		if e.Checked {
			t.Errorf("%s must not count as checked", e.Name)
		}
	}
}

func TestNoManifestsIsUnverified(t *testing.T) {
	f := collect(t, t.TempDir())
	if len(f.Ecosystems) != 0 || !hasGap(f, "nerasta") || len(f.Gaps) != 2 {
		t.Errorf("gaps: %+v", f.Gaps)
	}
}

func TestInfrastructureDeclarations(t *testing.T) {
	root := t.TempDir()
	write(t, root, "Dockerfile", "ARG PHP_VERSION=8.3\nARG NODE\nFROM --platform=linux/amd64 php:${PHP_VERSION}-fpm-alpine AS base\nFROM node:${NODE}-alpine AS assets\nFROM base\nFROM scratch\n")
	write(t, root, "compose.yaml", `services:
  db:
    image: mariadb:10.11
    environment:
      MARIADB_ROOT_PASSWORD: SECRET_MARKER
  cache:
    image: redis:7-alpine
  search:
    image: "docker.elastic.co/elasticsearch/elasticsearch:8.11.1"
  web:
    image: nginx
  var:
    image: postgres:${PG_VERSION:-16}
  priv:
    image: registry.internal.example/team/app:1.2
  app:
    image: built-app
    build:
      context: docker/app
      dockerfile: Dockerfile.dev
  out:
    build: ../outside
`)
	write(t, root, "docker/app/Dockerfile.dev", "FROM python:3.12-slim@sha256:abc\n")
	write(t, root, ".env", "PG_VERSION=PGVER_MARKER\nTOKEN=SECRET_MARKER\n")
	f := collect(t, root)

	want := map[string][2]string{ // name -> version, basis
		"PHP": {"8.3", BasisArgDefault}, "MariaDB": {"10.11", BasisImageTag}, "Redis": {"7", BasisImageTag},
		"Elasticsearch": {"8.11.1", BasisImageTag}, "nginx": {"", BasisImageTag}, "PostgreSQL": {"", BasisImageTag},
		"Python": {"3.12", BasisImageTag}, "Node.js": {"", BasisImageTag},
	}
	for name, w := range want {
		ts := techs(f, name)
		if len(ts) != 1 || ts[0].Version != w[0] || ts[0].Basis != w[1] {
			t.Errorf("%s: got %+v, want version %q basis %q", name, ts, w[0], w[1])
		}
		if len(ts) == 1 && ts[0].Version == "" && ts[0].Reason == "" {
			t.Errorf("%s: an undetermined version needs a reason", name)
		}
	}
	var priv *Technology
	for i, tt := range f.Technologies {
		if strings.Contains(tt.Declared, "registry.internal.example") {
			priv = &f.Technologies[i]
		}
		if tt.Declared == "built-app" {
			t.Error("an image name next to a build section is the build result, not a dependency")
		}
	}
	if priv == nil || priv.Product != "" || priv.Version != "" {
		t.Errorf("a private image must stay unrecognised: %+v", priv)
	}
	js, _ := json.Marshal(f)
	if strings.Contains(string(js), "SECRET_MARKER") || strings.Contains(string(js), "PGVER_MARKER") {
		t.Error("environment values or .env content reached the facts")
	}
	for _, file := range f.Files {
		if strings.Contains(file.Path, ".env") || strings.Contains(file.Path, "outside") {
			t.Errorf("unexpected file read: %s", file.Path)
		}
	}
}

func TestSymlinksAndOversizedFilesAreRejected(t *testing.T) {
	outside := t.TempDir()
	write(t, outside, "secret.lock", `{"packages":[{"name":"leak/leak","version":"1.0.0","notification-url":"https://packagist.org/downloads/"}]}`)
	write(t, outside, "Dockerfile", "FROM php:7.4\n")
	root := t.TempDir()
	write(t, root, "composer.json", `{"require":{"leak/leak":"^1"}}`)
	if err := os.Symlink(filepath.Join(outside, "secret.lock"), filepath.Join(root, "composer.lock")); err != nil {
		t.Skip("symlinks unavailable")
	}
	if err := os.Symlink(outside, filepath.Join(root, "docker")); err != nil {
		t.Fatal(err)
	}
	write(t, root, "compose.yaml", "services:\n  app:\n    build: docker\n")
	write(t, root, "package.json", `{"x":"`+strings.Repeat("a", MaxManifestBytes)+`"}`)
	f := collect(t, root)

	if p := pkg(f, "leak/leak"); p != nil && p.Version != "" {
		t.Errorf("a symlinked lock file was read: %+v", p)
	}
	if len(techs(f, "PHP")) != 0 {
		t.Error("a Dockerfile behind a symlinked directory was read")
	}
	if !hasGap(f, "simbolinė nuoroda") || !hasGap(f, "dydžio ribą") {
		t.Errorf("gaps: %+v", f.Gaps)
	}
	for _, file := range f.Files {
		if file.Path == "composer.lock" || file.Path == "package.json" {
			t.Errorf("rejected file listed as assessed: %s", file.Path)
		}
	}
}

func TestFilesHashFollowsContent(t *testing.T) {
	root := t.TempDir()
	write(t, root, "composer.json", `{"require":{}}`)
	write(t, root, "composer.lock", `{"packages":[],"packages-dev":[]}`)
	a := collect(t, root).FilesHash
	if b := collect(t, root).FilesHash; a != b || a == "" {
		t.Fatalf("hash is not stable: %q %q", a, b)
	}
	write(t, root, "composer.lock", `{"packages":[],"packages-dev":[] }`)
	if c := collect(t, root).FilesHash; c == a {
		t.Fatal("hash did not change with the lock file")
	}
}
