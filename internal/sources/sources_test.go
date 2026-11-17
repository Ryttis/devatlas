package sources_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Ryttis/devatlas/internal/sources"
	"github.com/Ryttis/devatlas/internal/sources/sourcestest"
)

func pub(t *testing.T, eco, name, version string) sources.PublicPackage {
	t.Helper()
	p, ok := sources.Publishable(eco, name, version, true)
	if !ok {
		t.Fatalf("%s %s@%s should be publishable", eco, name, version)
	}
	return p
}

func TestPublishableRefusesPrivateUnknownAndMalformed(t *testing.T) {
	for _, tc := range []struct {
		eco, name, version string
		public             bool
	}{
		{sources.Composer, "acme/private", "1.0.0", false},
		{sources.NPM, "@acme/ui", "1.0.0", false},
		{sources.Composer, "acme/x", "dev-main", true},
		{sources.Composer, "not a name", "1.0.0", true},
		{sources.NPM, "../../etc/passwd", "1.0.0", true},
		{sources.NPM, "ok", "1.0.0?token=abc", true},
		{sources.NPM, "ok", "https://internal.example/x.tgz", true},
		{"pypi", "requests", "2.0.0", true},
	} {
		if _, ok := sources.Publishable(tc.eco, tc.name, tc.version, tc.public); ok {
			t.Errorf("%+v must not be publishable", tc)
		}
	}
	if (sources.PublicPackage{}).Name() != "" {
		t.Error("the zero value must be empty")
	}
}

func TestCVSS3(t *testing.T) {
	for vec, want := range map[string]float64{
		"CVSS:3.1/AV:N/AC:L/PR:N/UI:N/S:U/C:H/I:H/A:H": 9.8,
		"CVSS:3.1/AV:N/AC:H/PR:N/UI:N/S:U/C:N/I:H/A:H": 7.4,
		"CVSS:3.1/AV:N/AC:L/PR:N/UI:R/S:C/C:L/I:L/A:N": 6.1,
		"CVSS:3.0/AV:N/AC:L/PR:N/UI:N/S:C/C:H/I:H/A:H": 10.0,
		"CVSS:3.1/AV:L/AC:H/PR:H/UI:R/S:U/C:N/I:N/A:N": 0,
	} {
		got, ok := sources.CVSS3(vec)
		if !ok || got != want {
			t.Errorf("%s = %v (%v), want %v", vec, got, ok, want)
		}
	}
	for _, bad := range []string{"", "CVSS:4.0/AV:N/AC:L/AT:N/PR:N/UI:N/VC:H/VI:H/VA:H/SC:N/SI:N/SA:N", "CVSS:3.1/AV:X/AC:L/PR:N/UI:N/S:U/C:H/I:H/A:H"} {
		if _, ok := sources.CVSS3(bad); ok {
			t.Errorf("%q must not parse", bad)
		}
	}
}

func TestOSVPaginationDedupeSeverityAndWithdrawn(t *testing.T) {
	f := sourcestest.New()
	defer f.Server.Close()
	f.PageSize = 2
	f.Vulns["npm|lodash|4.17.15"] = []string{"GHSA-aaaa-aaaa-aaaa", "CVE-2020-0001", "GHSA-bbbb-bbbb-bbbb", "GHSA-cccc-cccc-cccc", "GHSA-dddd-dddd-dddd", "GHSA-gone-gone-gone"}
	f.Details["GHSA-aaaa-aaaa-aaaa"] = sourcestest.VulnDoc("GHSA-aaaa-aaaa-aaaa", "lodash", "npm", "HIGH", "CVSS:3.1/AV:N/AC:L/PR:N/UI:N/S:U/C:H/I:H/A:H", "4.17.19", "CVE-2020-0001")
	f.Details["CVE-2020-0001"] = sourcestest.VulnDoc("CVE-2020-0001", "lodash", "npm", "", "", "4.17.19", "GHSA-aaaa-aaaa-aaaa")
	f.Details["GHSA-bbbb-bbbb-bbbb"] = sourcestest.VulnDoc("GHSA-bbbb-bbbb-bbbb", "lodash", "npm", "", "CVSS:4.0/AV:N/AC:L/AT:N/PR:N/UI:N/VC:H/VI:H/VA:H/SC:N/SI:N/SA:N", "4.17.21")
	f.Details["GHSA-cccc-cccc-cccc"] = sourcestest.VulnDoc("GHSA-cccc-cccc-cccc", "lodash", "npm", "MODERATE", "", "4.17.21")
	f.Details["GHSA-dddd-dddd-dddd"] = strings.Replace(sourcestest.VulnDoc("GHSA-dddd-dddd-dddd", "lodash", "npm", "CRITICAL", "", "5.0.0"), `{`, `{"withdrawn":"2024-01-01T00:00:00Z",`, 1)
	// GHSA-gone-gone-gone has no details record.

	lodash, clean := pub(t, sources.NPM, "lodash", "4.17.15"), pub(t, sources.NPM, "left-pad", "1.3.0")
	run := f.Client().NewRun(0)
	got, err := run.OSV(context.Background(), []sources.PublicPackage{lodash, clean})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got[clean.Key()]; ok {
		t.Error("a package without vulnerabilities must be absent")
	}
	by := map[string]sources.Advisory{}
	for _, a := range got[lodash.Key()] {
		by[a.ID] = a
	}
	if len(by) != 4 {
		t.Fatalf("want 4 advisories (alias and withdrawn removed), got %v", by)
	}
	if a := by["GHSA-aaaa-aaaa-aaaa"]; a.Severity != sources.SevCritical || a.CVSS != 9.8 || len(a.Fixed) != 1 || a.Fixed[0] != "4.17.19" {
		t.Errorf("the higher of the label and the computed CVSS score must win: %+v", a)
	}
	if a := by["GHSA-bbbb-bbbb-bbbb"]; a.Severity != sources.SevUnknown || !strings.Contains(a.SeverityBasis, "CVSS v4") {
		t.Errorf("a CVSS v4-only record has unknown severity: %+v", a)
	}
	if a := by["GHSA-cccc-cccc-cccc"]; a.Severity != sources.SevMedium {
		t.Errorf("label severity: %+v", a)
	}
	if a, ok := by["GHSA-gone-gone-gone"]; !ok || a.Severity != sources.SevUnknown {
		t.Errorf("a record without details is still a vulnerability of unknown severity: %+v", a)
	}
	if f.Count("/v1/querybatch") != 2 {
		t.Errorf("expected a second batch for the next page, got %d", f.Count("/v1/querybatch"))
	}
}

func TestRetriesBudgetAndFailures(t *testing.T) {
	f := sourcestest.New()
	defer f.Server.Close()
	f.EOL["php"] = sourcestest.EOLDoc("PHP", [3]string{"8.3", "2027-12-31", "2025-12-31"})
	f.Fail["/api/v1/products/php"] = 2
	c := f.Client()
	p, err := c.NewRun(0).EOL(context.Background(), "php")
	if err != nil || p.Releases[0].EOLFrom != "2027-12-31" || p.URL != "https://endoflife.date/php" {
		t.Fatalf("retry should succeed: %+v, %v", p, err)
	}
	if n := f.Count("/api/v1/products/php"); n != 3 {
		t.Errorf("want 3 requests (2 failures + success), got %d", n)
	}
	// Cached: a second lookup sends nothing.
	if _, err := c.NewRun(0).EOL(context.Background(), "php"); err != nil || f.Count("/api/v1/products/php") != 3 {
		t.Errorf("cache miss: %v", err)
	}

	f.Set(func(f *sourcestest.Fake) { f.Down = true })
	var se *sources.Error
	if _, err := c.NewRun(0).EOL(context.Background(), "nodejs"); !errors.As(err, &se) || se.Source != "endoflife.date" {
		t.Fatalf("an unreachable source must be an *Error, got %v", err)
	}
	if n := f.Count("/api/v1/products/nodejs"); n != 3 {
		t.Errorf("retries must be bounded at 3 requests, got %d", n)
	}
	f.Set(func(f *sourcestest.Fake) { f.Down = false })
	if _, err := c.NewRun(0).EOL(context.Background(), "unknown-product"); !errors.Is(err, sources.ErrNotFound) {
		t.Errorf("404 must be ErrNotFound, got %v", err)
	}
	run := c.NewRun(1)
	_, _ = run.EOL(context.Background(), "mariadb")
	if _, err := run.EOL(context.Background(), "mysql"); !errors.As(err, &se) || !strings.Contains(se.Reason, "riba") {
		t.Errorf("the request budget must stop the run, got %v", err)
	}
	if _, err := c.NewRun(0).EOL(context.Background(), "../secret"); err == nil || f.Count("/api/v1/products/..") != 0 {
		t.Error("an invalid product name must not be requested")
	}
}

func TestEOLNullableFields(t *testing.T) {
	f := sourcestest.New()
	defer f.Server.Close()
	f.EOL["nginx"] = `{"result":{"label":"nginx","releases":[{"name":"1.31","isEol":false,"eolFrom":null,"isEoas":null,"eoasFrom":null,"latest":null},{"name":"1.29","isEol":true,"eolFrom":"2026-05-13"}]}}`
	p, err := f.Client().NewRun(0).EOL(context.Background(), "nginx")
	if err != nil || len(p.Releases) != 2 || p.Releases[0].EOLFrom != "" || p.Releases[0].IsEOL || !p.Releases[1].IsEOL {
		t.Fatalf("%+v %v", p, err)
	}
}

func TestNpmUsesSmallManifestsAndLockedVersionDeprecation(t *testing.T) {
	f := sourcestest.New()
	defer f.Server.Close()
	f.NPM["@scope/pkg/latest"] = `{"version":"3.1.0"}`
	f.NPM["@scope/pkg/1.2.0"] = `{"version":"1.2.0","deprecated":"use something else"}`
	f.NPM["fine/latest"] = `{"version":"2.0.0","deprecated":"latest is deprecated but the locked one is not"}`
	f.NPM["fine/1.0.0"] = `{"version":"1.0.0"}`
	f.NPM["gone/latest"] = `{"version":"2.0.0"}`
	run := f.Client().NewRun(0)

	info, err := run.Registry(context.Background(), pub(t, sources.NPM, "@scope/pkg", "1.2.0"))
	if err != nil || info.Latest != "3.1.0" || !info.Deprecated || !info.DeprecationKnown || info.Note != "use something else" {
		t.Fatalf("%+v %v", info, err)
	}
	info, err = run.Registry(context.Background(), pub(t, sources.NPM, "fine", "1.0.0"))
	if err != nil || info.Deprecated || !info.DeprecationKnown {
		t.Fatalf("deprecation belongs to the locked version: %+v %v", info, err)
	}
	info, err = run.Registry(context.Background(), pub(t, sources.NPM, "gone", "1.0.0"))
	if err != nil || info.DeprecationKnown || info.Latest != "2.0.0" {
		t.Fatalf("an unknown locked version leaves deprecation unknown: %+v %v", info, err)
	}
	if _, err := run.Registry(context.Background(), pub(t, sources.NPM, "missing", "1.0.0")); !errors.Is(err, sources.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	if seen := f.Seen(); strings.Contains(seen, "GET /@scope/pkg\n") || strings.Contains(seen, "GET /fine\n") {
		t.Error("the full package document must never be requested")
	}
}

func TestPackagistMinifiedMetadata(t *testing.T) {
	f := sourcestest.New()
	defer f.Server.Close()
	// Minified: only the first entry is complete; later entries carry diffs.
	f.Packagist["acme/lib"] = `{"minified":"composer/2.0","packages":{"acme/lib":[
	  {"name":"acme/lib","version":"v4.0.0-BETA1","abandoned":"acme/new-lib"},
	  {"version":"v3.2.1"},{"version":"3.10.0"},{"version":"v3.9.9"},{"version":"dev-main"},{"version":"v2.0.0","abandoned":"__unset"}]}}`
	f.Packagist["acme/ok"] = `{"packages":{"acme/ok":[{"name":"acme/ok","version":"1.4.0"},{"version":"1.3.0"}]}}`
	f.Packagist["acme/empty"] = `{"packages":{}}`
	run := f.Client().NewRun(0)

	info, err := run.Registry(context.Background(), pub(t, sources.Composer, "acme/lib", "v3.2.1"))
	if err != nil || info.Latest != "3.10.0" || !info.Deprecated || !strings.Contains(info.Note, "acme/new-lib") {
		t.Fatalf("latest stable must skip pre-releases and compare numerically: %+v %v", info, err)
	}
	info, err = run.Registry(context.Background(), pub(t, sources.Composer, "acme/ok", "1.3.0"))
	if err != nil || info.Latest != "1.4.0" || info.Deprecated || !info.DeprecationKnown {
		t.Fatalf("%+v %v", info, err)
	}
	if _, err := run.Registry(context.Background(), pub(t, sources.Composer, "acme/empty", "1.0.0")); !errors.Is(err, sources.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestMalformedAnswersAreErrorsNotEmptyResults(t *testing.T) {
	f := sourcestest.New()
	defer f.Server.Close()
	f.EOL["php"] = `{"result":{"releases":[]}}`
	f.NPM["x/latest"] = `<html>`
	f.Vulns["npm|x|1.0.0"] = []string{"bad id with spaces"}
	run := f.Client().NewRun(0)
	var se *sources.Error
	if _, err := run.EOL(context.Background(), "php"); !errors.As(err, &se) {
		t.Errorf("EOL: %v", err)
	}
	if _, err := run.Registry(context.Background(), pub(t, sources.NPM, "x", "1.0.0")); !errors.As(err, &se) {
		t.Errorf("npm: %v", err)
	}
	if _, err := run.OSV(context.Background(), []sources.PublicPackage{pub(t, sources.NPM, "x", "1.0.0")}); !errors.As(err, &se) {
		t.Errorf("OSV: %v", err)
	}
}
