package assess

import (
	"strings"
	"testing"
	"time"

	"github.com/Ryttis/devatlas/internal/collect/stack"
	"github.com/Ryttis/devatlas/internal/sources"
)

var today = time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)

func lockedPkg(eco, name, version string, direct, dev bool) stack.Package {
	return stack.Package{ID: "P-" + name, Ecosystem: eco, Name: name, Version: version, Direct: direct, Dev: dev, Origin: stack.OriginPublic, Checkable: true}
}

func key(p stack.Package) string {
	pub, ok := PublicPackage(p)
	if !ok {
		panic("not public: " + p.Name)
	}
	return pub.Key()
}

func emptyExt() External {
	return External{Products: map[string]*sources.Product{}, Advisories: map[string][]sources.Advisory{},
		Registry: map[string]sources.RegistryInfo{}, RegistryMissing: map[string]bool{}}
}

func adv(id, sev string) sources.Advisory {
	return sources.Advisory{ID: id, Severity: sev, SeverityBasis: "test", Fixed: []string{"9.9.9"}, URL: "https://osv.dev/vulnerability/" + id}
}

func TestStatusMatrix(t *testing.T) {
	clean := lockedPkg("npm", "clean", "1.0.0", false, false)
	vuln := lockedPkg("npm", "vuln", "1.0.0", false, false)
	private := stack.Package{Ecosystem: "npm", Name: "@acme/ui", Version: "1.0.0", Origin: stack.OriginPrivate, Checkable: true}
	ext := emptyExt()
	ext.Advisories[key(vuln)] = []sources.Advisory{adv("GHSA-1", sources.SevHigh)}

	for _, tc := range []struct {
		name    string
		pkgs    []stack.Package
		gaps    []stack.Gap
		status  string
		partial bool
	}{
		{"complete, no findings", []stack.Package{clean}, nil, StatusGreen, false},
		{"incomplete, no findings", []stack.Package{clean, private}, nil, StatusUnverified, false},
		{"unsupported ecosystem, no findings", []stack.Package{clean}, []stack.Gap{{Area: stack.AreaBoth, Scope: "yarn", Reason: "x"}}, StatusUnverified, false},
		{"complete, findings", []stack.Package{clean, vuln}, nil, StatusYellow, false},
		{"incomplete, findings", []stack.Package{vuln, private}, nil, StatusYellow, true},
		{"nothing found at all", nil, []stack.Gap{{Area: stack.AreaSecurity, Scope: "priklausomybės", Reason: "nerasta"}}, StatusUnverified, false},
	} {
		ev := Evaluate(stack.Facts{Packages: tc.pkgs, Gaps: tc.gaps}, ext, today)
		if ev.Security.Status != tc.status || ev.Security.Partial != tc.partial || ev.Security.Reason == "" {
			t.Errorf("%s: got %s partial=%v (%s)", tc.name, ev.Security.Status, ev.Security.Partial, ev.Security.Reason)
		}
	}
}

func TestSecuritySeverityRules(t *testing.T) {
	dev := lockedPkg("composer", "acme/devtool", "1.0.0", false, true)
	unk := lockedPkg("composer", "acme/unknown", "1.0.0", false, false)
	ext := emptyExt()
	ext.Advisories[key(dev)] = []sources.Advisory{adv("GHSA-crit", sources.SevCritical)}
	ev := Evaluate(stack.Facts{Packages: []stack.Package{dev}}, ext, today)
	if ev.Security.Status != StatusRed || ev.Findings[0].Rule != "SEC-CRITICAL" {
		t.Errorf("a critical vulnerability in a dev package must stay urgent: %+v", ev.Security)
	}

	ext = emptyExt()
	ext.Advisories[key(unk)] = []sources.Advisory{{ID: "GHSA-unk", Severity: sources.SevUnknown, SeverityBasis: "nėra", URL: "u"}}
	ev = Evaluate(stack.Facts{Packages: []stack.Package{unk}}, ext, today)
	if ev.Security.Status != StatusYellow || len(ev.Findings) != 1 || ev.Findings[0].Rule != "SEC-UNKNOWN-SEVERITY" || !strings.Contains(ev.Findings[0].Action, "rankiniu") {
		t.Errorf("unknown severity must be a finding, never green: %+v %+v", ev.Security, ev.Findings)
	}
	for _, f := range ev.Findings {
		if f.ID == "" || len(f.Evidence) == 0 || len(f.Sources) == 0 || f.Action == "" {
			t.Errorf("finding lacks id, evidence, source, or action: %+v", f)
		}
	}
}

func phpFacts(kind, version string) stack.Facts {
	return stack.Facts{Technologies: []stack.Technology{{ID: "T1", Name: "PHP", Product: "php", Kind: kind, Source: "Dockerfile",
		Declared: "php:" + version, Version: version, Basis: stack.BasisImageTag}}}
}

func TestSupportDatesAreComparedWithToday(t *testing.T) {
	ext := emptyExt()
	ext.Products["php"] = &sources.Product{Slug: "php", URL: "https://endoflife.date/php", Releases: []sources.Release{
		{Name: "8.4", EOLFrom: "2028-12-31"}, {Name: "8.2", EOLFrom: "2026-12-31", EOASFrom: "2024-12-31"}}}
	facts := phpFacts("runtime", "8.2")

	for _, tc := range []struct {
		day    time.Time
		status string
		rule   string
	}{
		{time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC), StatusGreen, "FRESH-SECURITY-ONLY"},
		{today, StatusYellow, "FRESH-EOL-SOON"},
		{time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC), StatusRed, "FRESH-EOL-CRITICAL"},
		{time.Date(2027, 6, 1, 0, 0, 0, 0, time.UTC), StatusRed, "FRESH-EOL-CRITICAL"},
	} {
		ev := Evaluate(facts, ext, tc.day)
		if ev.Freshness.Status != tc.status || len(ev.Findings) != 1 || ev.Findings[0].Rule != tc.rule {
			t.Errorf("%s: got %s %+v, want %s %s", tc.day.Format("2006-01-02"), ev.Freshness.Status, ev.Findings, tc.status, tc.rule)
		}
	}
	// The service's stale boolean must not override a date that has passed.
	ext.Products["php"].Releases[1].IsEOL = false
	if ev := Evaluate(facts, ext, time.Date(2027, 6, 1, 0, 0, 0, 0, time.UTC)); ev.Freshness.Status != StatusRed {
		t.Error("the date, not the cached verdict, decides")
	}
	if ev := Evaluate(phpFacts("service", "8.2"), ext, time.Date(2027, 6, 1, 0, 0, 0, 0, time.UTC)); ev.Freshness.Status != StatusYellow || ev.Findings[0].Rule != "FRESH-EOL-OTHER" {
		t.Errorf("end of support of a non-critical component is a planned finding: %+v", ev.Findings)
	}
}

func TestUndeterminedVersionsAreGapsNotGuesses(t *testing.T) {
	ext := emptyExt()
	ext.Products["php"] = &sources.Product{Slug: "php", URL: "u", Releases: []sources.Release{{Name: "8.4", EOLFrom: "2028-12-31"}}}
	ext.Products["redis"] = &sources.Product{Slug: "redis", URL: "u", Releases: []sources.Release{{Name: "7.4", EOLFrom: "2028-01-01"}}}
	constraint := stack.Technology{ID: "T1", Name: "PHP", Product: "php", Kind: "runtime", Source: "composer.json", Declared: ">=8.1", Basis: stack.BasisConstraint, Reason: "platus"}

	ev := Evaluate(stack.Facts{Technologies: []stack.Technology{constraint}}, ext, today)
	if ev.Freshness.Status != StatusUnverified || len(ev.Freshness.Gaps) != 1 || ev.Technologies[0].Support != "unverified" {
		t.Errorf("a constraint alone must leave PHP unverified: %+v", ev.Freshness)
	}
	concrete := stack.Technology{ID: "T2", Name: "PHP", Product: "php", Kind: "runtime", Source: "Dockerfile", Declared: "php:8.4-fpm", Version: "8.4", Basis: stack.BasisImageTag}
	ev = Evaluate(stack.Facts{Technologies: []stack.Technology{constraint, concrete}}, ext, today)
	if ev.Freshness.Status != StatusGreen {
		t.Errorf("a concrete declaration makes the product verifiable: %+v", ev.Freshness)
	}
	majorOnly := stack.Technology{ID: "T3", Name: "Redis", Product: "redis", Kind: "database", Source: "compose.yaml", Declared: "redis:7-alpine", Version: "7", Basis: stack.BasisImageTag}
	ev = Evaluate(stack.Facts{Technologies: []stack.Technology{majorOnly}}, ext, today)
	if ev.Freshness.Status != StatusUnverified || ev.Technologies[0].Cycle != "" {
		t.Errorf("a major-only tag must not be matched to a minor cycle: %+v", ev.Technologies)
	}
	unknownImage := stack.Technology{ID: "T4", Name: "registry.internal/app:1", Kind: "image", Source: "compose.yaml", Declared: "registry.internal/app:1", Basis: stack.BasisImageTag, Reason: "neatpažintas"}
	ev = Evaluate(stack.Facts{Technologies: []stack.Technology{concrete, unknownImage}}, ext, today)
	if ev.Freshness.Status != StatusGreen {
		t.Errorf("an unrecognised image is outside the stated scope: %+v", ev.Freshness)
	}
}

func TestDirectDependencyFreshness(t *testing.T) {
	one := lockedPkg("composer", "acme/one", "v2.1.0", true, false)
	two := lockedPkg("composer", "acme/two", "1.0.0", true, false)
	types := lockedPkg("npm", "@types/node", "20.1.0", true, true)
	gone := lockedPkg("npm", "old", "1.0.0", true, false)
	lost := lockedPkg("npm", "lost", "1.0.0", true, false)
	ext := emptyExt()
	ext.Registry[key(one)] = sources.RegistryInfo{Latest: "3.0.0", DeprecationKnown: true, URL: "u"}
	ext.Registry[key(two)] = sources.RegistryInfo{Latest: "3.2.0", DeprecationKnown: true, URL: "u"}
	ext.Registry[key(types)] = sources.RegistryInfo{Latest: "26.0.0", DeprecationKnown: true, URL: "u"}
	ext.Registry[key(gone)] = sources.RegistryInfo{Latest: "1.0.0", Deprecated: true, DeprecationKnown: true, Note: "no longer maintained", URL: "u"}

	ev := Evaluate(stack.Facts{Packages: []stack.Package{one}}, ext, today)
	if ev.Freshness.Status != StatusGreen || len(ev.Findings) != 1 || ev.Findings[0].Level != LevelInfo {
		t.Errorf("one new major version is a note, not a problem: %+v %+v", ev.Freshness, ev.Findings)
	}
	ev = Evaluate(stack.Facts{Packages: []stack.Package{two, types, gone}}, ext, today)
	rules := map[string]string{}
	for _, f := range ev.Findings {
		rules[f.Subject] = f.Rule + "/" + f.Level
	}
	if ev.Freshness.Status != StatusYellow || rules["P-acme/two"] != "FRESH-MAJORS-BEHIND/yellow" || rules["P-old"] != "FRESH-DEPRECATED/yellow" || rules["P-@types/node"] != "FRESH-MAJOR-AVAILABLE/info" {
		t.Errorf("got %s %v", ev.Freshness.Status, rules)
	}
	ext.RegistryMissing[key(lost)] = true
	ev = Evaluate(stack.Facts{Packages: []stack.Package{lost}}, ext, today)
	if ev.Freshness.Status != StatusUnverified || ev.Security.Status != StatusGreen {
		t.Errorf("a direct dependency unknown to the registry leaves freshness unverified: %+v", ev.Freshness)
	}
}
