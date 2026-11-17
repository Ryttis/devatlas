package sources

import (
	"context"
	"encoding/json"
	"net/url"
	"strconv"
	"strings"
)

// RegistryInfo is what a package registry says about a direct dependency.
type RegistryInfo struct {
	// Latest is the newest stable release.
	Latest string `json:"latest,omitempty"`
	// Deprecated is true when the registry marks the package abandoned
	// (Packagist) or the locked version deprecated (npm).
	Deprecated bool `json:"deprecated"`
	// DeprecationKnown is false when the registry could not say whether
	// the locked version is deprecated.
	DeprecationKnown bool   `json:"deprecation_known"`
	Note             string `json:"note,omitempty"`
	URL              string `json:"url"`
}

// Registry looks a public package up in its registry.
func (r *Run) Registry(ctx context.Context, p PublicPackage) (RegistryInfo, error) {
	key := "reg:" + p.Key()
	if v, ok := r.c.cached(key); ok {
		return v.(RegistryInfo), nil
	}
	var info RegistryInfo
	var err error
	if p.ecosystem == Composer {
		info, err = r.packagist(ctx, p)
	} else {
		info, err = r.npm(ctx, p)
	}
	if err == nil {
		r.c.store(key, info)
	}
	return info, err
}

// npm asks for the manifest of the "latest" dist-tag and for the manifest
// of the locked version. Both are small documents; the full package
// document, which can be many megabytes, is never requested. Deprecation is
// a property of a version, so it is read from the locked version's manifest.
func (r *Run) npm(ctx context.Context, p PublicPackage) (RegistryInfo, error) {
	const source = "npm registras"
	base := r.c.NPM + "/" + npmPath(p.name)
	info := RegistryInfo{URL: "https://www.npmjs.com/package/" + p.name}
	type manifest struct {
		Version    string          `json:"version"`
		Deprecated json.RawMessage `json:"deprecated"`
	}
	data, err := r.do(ctx, source, "GET", base+"/latest", nil, "application/json", maxSmallBody)
	if err != nil {
		return info, err
	}
	var latest manifest
	if json.Unmarshal(data, &latest) != nil || !versionRE.MatchString(latest.Version) {
		return info, &Error{Source: source, Reason: "atsakymo formatas neatpažintas"}
	}
	info.Latest = latest.Version
	data, err = r.do(ctx, source, "GET", base+"/"+url.PathEscape(p.version), nil, "application/json", maxSmallBody)
	if err == ErrNotFound {
		info.Note = "užrakintos versijos registre nėra"
		return info, nil
	}
	if err != nil {
		return info, err
	}
	var locked manifest
	if json.Unmarshal(data, &locked) != nil {
		return info, &Error{Source: source, Reason: "atsakymo formatas neatpažintas"}
	}
	info.DeprecationKnown = true
	var note string
	if json.Unmarshal(locked.Deprecated, &note) == nil && note != "" {
		info.Deprecated, info.Note = true, cleanText(note, 240)
	}
	return info, nil
}

func npmPath(name string) string {
	if scope, rest, ok := strings.Cut(name, "/"); ok {
		return url.PathEscape(scope) + "/" + url.PathEscape(rest)
	}
	return url.PathEscape(name)
}

// packagist reads the Composer v2 metadata file of a package. That file
// lists tagged releases newest first and is minified: each entry holds only
// the keys that differ from the previous one, so only the first entry is
// complete. "abandoned" is therefore read from the first entry, and the
// latest stable release is found by comparing version numbers, since the
// first entry is often a pre-release.
func (r *Run) packagist(ctx context.Context, p PublicPackage) (RegistryInfo, error) {
	const source = "Packagist"
	info := RegistryInfo{URL: "https://packagist.org/packages/" + p.name}
	data, err := r.do(ctx, source, "GET", r.c.Packagist+"/p2/"+p.name+".json", nil, "application/json", maxPackagistBody)
	if err != nil {
		return info, err
	}
	var doc struct {
		Packages map[string][]struct {
			Version   string          `json:"version"`
			Abandoned json.RawMessage `json:"abandoned"`
		} `json:"packages"`
	}
	if json.Unmarshal(data, &doc) != nil {
		return info, &Error{Source: source, Reason: "atsakymo formatas neatpažintas"}
	}
	versions := doc.Packages[p.name]
	if len(versions) == 0 {
		return info, ErrNotFound
	}
	info.DeprecationKnown = true
	if ab := versions[0].Abandoned; len(ab) > 0 {
		var flag bool
		var repl string
		if json.Unmarshal(ab, &flag) == nil && flag {
			info.Deprecated, info.Note = true, "paketas pažymėtas kaip apleistas (abandoned)"
		} else if json.Unmarshal(ab, &repl) == nil && repl != "" {
			info.Deprecated = true
			info.Note = "paketas pažymėtas kaip apleistas (abandoned); siūlomas pakaitalas: " + cleanText(repl, 120)
		}
	}
	var best []int
	for _, v := range versions {
		nums, ok := StableVersion(v.Version)
		if ok && (best == nil || CompareVersions(nums, best) > 0) {
			best, info.Latest = nums, strings.TrimPrefix(v.Version, "v")
		}
	}
	return info, nil
}

// StableVersion parses a plain release version (1.2.3, v1.2, 1.2.3.4) into
// its numeric parts. Pre-releases, branch aliases, and anything with a
// suffix are not stable versions.
func StableVersion(v string) ([]int, bool) {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if v == "" {
		return nil, false
	}
	parts := strings.Split(v, ".")
	if len(parts) > 4 {
		return nil, false
	}
	nums := make([]int, len(parts))
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 || len(p) > 9 {
			return nil, false
		}
		nums[i] = n
	}
	return nums, true
}

// CompareVersions compares numeric version parts; missing parts count as 0.
func CompareVersions(a, b []int) int {
	for i := 0; i < len(a) || i < len(b); i++ {
		var x, y int
		if i < len(a) {
			x = a[i]
		}
		if i < len(b) {
			y = b[i]
		}
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	return 0
}

// Major returns the leading number of a version, with or without a suffix.
func Major(v string) (int, bool) {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	end := 0
	for end < len(v) && v[end] >= '0' && v[end] <= '9' {
		end++
	}
	if end == 0 || end > 9 {
		return 0, false
	}
	n, err := strconv.Atoi(v[:end])
	return n, err == nil
}
