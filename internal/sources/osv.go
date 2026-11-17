package sources

import (
	"context"
	"encoding/json"
	"math"
	"regexp"
	"sort"
	"strings"
)

// Severity labels. Unknown is a severity that could not be established; it
// is not the absence of a vulnerability.
const (
	SevCritical = "critical"
	SevHigh     = "high"
	SevMedium   = "medium"
	SevLow      = "low"
	SevUnknown  = "unknown"
)

// Advisory is one known vulnerability affecting a locked package version.
type Advisory struct {
	ID       string   `json:"id"`
	Aliases  []string `json:"aliases,omitempty"`
	Summary  string   `json:"summary,omitempty"`
	Severity string   `json:"severity"`
	// SeverityBasis says where Severity came from, or why it is unknown.
	SeverityBasis string   `json:"severity_basis"`
	CVSS          float64  `json:"cvss,omitempty"`
	Fixed         []string `json:"fixed,omitempty"`
	URL           string   `json:"url"`
}

const (
	osvBatchSize  = 500
	osvMaxPages   = 5
	osvMaxDetails = 400
)

var osvIDRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{1,79}$`)

func osvEcosystem(e string) string {
	if e == Composer {
		return "Packagist"
	}
	return "npm"
}

type osvQuery struct {
	Package struct {
		Name      string `json:"name"`
		Ecosystem string `json:"ecosystem"`
	} `json:"package"`
	Version   string `json:"version"`
	PageToken string `json:"page_token,omitempty"`
}

// OSV returns the known vulnerabilities of each package, keyed by
// PublicPackage.Key. The batch endpoint returns identifiers only, possibly
// in several pages per query; details are then fetched per identifier.
// A package absent from the map has no known vulnerability.
func (r *Run) OSV(ctx context.Context, pkgs []PublicPackage) (map[string][]Advisory, error) {
	const source = "OSV"
	ids := map[string][]string{}
	type pending struct {
		pkg   PublicPackage
		token string
	}
	todo := make([]pending, len(pkgs))
	for i, p := range pkgs {
		todo[i] = pending{pkg: p}
	}
	for page := 0; len(todo) > 0; page++ {
		if page >= osvMaxPages {
			return nil, &Error{Source: source, Reason: "per daug rezultatų puslapių"}
		}
		var next []pending
		for start := 0; start < len(todo); start += osvBatchSize {
			chunk := todo[start:min(start+osvBatchSize, len(todo))]
			var body struct {
				Queries []osvQuery `json:"queries"`
			}
			for _, p := range chunk {
				var q osvQuery
				q.Package.Name, q.Package.Ecosystem = p.pkg.name, osvEcosystem(p.pkg.ecosystem)
				q.Version, q.PageToken = p.pkg.version, p.token
				body.Queries = append(body.Queries, q)
			}
			payload, _ := json.Marshal(body)
			data, err := r.do(ctx, source, "POST", r.c.OSV+"/v1/querybatch", payload, "application/json", maxPackagistBody)
			if err != nil {
				if err == ErrNotFound {
					err = &Error{Source: source, Reason: "paieškos adresas nerastas"}
				}
				return nil, err
			}
			var resp struct {
				Results []struct {
					Vulns []struct {
						ID string `json:"id"`
					} `json:"vulns"`
					NextPageToken string `json:"next_page_token"`
				} `json:"results"`
			}
			if err := json.Unmarshal(data, &resp); err != nil || len(resp.Results) != len(chunk) {
				return nil, &Error{Source: source, Reason: "atsakymo formatas neatpažintas"}
			}
			for i, res := range resp.Results {
				key := chunk[i].pkg.Key()
				for _, v := range res.Vulns {
					if !osvIDRE.MatchString(v.ID) {
						return nil, &Error{Source: source, Reason: "atsakymo formatas neatpažintas"}
					}
					ids[key] = append(ids[key], v.ID)
				}
				if res.NextPageToken != "" {
					next = append(next, pending{pkg: chunk[i].pkg, token: res.NextPageToken})
				}
			}
		}
		todo = next
	}

	unique := map[string]bool{}
	for _, list := range ids {
		for _, id := range list {
			unique[id] = true
		}
	}
	if len(unique) > osvMaxDetails {
		return nil, &Error{Source: source, Reason: "per daug pažeidžiamumų įrašų vienam vertinimui"}
	}
	details := map[string]*osvVuln{}
	for id := range unique {
		v, err := r.osvVuln(ctx, id)
		if err != nil {
			return nil, err
		}
		details[id] = v
	}

	out := map[string][]Advisory{}
	for _, p := range pkgs {
		list := ids[p.Key()]
		if len(list) == 0 {
			continue
		}
		// Prefer GHSA records, then drop records that are aliases of one
		// already kept, so one vulnerability is reported once.
		sort.Slice(list, func(i, j int) bool {
			gi, gj := strings.HasPrefix(list[i], "GHSA-"), strings.HasPrefix(list[j], "GHSA-")
			if gi != gj {
				return gi
			}
			return list[i] < list[j]
		})
		seen := map[string]bool{}
		var advs []Advisory
		for _, id := range list {
			v := details[id]
			if v != nil && v.Withdrawn != "" {
				continue
			}
			dup := seen[id]
			if v != nil {
				for _, a := range v.Aliases {
					dup = dup || seen[a]
				}
			}
			seen[id] = true
			if v != nil {
				for _, a := range v.Aliases {
					seen[a] = true
				}
			}
			if dup {
				continue
			}
			advs = append(advs, toAdvisory(id, v, p))
		}
		if len(advs) > 0 {
			out[p.Key()] = advs
		}
	}
	return out, nil
}

type osvVuln struct {
	Summary   string   `json:"summary"`
	Aliases   []string `json:"aliases"`
	Withdrawn string   `json:"withdrawn"`
	Severity  []struct {
		Type  string `json:"type"`
		Score string `json:"score"`
	} `json:"severity"`
	DatabaseSpecific struct {
		Severity string `json:"severity"`
	} `json:"database_specific"`
	Affected []struct {
		Package struct {
			Name      string `json:"name"`
			Ecosystem string `json:"ecosystem"`
		} `json:"package"`
		Ranges []struct {
			Events []map[string]string `json:"events"`
		} `json:"ranges"`
	} `json:"affected"`
}

// osvVuln fetches one record. A record that no longer exists yields nil:
// the vulnerability is still reported, with an unknown severity.
func (r *Run) osvVuln(ctx context.Context, id string) (*osvVuln, error) {
	if v, ok := r.c.cached("osv:" + id); ok {
		return v.(*osvVuln), nil
	}
	data, err := r.do(ctx, "OSV", "GET", r.c.OSV+"/v1/vulns/"+id, nil, "application/json", maxSmallBody)
	if err == ErrNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var v osvVuln
	if err := json.Unmarshal(data, &v); err != nil {
		return nil, &Error{Source: "OSV", Reason: "pažeidžiamumo įrašo formatas neatpažintas"}
	}
	r.c.store("osv:"+id, &v)
	return &v, nil
}

func toAdvisory(id string, v *osvVuln, p PublicPackage) Advisory {
	a := Advisory{ID: id, URL: "https://osv.dev/vulnerability/" + id, Severity: SevUnknown, SeverityBasis: "OSV įrašo detalės negautos"}
	if v == nil {
		return a
	}
	a.Summary = cleanText(v.Summary, 300)
	for _, al := range v.Aliases {
		if osvIDRE.MatchString(al) && len(a.Aliases) < 8 {
			a.Aliases = append(a.Aliases, al)
		}
	}
	a.Severity, a.SeverityBasis, a.CVSS = severity(v)
	fixed := map[string]bool{}
	for _, aff := range v.Affected {
		if !strings.EqualFold(aff.Package.Name, p.name) || aff.Package.Ecosystem != osvEcosystem(p.ecosystem) {
			continue
		}
		for _, rg := range aff.Ranges {
			for _, ev := range rg.Events {
				if f := ev["fixed"]; f != "" && versionRE.MatchString(f) && !fixed[f] && len(a.Fixed) < 12 {
					fixed[f] = true
					a.Fixed = append(a.Fixed, f)
				}
			}
		}
	}
	return a
}

var sevRank = map[string]int{SevUnknown: 0, SevLow: 1, SevMedium: 2, SevHigh: 3, SevCritical: 4}

// severity derives a label from the database's own rating and from a CVSS
// v3 vector, taking the higher of the two. OSV stores CVSS as a vector, not
// a number, so the v3 base score is computed here. A record with only a
// CVSS v4 vector or no rating at all stays unknown.
func severity(v *osvVuln) (label, basis string, score float64) {
	label, basis = SevUnknown, "OSV įraše nėra sunkumo įvertinimo"
	switch strings.ToUpper(v.DatabaseSpecific.Severity) {
	case "CRITICAL":
		label, basis = SevCritical, "duomenų bazės įvertinimas"
	case "HIGH":
		label, basis = SevHigh, "duomenų bazės įvertinimas"
	case "MODERATE", "MEDIUM":
		label, basis = SevMedium, "duomenų bazės įvertinimas"
	case "LOW":
		label, basis = SevLow, "duomenų bazės įvertinimas"
	}
	onlyV4 := false
	for _, s := range v.Severity {
		if s.Type == "CVSS_V4" {
			onlyV4 = true
		}
		if s.Type != "CVSS_V3" {
			continue
		}
		onlyV4 = false
		sc, ok := CVSS3(s.Score)
		if !ok {
			continue
		}
		score = math.Max(score, sc)
		l := SevLow
		switch {
		case sc >= 9:
			l = SevCritical
		case sc >= 7:
			l = SevHigh
		case sc >= 4:
			l = SevMedium
		case sc == 0:
			continue
		}
		if sevRank[l] > sevRank[label] {
			label, basis = l, "CVSS v3 bazinis balas"
		}
	}
	if label == SevUnknown && onlyV4 {
		basis = "pateiktas tik CVSS v4 vektorius, kurio balas neskaičiuojamas"
	}
	return label, basis, score
}

// CVSS3 computes the base score of a CVSS v3.0 or v3.1 vector.
func CVSS3(vector string) (float64, bool) {
	parts := strings.Split(vector, "/")
	if len(parts) < 9 || !strings.HasPrefix(parts[0], "CVSS:3.") {
		return 0, false
	}
	m := map[string]string{}
	for _, p := range parts[1:] {
		k, v, ok := strings.Cut(p, ":")
		if !ok {
			return 0, false
		}
		m[k] = v
	}
	changed := m["S"] == "C"
	if m["S"] != "C" && m["S"] != "U" {
		return 0, false
	}
	av, ok1 := map[string]float64{"N": 0.85, "A": 0.62, "L": 0.55, "P": 0.2}[m["AV"]]
	ac, ok2 := map[string]float64{"L": 0.77, "H": 0.44}[m["AC"]]
	ui, ok3 := map[string]float64{"N": 0.85, "R": 0.62}[m["UI"]]
	prTable := map[string]float64{"N": 0.85, "L": 0.62, "H": 0.27}
	if changed {
		prTable = map[string]float64{"N": 0.85, "L": 0.68, "H": 0.5}
	}
	pr, ok4 := prTable[m["PR"]]
	cia := map[string]float64{"H": 0.56, "L": 0.22, "N": 0}
	cc, ok5 := cia[m["C"]]
	ii, ok6 := cia[m["I"]]
	aa, ok7 := cia[m["A"]]
	if !(ok1 && ok2 && ok3 && ok4 && ok5 && ok6 && ok7) {
		return 0, false
	}
	iss := 1 - (1-cc)*(1-ii)*(1-aa)
	impact := 6.42 * iss
	if changed {
		impact = 7.52*(iss-0.029) - 3.25*math.Pow(iss-0.02, 15)
	}
	if impact <= 0 {
		return 0, true
	}
	expl := 8.22 * av * ac * pr * ui
	total := impact + expl
	if changed {
		total *= 1.08
	}
	return math.Ceil(math.Min(total, 10)*10-1e-9) / 10, true
}
