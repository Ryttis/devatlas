package stack

import (
	"encoding/json"
	"net/url"
	"os"
	"regexp"
	"strings"
)

// stringMap decodes a JSON object of strings and tolerates the empty array
// that PHP tooling writes for an empty object.
type stringMap map[string]string

func (m *stringMap) UnmarshalJSON(b []byte) error {
	out := map[string]string{}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		*m = out
		return nil
	}
	for k, v := range raw {
		var s string
		if json.Unmarshal(v, &s) == nil {
			out[k] = s
		}
	}
	*m = out
	return nil
}

func trimText(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > max {
		s = string(r[:max]) + "…"
	}
	return s
}

var (
	composerNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,99}/[a-z0-9][a-z0-9_.-]{0,99}$`)
	npmNameRE      = regexp.MustCompile(`^(@[A-Za-z0-9][A-Za-z0-9._~-]{0,99}/)?[A-Za-z0-9][A-Za-z0-9._~-]{0,199}$`)
	releaseRE      = regexp.MustCompile(`^v?\d+(\.\d+){0,3}([-+.][0-9A-Za-z][0-9A-Za-z.+-]{0,40})?$`)
)

// frameworks maps a package to the endoflife.date product it represents.
var composerFrameworks = map[string][2]string{
	"symfony/framework-bundle": {"Symfony", "symfony"},
	"symfony/symfony":          {"Symfony", "symfony"},
	"laravel/framework":        {"Laravel", "laravel"},
	"api-platform/core":        {"API Platform", "api-platform"},
}

var npmFrameworks = map[string][2]string{
	"react":         {"React", "react"},
	"vue":           {"Vue", "vue"},
	"@angular/core": {"Angular", "angular"},
	"next":          {"Next.js", "nextjs"},
	"nuxt":          {"Nuxt", "nuxt"},
	"express":       {"Express", "express"},
}

type composerJSON struct {
	Description string    `json:"description"`
	Require     stringMap `json:"require"`
	RequireDev  stringMap `json:"require-dev"`
	Config      struct {
		Platform stringMap `json:"platform"`
	} `json:"config"`
}

type composerLockPkg struct {
	Name            string `json:"name"`
	Version         string `json:"version"`
	NotificationURL string `json:"notification-url"`
	Source          *struct {
		URL string `json:"url"`
	} `json:"source"`
	Dist *struct {
		URL string `json:"url"`
	} `json:"dist"`
}

type composerLock struct {
	Packages    []composerLockPkg `json:"packages"`
	PackagesDev []composerLockPkg `json:"packages-dev"`
}

const packagistNotification = "https://packagist.org/downloads/"

func (c *collector) composer(names map[string]os.FileMode) {
	if !has(names, "composer.json") {
		if has(names, "composer.lock") {
			c.facts.Ecosystems = append(c.facts.Ecosystems, Ecosystem{Name: "composer", Supported: true, Lock: "composer.lock", Reason: "nėra composer.json"})
			c.gap(AreaBoth, "composer", "rastas composer.lock be composer.json")
		}
		return
	}
	eco := Ecosystem{Name: "composer", Supported: true, Manifest: "composer.json"}
	defer func() { c.facts.Ecosystems = append(c.facts.Ecosystems, eco) }()

	data, err := c.read("composer.json", "composer-manifest", MaxManifestBytes)
	if err != nil {
		eco.Reason = readReason(err)
		c.gap(AreaBoth, "composer.json", eco.Reason)
		return
	}
	var man composerJSON
	if err := json.Unmarshal(data, &man); err != nil {
		eco.Reason = "composer.json nėra galiojantis JSON"
		c.gap(AreaBoth, "composer.json", eco.Reason)
		return
	}
	if c.facts.Description == "" {
		c.facts.Description = trimText(man.Description, 200)
	}
	if v, ok := man.Config.Platform["php"]; ok {
		c.tech(Technology{Name: "PHP", Product: "php", Kind: "runtime", Source: "composer.json", Declared: trimText(v, 60),
			Version: exactVersion(v), Basis: BasisPlatform, Reason: reasonIfEmpty(exactVersion(v), "config.platform.php reikšmė nėra konkreti versija")})
	}
	if v, ok := man.Require["php"]; ok {
		pinned := pinnedVersion(v)
		c.tech(Technology{Name: "PHP", Product: "php", Kind: "runtime", Source: "composer.json", Declared: trimText(v, 60),
			Version: pinned, Basis: BasisConstraint, Reason: reasonIfEmpty(pinned, "versijos apribojimas leidžia kelias šakas; konkreti versija nedeklaruota")})
	}
	direct := map[string]string{}
	devDirect := map[string]bool{}
	for n, v := range man.Require {
		direct[strings.ToLower(n)] = v
	}
	for n, v := range man.RequireDev {
		direct[strings.ToLower(n)] = v
		devDirect[strings.ToLower(n)] = true
	}

	if !has(names, "composer.lock") {
		eco.Reason = "nėra composer.lock: užrakintos versijos nežinomos"
		c.gap(AreaBoth, "composer", eco.Reason)
		c.declaredOnly("composer", direct, devDirect, composerFrameworks, "composer.json", "nėra composer.lock")
		return
	}
	eco.Lock = "composer.lock"
	lockData, err := c.read("composer.lock", "composer-lock", MaxLockBytes)
	var lock composerLock
	if err == nil {
		if json.Unmarshal(lockData, &lock) != nil {
			err = os.ErrInvalid
		}
	}
	if err != nil {
		eco.Reason = "composer.lock: " + readReason(err)
		c.gap(AreaBoth, "composer.lock", eco.Reason)
		c.declaredOnly("composer", direct, devDirect, composerFrameworks, "composer.json", "composer.lock neperskaitytas")
		return
	}
	if len(lock.Packages)+len(lock.PackagesDev) > MaxPackages {
		eco.Reason = "composer.lock viršija paketų skaičiaus ribą"
		c.gap(AreaBoth, "composer.lock", eco.Reason)
		return
	}
	eco.Checked = true
	seen := map[string]bool{}
	add := func(list []composerLockPkg, dev bool) {
		for _, p := range list {
			name := strings.ToLower(p.Name)
			seen[name] = true
			pkg := Package{Ecosystem: "composer", Name: name, Version: p.Version, Dev: dev, Origin: composerOrigin(p)}
			pkg.Checkable = releaseRE.MatchString(p.Version) && !strings.Contains(strings.ToLower(p.Version), "dev")
			if !composerNameRE.MatchString(name) {
				pkg.Origin = OriginUnknown
			}
			if cons, ok := direct[name]; ok {
				pkg.Direct, pkg.Constraint = true, trimText(cons, 80)
			}
			c.addPackage(pkg)
			if fw, ok := composerFrameworks[name]; ok && pkg.Checkable {
				c.tech(Technology{Name: fw[0], Product: fw[1], Kind: "framework", Source: "composer.lock",
					Declared: p.Version, Version: strings.TrimPrefix(p.Version, "v"), Basis: BasisLocked})
			}
		}
	}
	add(lock.Packages, false)
	add(lock.PackagesDev, true)
	for name := range direct {
		if !seen[name] && isComposerPackage(name) {
			c.gap(AreaBoth, "composer: "+name, "tiesioginės priklausomybės nėra composer.lock: lock failas neatitinka manifesto")
		}
	}
}

// isComposerPackage excludes platform requirements (php, ext-*, lib-*,
// composer-plugin-api and similar), which are not lock file packages.
func isComposerPackage(name string) bool {
	return strings.Contains(name, "/")
}

func composerOrigin(p composerLockPkg) string {
	if p.NotificationURL == packagistNotification {
		return OriginPublic
	}
	if (p.Dist != nil && p.Dist.URL != "") || (p.Source != nil && p.Source.URL != "") {
		return OriginPrivate
	}
	return OriginUnknown
}

// declaredOnly records direct dependencies that have no locked version.
func (c *collector) declaredOnly(eco string, direct map[string]string, dev map[string]bool, frameworks map[string][2]string, source, why string) {
	for name, cons := range direct {
		if eco == "composer" && !isComposerPackage(name) {
			continue
		}
		c.addPackage(Package{Ecosystem: eco, Name: name, Constraint: trimText(cons, 80), Direct: true, Dev: dev[name], Origin: OriginUnknown})
		if fw, ok := frameworks[name]; ok {
			c.tech(Technology{Name: fw[0], Product: fw[1], Kind: "framework", Source: source, Declared: trimText(cons, 60),
				Basis: BasisConstraint, Reason: why + ": žinomas tik versijos apribojimas"})
		}
	}
}

type packageJSON struct {
	Description string    `json:"description"`
	Deps        stringMap `json:"dependencies"`
	DevDeps     stringMap `json:"devDependencies"`
	OptDeps     stringMap `json:"optionalDependencies"`
	Engines     stringMap `json:"engines"`
}

type npmLockPkg struct {
	Name     string `json:"name"`
	Version  string `json:"version"`
	Resolved string `json:"resolved"`
	Dev      bool   `json:"dev"`
	Link     bool   `json:"link"`
}

type npmLock struct {
	LockfileVersion int                   `json:"lockfileVersion"`
	Packages        map[string]npmLockPkg `json:"packages"`
}

func (c *collector) npm(names map[string]os.FileMode) {
	if !has(names, "package.json") {
		if has(names, "package-lock.json") {
			c.facts.Ecosystems = append(c.facts.Ecosystems, Ecosystem{Name: "npm", Supported: true, Lock: "package-lock.json", Reason: "nėra package.json"})
			c.gap(AreaBoth, "npm", "rastas package-lock.json be package.json")
		}
		return
	}
	hasLock := has(names, "package-lock.json")
	otherLock := has(names, "yarn.lock") || has(names, "pnpm-lock.yaml")
	eco := Ecosystem{Name: "npm", Supported: true, Manifest: "package.json"}
	// With only a Yarn or pnpm lock file the manifest is still read for its
	// declarations, but the npm ecosystem itself is not what is in use.
	record := hasLock || !otherLock
	defer func() {
		if record {
			c.facts.Ecosystems = append(c.facts.Ecosystems, eco)
		}
	}()

	data, err := c.read("package.json", "npm-manifest", MaxManifestBytes)
	if err != nil {
		eco.Reason = readReason(err)
		c.gap(AreaBoth, "package.json", eco.Reason)
		return
	}
	var man packageJSON
	if err := json.Unmarshal(data, &man); err != nil {
		eco.Reason = "package.json nėra galiojantis JSON"
		c.gap(AreaBoth, "package.json", eco.Reason)
		return
	}
	if c.facts.Description == "" {
		c.facts.Description = trimText(man.Description, 200)
	}
	if v, ok := man.Engines["node"]; ok {
		pinned := pinnedVersion(v)
		c.tech(Technology{Name: "Node.js", Product: "nodejs", Kind: "runtime", Source: "package.json", Declared: trimText(v, 60),
			Version: pinned, Basis: BasisConstraint, Reason: reasonIfEmpty(pinned, "versijos apribojimas leidžia kelias šakas; konkreti versija nedeklaruota")})
	}
	direct := map[string]string{}
	devDirect := map[string]bool{}
	for n, v := range man.Deps {
		direct[n] = v
	}
	for n, v := range man.OptDeps {
		direct[n] = v
	}
	for n, v := range man.DevDeps {
		direct[n] = v
		devDirect[n] = true
	}

	if !hasLock {
		why := "nėra package-lock.json"
		if otherLock {
			why = "užrakintos versijos yra nepalaikomame lock faile"
		} else {
			eco.Reason = "nėra package-lock.json: užrakintos versijos nežinomos"
			c.gap(AreaBoth, "npm", eco.Reason)
		}
		c.declaredOnly("npm", direct, devDirect, npmFrameworks, "package.json", why)
		return
	}
	eco.Lock = "package-lock.json"
	lockData, err := c.read("package-lock.json", "npm-lock", MaxLockBytes)
	var lock npmLock
	if err == nil {
		if json.Unmarshal(lockData, &lock) != nil {
			err = os.ErrInvalid
		}
	}
	switch {
	case err != nil:
		eco.Reason = "package-lock.json: " + readReason(err)
	case lock.LockfileVersion < 2 || lock.Packages == nil:
		eco.Reason = "package-lock.json lockfileVersion 1 nepalaikoma (reikia 2 arba 3)"
	case len(lock.Packages) > MaxPackages:
		eco.Reason = "package-lock.json viršija paketų skaičiaus ribą"
	}
	if eco.Reason != "" {
		c.gap(AreaBoth, "package-lock.json", eco.Reason)
		c.declaredOnly("npm", direct, devDirect, npmFrameworks, "package.json", "package-lock.json nepanaudotas")
		return
	}
	eco.Checked = true
	seen := map[string]bool{}
	for key, p := range lock.Packages {
		i := strings.LastIndex(key, "node_modules/")
		if key == "" || i < 0 || p.Link {
			continue // the root project, a workspace member, or a link to one
		}
		pathName := key[i+len("node_modules/"):]
		name := p.Name
		if name == "" {
			name = pathName
		}
		pkg := Package{Ecosystem: "npm", Name: name, Version: p.Version, Dev: p.Dev, Origin: npmOrigin(p.Resolved)}
		pkg.Checkable = releaseRE.MatchString(p.Version)
		if !npmNameRE.MatchString(name) {
			pkg.Origin = OriginUnknown
		}
		if cons, ok := direct[pathName]; ok && key == "node_modules/"+pathName {
			pkg.Direct, pkg.Constraint = true, trimText(cons, 80)
			seen[pathName] = true
			if fw, ok := npmFrameworks[name]; ok && pkg.Checkable {
				c.tech(Technology{Name: fw[0], Product: fw[1], Kind: "framework", Source: "package-lock.json",
					Declared: p.Version, Version: p.Version, Basis: BasisLocked})
			}
		}
		c.addPackage(pkg)
	}
	for name := range direct {
		if !seen[name] {
			if _, workspace := lock.Packages["node_modules/"+name]; !workspace {
				c.gap(AreaBoth, "npm: "+trimText(name, 80), "tiesioginės priklausomybės nėra package-lock.json: lock failas neatitinka manifesto")
			}
		}
	}
}

// npmOrigin classifies a lock entry by where it was resolved from. Only the
// public npm registry counts as public; an entry without a resolved URL has
// an unknown origin and is never named to an external service.
func npmOrigin(resolved string) string {
	if resolved == "" {
		return OriginUnknown
	}
	u, err := url.Parse(resolved)
	if err != nil {
		return OriginUnknown
	}
	if u.Scheme == "https" && u.Host == "registry.npmjs.org" && u.User == nil {
		return OriginPublic
	}
	if u.Scheme == "http" || u.Scheme == "https" {
		return OriginPrivate
	}
	return OriginUnknown
}

func reasonIfEmpty(v, reason string) string {
	if v == "" {
		return reason
	}
	return ""
}

var (
	exactRE  = regexp.MustCompile(`^v?(\d+\.\d+)(\.\d+){0,2}$`)
	pinnedRE = regexp.MustCompile(`^(?:[=v]?(\d+\.\d+)(?:\.\d+)?|(\d+\.\d+)\.(?:\*|x)|~(\d+\.\d+)\.\d+)$`)
)

// exactVersion returns the version when s is a plain version number.
func exactVersion(s string) string {
	s = strings.TrimSpace(s)
	if exactRE.MatchString(s) {
		return strings.TrimPrefix(s, "v")
	}
	return ""
}

// pinnedVersion returns major.minor when a constraint admits exactly one
// minor release line (8.2.*, ~8.2.0, 8.2.10), and "" for anything wider.
// A wider constraint is never narrowed to its lower bound.
func pinnedVersion(s string) string {
	m := pinnedRE.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return ""
	}
	for _, g := range m[1:] {
		if g != "" {
			return g
		}
	}
	return ""
}
