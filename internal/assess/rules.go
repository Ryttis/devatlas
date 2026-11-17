package assess

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Ryttis/devatlas/internal/collect/stack"
	"github.com/Ryttis/devatlas/internal/sources"
)

// Thresholds of the rules.
const (
	// EOLSoonDays is how close an end of support must be for a planned
	// update finding.
	EOLSoonDays = 180
	// MajorsBehind is how many major versions a direct dependency must lag
	// for a planned update finding. One major behind is a note only.
	MajorsBehind = 2
	maxListed    = 8
)

// criticalKinds are components whose end of support is urgent.
var criticalKinds = map[string]bool{"runtime": true, "framework": true, "database": true}

// External is everything the external sources returned for one run.
type External struct {
	Products   map[string]*sources.Product
	Advisories map[string][]sources.Advisory
	Registry   map[string]sources.RegistryInfo
	// RegistryMissing lists public direct packages the registry does not know.
	RegistryMissing map[string]bool
}

// Evaluated is the pure rule outcome.
type Evaluated struct {
	Technologies []TechView
	Packages     []PackageView
	Findings     []Finding
	Freshness    Area
	Security     Area
}

// PublicPackage maps a collected package to its publishable form, if any.
func PublicPackage(p stack.Package) (sources.PublicPackage, bool) {
	if p.Version == "" || !p.Checkable {
		return sources.PublicPackage{}, false
	}
	return sources.Publishable(p.Ecosystem, p.Name, p.Version, p.Origin == stack.OriginPublic)
}

// Evaluate applies the documented rules. It is a pure function of the
// facts, the external data, and today's date: support dates are compared
// with today on every evaluation, never taken from an earlier verdict.
func Evaluate(facts stack.Facts, ext External, today time.Time) Evaluated {
	var ev Evaluated
	var fresh, sec []stack.Gap
	for _, g := range facts.Gaps {
		if g.Area != stack.AreaSecurity {
			fresh = append(fresh, stack.Gap{Area: AreaFreshness, Scope: g.Scope, Reason: g.Reason})
		}
		if g.Area != stack.AreaFreshness {
			sec = append(sec, stack.Gap{Area: AreaSecurity, Scope: g.Scope, Reason: g.Reason})
		}
	}
	day := today.UTC().Format("2006-01-02")
	soon := today.UTC().AddDate(0, 0, EOLSoonDays).Format("2006-01-02")

	// Technologies: a product is verifiable when at least one declaration
	// gives a concrete version; declarations without one are then context.
	concrete := map[string]bool{}
	for _, t := range facts.Technologies {
		if t.Product != "" && t.Version != "" {
			concrete[t.Product] = true
		}
	}
	gapped := map[string]bool{}
	for _, t := range facts.Technologies {
		tv := TechView{Technology: t, Support: "unverified"}
		switch {
		case t.Product == "":
			tv.Note = t.Reason
		case t.Version == "":
			tv.Note = t.Reason
			if !concrete[t.Product] && !gapped[t.Product] {
				gapped[t.Product] = true
				fresh = append(fresh, stack.Gap{Area: AreaFreshness, Scope: t.Name + " (" + t.Source + ")", Reason: "konkreti versija nedeklaruota: " + t.Reason})
			}
		default:
			prod := ext.Products[t.Product]
			rel := matchCycle(prod, t.Version)
			if rel == nil {
				tv.Note = "endoflife.date neturi šios versijos ciklo arba versija nurodyta per bendrai"
				fresh = append(fresh, stack.Gap{Area: AreaFreshness, Scope: fmt.Sprintf("%s %s (%s)", t.Name, t.Version, t.Source), Reason: tv.Note})
				break
			}
			tv.Cycle, tv.EOLFrom, tv.EOASFrom, tv.LatestInCy = rel.Name, rel.EOLFrom, rel.EOASFrom, rel.Latest
			src := []Link{{Name: "endoflife.date: " + t.Name, URL: prod.URL}}
			evd := []Evidence{
				{"Komponentas", t.Name + " " + t.Version},
				{"Deklaracija", fmt.Sprintf("%s (%s, %s)", t.Declared, t.Source, basisLabel(t.Basis))},
				{"Išleidimo ciklas", rel.Name},
			}
			eol := rel.IsEOL
			if rel.EOLFrom != "" {
				eol = rel.EOLFrom <= day
				evd = append(evd, Evidence{"Palaikymo pabaiga", rel.EOLFrom})
			} else if rel.IsEOL {
				evd = append(evd, Evidence{"Palaikymo pabaiga", "šaltinis nurodo, kad ciklas nebepalaikomas (data nepateikta)"})
			}
			target := "palaikomą versiją"
			if latest := newestSupported(prod, day); latest != "" {
				target = "palaikomą ciklą (naujausias palaikomas: " + latest + ")"
			}
			switch {
			case eol:
				tv.Support = "eol"
				level, rule := LevelYellow, "FRESH-EOL-OTHER"
				if criticalKinds[t.Kind] {
					level, rule = LevelRed, "FRESH-EOL-CRITICAL"
				}
				ev.Findings = append(ev.Findings, Finding{Area: AreaFreshness, Level: level, Rule: rule,
					Title:   fmt.Sprintf("%s %s nebepalaikoma", t.Name, rel.Name),
					Subject: t.ID, Evidence: evd, Sources: src,
					Action: fmt.Sprintf("Atnaujinti %s iš %s į %s.", t.Name, rel.Name, target)})
			case rel.EOLFrom != "" && rel.EOLFrom <= soon:
				tv.Support = "eol-soon"
				ev.Findings = append(ev.Findings, Finding{Area: AreaFreshness, Level: LevelYellow, Rule: "FRESH-EOL-SOON",
					Title:   fmt.Sprintf("%s %s palaikymas baigiasi %s", t.Name, rel.Name, rel.EOLFrom),
					Subject: t.ID, Evidence: evd, Sources: src,
					Action: fmt.Sprintf("Suplanuoti %s atnaujinimą į %s iki %s.", t.Name, target, rel.EOLFrom)})
			default:
				tv.Support = "supported"
				if rel.EOASFrom != "" && rel.EOASFrom <= day || rel.EOASFrom == "" && rel.IsEOAS {
					tv.Note = "aktyvus palaikymas baigtas, teikiami tik saugumo pataisymai"
					ev.Findings = append(ev.Findings, Finding{Area: AreaFreshness, Level: LevelInfo, Rule: "FRESH-SECURITY-ONLY",
						Title:   fmt.Sprintf("%s %s gauna tik saugumo pataisymus", t.Name, rel.Name),
						Subject: t.ID, Evidence: evd, Sources: src,
						Action: "Veiksmų nereikia; įtraukti atnaujinimą į įprastą planą."})
				}
			}
		}
		ev.Technologies = append(ev.Technologies, tv)
	}

	// Packages.
	var private, uncheckable, regMissing, noDeprecation []string
	lockedCount, checkedCount := 0, 0
	for _, p := range facts.Packages {
		pv := PackageView{Package: p}
		if p.Version == "" {
			pv.Unchecked = "nėra užrakintos versijos"
			ev.Packages = append(ev.Packages, pv)
			continue
		}
		lockedCount++
		pub, ok := PublicPackage(p)
		if !ok {
			label := p.Name + " " + p.Version
			if p.Origin != stack.OriginPublic {
				pv.Unchecked = "privatus arba neaiškios kilmės paketas: vardas išoriniams šaltiniams nesiunčiamas"
				private = append(private, label)
			} else {
				pv.Unchecked = "užrakinta versija nėra išleista versija (pvz., šakos nuoroda)"
				uncheckable = append(uncheckable, label)
			}
			ev.Packages = append(ev.Packages, pv)
			continue
		}
		pv.Checked = true
		checkedCount++
		pv.Advisories = ext.Advisories[pub.Key()]
		for _, a := range pv.Advisories {
			ev.Findings = append(ev.Findings, advisoryFinding(p, a))
		}
		if p.Direct {
			reg, known := ext.Registry[pub.Key()]
			switch {
			case ext.RegistryMissing[pub.Key()]:
				regMissing = append(regMissing, p.Name)
			case known:
				pv.Latest, pv.Deprecated = reg.Latest, reg.Deprecated
				if !reg.DeprecationKnown {
					noDeprecation = append(noDeprecation, p.Name+" "+p.Version)
				}
				ev.Findings = append(ev.Findings, registryFindings(p, reg)...)
			}
		}
		ev.Packages = append(ev.Packages, pv)
	}
	both := func(g stack.Gap) {
		fresh = append(fresh, stack.Gap{Area: AreaFreshness, Scope: g.Scope, Reason: g.Reason})
		sec = append(sec, stack.Gap{Area: AreaSecurity, Scope: g.Scope, Reason: g.Reason})
	}
	if len(private) > 0 {
		both(stack.Gap{Scope: listed(private), Reason: fmt.Sprintf("%d privatūs arba neaiškios kilmės paketai nepatikrinti: jų vardai išoriniams šaltiniams nesiunčiami", len(private))})
	}
	if len(uncheckable) > 0 {
		both(stack.Gap{Scope: listed(uncheckable), Reason: fmt.Sprintf("%d paketų užrakinta versija nėra išleista versija, todėl jos negalima sutikrinti", len(uncheckable))})
	}
	if len(regMissing) > 0 {
		fresh = append(fresh, stack.Gap{Area: AreaFreshness, Scope: listed(regMissing), Reason: "registras šių tiesioginių priklausomybių nerado"})
	}
	if len(noDeprecation) > 0 {
		fresh = append(fresh, stack.Gap{Area: AreaFreshness, Scope: listed(noDeprecation), Reason: "registras neturi užrakintos versijos įrašo: deprecated būsena nežinoma"})
	}

	sort.SliceStable(ev.Findings, func(i, j int) bool {
		a, b := ev.Findings[i], ev.Findings[j]
		if a.Area != b.Area {
			return a.Area > b.Area // security first
		}
		if levelRank[a.Level] != levelRank[b.Level] {
			return levelRank[a.Level] > levelRank[b.Level]
		}
		return a.Title < b.Title
	})
	for i := range ev.Findings {
		ev.Findings[i].ID = fmt.Sprintf("F%d", i+1)
	}

	ev.Freshness = area(AreaFreshness, ev.Findings, fresh)
	ev.Freshness.Checked = fmt.Sprintf("%d technologijų deklaracijos, %d tiesioginės priklausomybės su užrakinta versija", len(facts.Technologies), countDirect(ev.Packages))
	ev.Security = area(AreaSecurity, ev.Findings, sec)
	ev.Security.Checked = fmt.Sprintf("%d iš %d užrakintų paketų sutikrinti su OSV", checkedCount, lockedCount)
	if ev.Technologies == nil {
		ev.Technologies = []TechView{}
	}
	if ev.Packages == nil {
		ev.Packages = []PackageView{}
	}
	if ev.Findings == nil {
		ev.Findings = []Finding{}
	}
	return ev
}

var levelRank = map[string]int{LevelInfo: 0, LevelYellow: 1, LevelRed: 2}

// area derives the status of one area:
//
//	findings, scope fully checked  -> worst finding colour
//	findings, scope not fully checked -> worst finding colour + partial
//	no findings, scope fully checked  -> green
//	no findings, scope not fully checked -> unverified
func area(name string, findings []Finding, gaps []stack.Gap) Area {
	a := Area{Status: StatusGreen, Gaps: gaps}
	if a.Gaps == nil {
		a.Gaps = []stack.Gap{}
	}
	worst, red, yellow := 0, 0, 0
	for _, f := range findings {
		if f.Area != name {
			continue
		}
		worst = max(worst, levelRank[f.Level])
		switch f.Level {
		case LevelRed:
			red++
		case LevelYellow:
			yellow++
		}
	}
	complete := len(gaps) == 0
	switch {
	case worst == 2:
		a.Status = StatusRed
		a.Reason = fmt.Sprintf("neatidėliotinų radinių: %d, planinių: %d", red, yellow)
	case worst == 1:
		a.Status = StatusYellow
		a.Reason = fmt.Sprintf("planine tvarka spręstinų radinių: %d", yellow)
	case complete:
		a.Reason = "nurodyta apimtis patikrinta, žinomų problemų nenustatyta"
	default:
		a.Status = StatusUnverified
		a.Reason = "patikra nepilna: " + gaps[0].Reason
		if len(gaps) > 1 {
			a.Reason += fmt.Sprintf(" (ir dar priežasčių: %d)", len(gaps)-1)
		}
	}
	if worst > 0 && !complete {
		a.Partial = true
		a.Reason += fmt.Sprintf("; dalinė patikra, nepatikrintų apimties dalių: %d", len(gaps))
	}
	return a
}

// matchCycle finds the release cycle of a version: the cycle named by its
// major.minor, else the cycle named by its major alone. A version given
// only as a major number never matches a major.minor cycle.
func matchCycle(p *sources.Product, version string) *sources.Release {
	if p == nil {
		return nil
	}
	parts := strings.Split(strings.TrimPrefix(version, "v"), ".")
	var candidates []string
	if len(parts) >= 2 {
		candidates = append(candidates, parts[0]+"."+parts[1])
	}
	candidates = append(candidates, parts[0])
	for _, c := range candidates {
		for i := range p.Releases {
			if p.Releases[i].Name == c {
				return &p.Releases[i]
			}
		}
	}
	return nil
}

// newestSupported names the first listed cycle that is still supported.
func newestSupported(p *sources.Product, day string) string {
	for _, r := range p.Releases {
		if r.EOLFrom != "" && r.EOLFrom > day || r.EOLFrom == "" && !r.IsEOL {
			return r.Name
		}
	}
	return ""
}

func basisLabel(b string) string {
	switch b {
	case stack.BasisLocked:
		return "užrakinta versija"
	case stack.BasisImageTag:
		return "deklaruota image žyma"
	case stack.BasisArgDefault:
		return "deklaruota image žyma pagal numatytąją ARG reikšmę"
	case stack.BasisPlatform:
		return "Composer config.platform deklaracija"
	default:
		return "versijos apribojimas"
	}
}

func pkgLabel(p stack.Package) string {
	kind := "tranzityvi"
	if p.Direct {
		kind = "tiesioginė"
	}
	if p.Dev {
		kind += ", dev"
	}
	return fmt.Sprintf("%s %s (%s, %s)", p.Name, p.Version, p.Ecosystem, kind)
}

var sevLabel = map[string]string{
	sources.SevCritical: "kritinis", sources.SevHigh: "aukštas", sources.SevMedium: "vidutinis",
	sources.SevLow: "žemas", sources.SevUnknown: "nežinomas",
}

// advisoryFinding applies the security rules. A critical vulnerability is
// urgent whether or not the package is a development dependency. An unknown
// severity is still a finding and asks for a manual assessment.
func advisoryFinding(p stack.Package, a sources.Advisory) Finding {
	f := Finding{Area: AreaSecurity, Level: LevelYellow, Rule: "SEC-KNOWN", Subject: p.ID,
		Sources: []Link{{Name: "OSV: " + a.ID, URL: a.URL}}}
	sev := sevLabel[a.Severity]
	f.Title = fmt.Sprintf("%s %s: %s (sunkumas: %s)", p.Name, p.Version, a.ID, sev)
	f.Evidence = []Evidence{{"Paketas", pkgLabel(p)}, {"Pažeidžiamumas", strings.Join(append([]string{a.ID}, a.Aliases...), ", ")}}
	if a.Summary != "" {
		f.Evidence = append(f.Evidence, Evidence{"Aprašas (OSV)", a.Summary})
	}
	sevText := sev + " — " + a.SeverityBasis
	if a.CVSS > 0 {
		sevText += fmt.Sprintf(" (CVSS %.1f)", a.CVSS)
	}
	f.Evidence = append(f.Evidence, Evidence{"Sunkumas", sevText})
	fix := "Pataisytos versijos OSV įraše nenurodytos: įvertinti paketo pakeitimą arba apsaugos priemones."
	if len(a.Fixed) > 0 {
		f.Evidence = append(f.Evidence, Evidence{"Pataisyta versijose", strings.Join(a.Fixed, ", ")})
		fix = fmt.Sprintf("Atnaujinti %s iki versijos, kurioje pažeidžiamumas pataisytas (%s), pasirenkant naudojamos šakos pataisymą.", p.Name, strings.Join(a.Fixed, ", "))
		if !p.Direct {
			fix += " Paketas tranzityvus: atnaujinti jį įtraukiančią tiesioginę priklausomybę arba lock failą."
		}
	}
	switch a.Severity {
	case sources.SevCritical:
		f.Level, f.Rule = LevelRed, "SEC-CRITICAL"
		f.Action = "Spręsti nedelsiant. " + fix
	case sources.SevUnknown:
		f.Rule = "SEC-UNKNOWN-SEVERITY"
		f.Action = "Sunkumas nenustatytas: peržiūrėti OSV įrašą ir įvertinti rankiniu būdu. " + fix
	default:
		f.Action = fix
	}
	return f
}

func registryFindings(p stack.Package, reg sources.RegistryInfo) []Finding {
	var out []Finding
	src := []Link{{Name: registryName(p.Ecosystem) + ": " + p.Name, URL: reg.URL}}
	base := []Evidence{{"Paketas", pkgLabel(p)}, {"Deklaruotas apribojimas", p.Constraint}}
	if reg.Deprecated {
		out = append(out, Finding{Area: AreaFreshness, Level: LevelYellow, Rule: "FRESH-DEPRECATED", Subject: p.ID,
			Title:    fmt.Sprintf("%s %s pažymėtas kaip nebepalaikomas", p.Name, p.Version),
			Evidence: append(append([]Evidence{}, base...), Evidence{"Registro žyma", reg.Note}), Sources: src,
			Action: fmt.Sprintf("Suplanuoti %s pakeitimą palaikomu paketu arba palaikoma versija.", p.Name)})
	}
	cur, ok1 := sources.Major(p.Version)
	latest, ok2 := sources.Major(reg.Latest)
	if ok1 && ok2 && latest > cur {
		evd := append(append([]Evidence{}, base...), Evidence{"Užrakinta versija", p.Version}, Evidence{"Naujausia stabili versija", reg.Latest})
		f := Finding{Area: AreaFreshness, Subject: p.ID, Evidence: evd, Sources: src}
		// Type declaration packages follow the version of what they describe,
		// so their lag is never a planned update on its own.
		if latest-cur >= MajorsBehind && !strings.HasPrefix(p.Name, "@types/") {
			f.Level, f.Rule = LevelYellow, "FRESH-MAJORS-BEHIND"
			f.Title = fmt.Sprintf("%s atsilieka %d pagrindinėmis versijomis (%s → %s)", p.Name, latest-cur, p.Version, reg.Latest)
			f.Action = fmt.Sprintf("Suplanuoti %s atnaujinimą iki %s, pereinant per tarpines pagrindines versijas.", p.Name, reg.Latest)
		} else {
			f.Level, f.Rule = LevelInfo, "FRESH-MAJOR-AVAILABLE"
			f.Title = fmt.Sprintf("%s: išleista nauja pagrindinė versija (%s → %s)", p.Name, p.Version, reg.Latest)
			f.Action = "Veiksmų nereikia; vien nauja pagrindinė versija nėra problema."
		}
		out = append(out, f)
	}
	return out
}

func registryName(eco string) string {
	if eco == sources.Composer {
		return "Packagist"
	}
	return "npm"
}

func listed(items []string) string {
	sort.Strings(items)
	if len(items) > maxListed {
		return strings.Join(items[:maxListed], ", ") + fmt.Sprintf(" ir dar %d", len(items)-maxListed)
	}
	return strings.Join(items, ", ")
}

func countDirect(pkgs []PackageView) int {
	n := 0
	for _, p := range pkgs {
		if p.Direct && p.Version != "" {
			n++
		}
	}
	return n
}
