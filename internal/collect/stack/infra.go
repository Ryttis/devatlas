package stack

import (
	"bufio"
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"
)

func isDockerfile(name string) bool {
	return name == "Dockerfile" || strings.HasPrefix(name, "Dockerfile.") || strings.HasSuffix(name, ".Dockerfile") || strings.HasSuffix(name, ".dockerfile")
}

func isCompose(name string) bool {
	if !strings.HasSuffix(name, ".yml") && !strings.HasSuffix(name, ".yaml") {
		return false
	}
	return strings.HasPrefix(name, "compose") || strings.HasPrefix(name, "docker-compose")
}

// images maps an official Docker Hub image to a display name, an
// endoflife.date product, and a kind.
var images = map[string][3]string{
	"php":           {"PHP", "php", "runtime"},
	"node":          {"Node.js", "nodejs", "runtime"},
	"python":        {"Python", "python", "runtime"},
	"golang":        {"Go", "go", "runtime"},
	"mariadb":       {"MariaDB", "mariadb", "database"},
	"mysql":         {"MySQL", "mysql", "database"},
	"postgres":      {"PostgreSQL", "postgresql", "database"},
	"mongo":         {"MongoDB", "mongodb", "database"},
	"redis":         {"Redis", "redis", "database"},
	"valkey":        {"Valkey", "valkey", "database"},
	"memcached":     {"Memcached", "memcached", "database"},
	"elasticsearch": {"Elasticsearch", "elasticsearch", "database"},
	"nginx":         {"nginx", "nginx", "service"},
	"httpd":         {"Apache HTTP Server", "apache-http-server", "service"},
	"rabbitmq":      {"RabbitMQ", "rabbitmq", "service"},
	"traefik":       {"Traefik", "traefik", "service"},
	"alpine":        {"Alpine Linux", "alpine-linux", "base-image"},
	"debian":        {"Debian", "debian", "base-image"},
	"ubuntu":        {"Ubuntu", "ubuntu", "base-image"},
}

var fullImages = map[string][3]string{
	"docker.elastic.co/elasticsearch/elasticsearch": {"Elasticsearch", "elasticsearch", "database"},
	"opensearchproject/opensearch":                  {"OpenSearch", "opensearch", "database"},
	"docker.io/opensearchproject/opensearch":        {"OpenSearch", "opensearch", "database"},
	"valkey/valkey":                                 {"Valkey", "valkey", "database"},
	"docker.io/valkey/valkey":                       {"Valkey", "valkey", "database"},
}

var tagVersionRE = regexp.MustCompile(`^v?(\d+(?:\.\d+){0,2})(?:$|[-_.])`)

// imageTech turns an image reference into a technology. basis says how the
// reference was obtained. Nothing is inferred for a missing, symbolic, or
// variable tag.
func imageTech(ref, source, basis string) Technology {
	t := Technology{Name: trimText(ref, 120), Kind: "image", Source: source, Declared: trimText(ref, 120), Basis: basis}
	if strings.Contains(ref, "$") {
		t.Reason = "image nuoroda priklauso nuo kintamojo; reikšmė neskaitoma iš aplinkos"
		if i := strings.IndexAny(ref, ":$"); i > 0 {
			if e, ok := lookupImage(ref[:i]); ok && !strings.Contains(ref[:i], "$") {
				t.Name, t.Product, t.Kind = e[0], e[1], e[2]
			}
		}
		return t
	}
	repo := ref
	if i := strings.Index(repo, "@"); i >= 0 {
		repo = repo[:i]
	}
	tag := ""
	if i := strings.LastIndex(repo, ":"); i > strings.LastIndex(repo, "/") {
		repo, tag = repo[:i], repo[i+1:]
	}
	e, ok := lookupImage(repo)
	if !ok {
		t.Reason = "neatpažintas arba privatus image: palaikymo duomenų šaltinio nėra, komponentas už patikros ribų"
		return t
	}
	t.Name, t.Product, t.Kind = e[0], e[1], e[2]
	switch m := tagVersionRE.FindStringSubmatch(tag); {
	case tag == "":
		t.Reason = "image be žymos: versija nedeklaruota"
	case m == nil:
		t.Reason = "žyma „" + trimText(tag, 40) + "“ nenurodo versijos"
	default:
		t.Version = m[1]
	}
	return t
}

func lookupImage(repo string) ([3]string, bool) {
	repo = strings.ToLower(repo)
	if e, ok := fullImages[repo]; ok {
		return e, true
	}
	repo = strings.TrimPrefix(repo, "docker.io/")
	repo = strings.TrimPrefix(repo, "library/")
	if strings.Contains(repo, "/") {
		return [3]string{}, false
	}
	e, ok := images[repo]
	return e, ok
}

type composeFile struct {
	Services map[string]struct {
		Image string    `yaml:"image"`
		Build yaml.Node `yaml:"build"`
	} `yaml:"services"`
}

// infra parses Dockerfiles and Compose files in the root and those found in
// subdirectories or referenced by a Compose build section.
func (c *collector) infra(names map[string]os.FileMode, sub []string) {
	var composes, dockerfiles []string
	for n, mode := range names {
		if !mode.IsRegular() {
			if mode&os.ModeSymlink != 0 && (isCompose(n) || isDockerfile(n)) {
				c.gap(AreaFreshness, n, "failas atmestas: "+errSymlink.Error())
			}
			continue
		}
		if isCompose(n) {
			composes = append(composes, n)
		} else if isDockerfile(n) {
			dockerfiles = append(dockerfiles, n)
		}
	}
	for _, p := range sub {
		if isCompose(filepath.Base(p)) {
			composes = append(composes, p)
		} else {
			dockerfiles = append(dockerfiles, p)
		}
	}
	sort.Strings(composes)
	budget := maxInfraFiles
	for _, p := range composes {
		if budget--; budget < 0 {
			break
		}
		dockerfiles = append(dockerfiles, c.compose(p)...)
	}
	sort.Strings(dockerfiles)
	done := map[string]bool{}
	for _, p := range dockerfiles {
		p = filepath.Clean(p)
		if done[p] {
			continue
		}
		done[p] = true
		if budget--; budget < 0 {
			c.gap(AreaFreshness, "infrastruktūros failai", "viršyta tikrinamų Dockerfile ir Compose failų riba")
			break
		}
		c.dockerfile(p)
	}
}

// compose records image declarations and returns Dockerfiles referenced by
// build sections. Environment values are never decoded.
func (c *collector) compose(rel string) []string {
	slash := filepath.ToSlash(rel)
	data, err := c.read(rel, "compose", MaxManifestBytes)
	if err != nil {
		c.gap(AreaFreshness, slash, readReason(err))
		return nil
	}
	var doc composeFile
	if err := yaml.Unmarshal(data, &doc); err != nil {
		c.gap(AreaFreshness, slash, "Compose failas nėra galiojantis YAML")
		return nil
	}
	svcNames := make([]string, 0, len(doc.Services))
	for n := range doc.Services {
		svcNames = append(svcNames, n)
	}
	sort.Strings(svcNames)
	var refs []string
	for _, n := range svcNames {
		svc := doc.Services[n]
		hasBuild := svc.Build.Kind != 0
		// An image name next to a build section names the build result.
		if svc.Image != "" && !hasBuild {
			c.tech(imageTech(strings.TrimSpace(svc.Image), slash, BasisImageTag))
		}
		if !hasBuild {
			continue
		}
		ctxDir, file := "", "Dockerfile"
		switch svc.Build.Kind {
		case yaml.ScalarNode:
			ctxDir = svc.Build.Value
		case yaml.MappingNode:
			var b struct {
				Context    string `yaml:"context"`
				Dockerfile string `yaml:"dockerfile"`
			}
			if svc.Build.Decode(&b) != nil {
				continue
			}
			ctxDir = b.Context
			if b.Dockerfile != "" {
				file = b.Dockerfile
			}
		}
		if strings.Contains(ctxDir, "$") || strings.Contains(file, "$") || strings.Contains(ctxDir, "://") || filepath.IsAbs(ctxDir) || filepath.IsAbs(file) {
			continue
		}
		p := filepath.Join(filepath.Dir(rel), ctxDir, file)
		if p == ".." || strings.HasPrefix(p, ".."+string(filepath.Separator)) {
			continue // outside the checkout
		}
		refs = append(refs, p)
	}
	return refs
}

var argRefRE = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(?::?-([^}]*))?\}|\$([A-Za-z_][A-Za-z0-9_]*)`)

// dockerfile records the base image of every build stage. A tag written
// with ARG variables is resolved from ARG defaults declared before the FROM
// line and labelled as such; a build argument can still override it.
func (c *collector) dockerfile(rel string) {
	slash := filepath.ToSlash(rel)
	data, err := c.read(rel, "dockerfile", MaxManifestBytes)
	if err != nil {
		if !os.IsNotExist(err) {
			c.gap(AreaFreshness, slash, readReason(err))
		}
		return
	}
	args := map[string]string{}
	stages := map[string]bool{}
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 {
			continue
		}
		switch strings.ToUpper(fields[0]) {
		case "ARG":
			if k, v, ok := strings.Cut(fields[1], "="); ok {
				args[k] = strings.Trim(v, `"'`)
			}
		case "FROM":
			rest := fields[1:]
			for len(rest) > 0 && strings.HasPrefix(rest[0], "--") {
				rest = rest[1:]
			}
			if len(rest) == 0 {
				continue
			}
			ref := rest[0]
			if len(rest) >= 3 && strings.EqualFold(rest[1], "AS") {
				stages[strings.ToLower(rest[2])] = true
			}
			if ref == "scratch" || stages[strings.ToLower(ref)] {
				continue
			}
			basis := BasisImageTag
			if strings.Contains(ref, "$") {
				resolved := argRefRE.ReplaceAllStringFunc(ref, func(m string) string {
					g := argRefRE.FindStringSubmatch(m)
					name := g[1] + g[3]
					if v, ok := args[name]; ok && v != "" {
						return v
					}
					if g[2] != "" {
						return g[2]
					}
					return m
				})
				if !strings.Contains(resolved, "$") {
					ref, basis = resolved, BasisArgDefault
				}
			}
			c.tech(imageTech(ref, slash, basis))
		}
	}
}
