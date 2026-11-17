package httpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Ryttis/devatlas/internal/assess"
	"github.com/Ryttis/devatlas/internal/collect/git"
	"github.com/Ryttis/devatlas/internal/registry"
)

var projectTemplate = template.Must(template.New("project.html").Funcs(template.FuncMap{
	"deref": func(b *bool) bool { return b != nil && *b },
	"basis": func(b string) string {
		switch b {
		case "locked":
			return "užrakinta (lock failas)"
		case "image-tag":
			return "deklaruota image žyma"
		case "arg-default":
			return "deklaruota image žyma (numatytoji ARG reikšmė)"
		case "platform":
			return "deklaruota Composer config.platform"
		}
		return "deklaruotas versijos apribojimas"
	},
}).ParseFS(templateFS, "templates/project.html"))

// ScopeNote states what a status does and does not cover. It is shown
// wherever statuses are shown.
const ScopeNote = "Patikros apimtis: deklaruotų technologijų palaikymas ir žinomi užrakintų priklausomybių pažeidžiamumai (Composer, npm, Dockerfile, Compose). Tai nėra visos sistemos saugumo auditas: veikianti aplinka, kodas, konfigūracija ir paslaptys netikrinami."

const maxFormBytes = 4 << 10

// SetAssessment enables the technology status dashboard. It must be called
// before Listen.
func (s *Server) SetAssessment(svc *assess.Service) { s.assess = svc }

// statusView is how one area status is displayed.
type statusView struct {
	Class   string
	Icon    string
	Label   string
	Reason  string
	Partial bool
}

func unverified(reason string) statusView {
	return statusView{Class: "unverified", Icon: "⚪", Label: "Nepatikrinta", Reason: reason}
}

func areaView(a assess.Area) statusView {
	v := statusView{Reason: a.Reason, Partial: a.Partial}
	switch a.Status {
	case assess.StatusGreen:
		v.Class, v.Icon, v.Label = "green", "🟢", "OK"
	case assess.StatusYellow:
		v.Class, v.Icon, v.Label = "yellow", "🟡", "Smulkios problemos"
	case assess.StatusRed:
		v.Class, v.Icon, v.Label = "red", "🔴", "Spręsti nedelsiant"
	default:
		return unverified(a.Reason)
	}
	return v
}

// rowView is one project in the status table.
type rowView struct {
	ID          string
	Name        string
	Description string
	Tech        []string
	Fresh       statusView
	Sec         statusView
	CheckedAt   string
	Stale       []string
	AttemptNote string
	Job         string
	Checkout    string
	Checkouts   int
}

func fmtTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format("2006-01-02 15:04") + " UTC"
}

// techSummary lists recognised technologies: a concrete version where one
// was established, otherwise the declaration marked as such.
func techSummary(res *assess.Result) []string {
	var out []string
	seen := map[string]bool{}
	withVersion := map[string]bool{}
	for _, t := range res.Technologies {
		if t.Product != "" && t.Version != "" {
			withVersion[t.Product] = true
		}
	}
	for _, t := range res.Technologies {
		if t.Product == "" {
			continue
		}
		var label string
		switch {
		case t.Version != "":
			v := t.Version
			if t.Cycle != "" && t.Basis != "locked" {
				v = t.Cycle
			}
			label = t.Name + " " + v
		case withVersion[t.Product]:
			continue
		default:
			label = t.Name + " " + t.Declared + " (tik deklaracija)"
		}
		if !seen[label] {
			seen[label] = true
			out = append(out, label)
		}
	}
	return out
}

func jobLabel(state string) string {
	switch state {
	case assess.JobQueued:
		return "Eilėje…"
	case assess.JobRunning:
		return "Vertinama…"
	}
	return ""
}

// current describes a checkout as observed now.
func current(c *CheckoutDTO) assess.Current {
	if c == nil {
		return assess.Current{}
	}
	cur := assess.Current{Registered: true}
	if r := c.Git.Repository; c.Git.Status == git.StatusOK && r != nil {
		cur.GitOK, cur.Head = true, r.Head.Commit
		cur.Dirty = r.Changes.Tracked > 0 || r.Changes.Untracked > 0
	}
	return cur
}

func findCheckout(p *ProjectDTO, id string) *CheckoutDTO {
	for i := range p.Checkouts {
		if p.Checkouts[i].ID == id {
			return &p.Checkouts[i]
		}
	}
	return nil
}

func attemptNote(rec *assess.Record) string {
	if rec == nil || rec.LastAttempt.OK || rec.LastAttempt.At.IsZero() {
		return ""
	}
	return fmt.Sprintf("Paskutinis bandymas %s nepavyko: %s", fmtTime(rec.LastAttempt.At), rec.LastAttempt.Error)
}

// buildRows joins registrations with stored records. Statuses always come
// from a stored result; nothing is evaluated while rendering.
func (s *Server) buildRows(resp *ProjectsResponse, now time.Time) []rowView {
	rows := make([]rowView, 0, len(resp.Projects))
	for i := range resp.Projects {
		p := &resp.Projects[i]
		row := rowView{ID: p.ID, Name: p.Name, Checkouts: len(p.Checkouts)}
		if s.assess == nil {
			row.Fresh = unverified("vertinimas šiame procese neįjungtas")
			row.Sec = row.Fresh
			rows = append(rows, row)
			continue
		}
		row.Job = jobLabel(s.assess.State(p.ID))
		rec, err := s.assess.Record(p.ID)
		switch {
		case err != nil:
			row.Fresh = unverified("išsaugotas vertinimo įrašas neperskaitomas; paleiskite „Atnaujinti“")
			row.Sec = row.Fresh
		case rec == nil || rec.Result == nil:
			reason := "dar nevertinta; paspauskite „Atnaujinti“"
			if note := attemptNote(rec); note != "" {
				reason = note
			}
			row.Fresh = unverified(reason)
			row.Sec = row.Fresh
		default:
			res := rec.Result
			row.Description = res.Description
			row.Tech = techSummary(res)
			row.Fresh, row.Sec = areaView(res.Freshness), areaView(res.Security)
			row.CheckedAt = fmtTime(res.SourcesCheckedAt)
			row.Checkout = res.Checkout.Path
			row.Stale = assess.Staleness(res, now, current(findCheckout(p, res.Checkout.ID)))
			row.AttemptNote = attemptNote(rec)
		}
		rows = append(rows, row)
	}
	return rows
}

var flashMessages = map[string]string{
	"queued":   "Vertinimas įtrauktas į eilę. Puslapis atsinaujins automatiškai.",
	"busy":     "Šio projekto vertinimas jau suplanuotas arba vykdomas.",
	"cooldown": "Projektas ką tik vertintas; pakartoti galima po trumpos pertraukos.",
	"full":     "Vertinimų eilė pilna; pabandykite vėliau.",
	"partial":  "Į eilę įtraukti ne visi projektai: dalis jau vertinama, ką tik vertinta arba eilė pilna.",
	"none":     "Nėra projektų, kuriuos būtų galima įtraukti į eilę.",
	"stopped":  "Vertinimų vykdymas sustabdytas.",
}

func enqueueCode(err error) string {
	switch err {
	case nil:
		return "queued"
	case assess.ErrAlreadyScheduled:
		return "busy"
	case assess.ErrCooldown:
		return "cooldown"
	case assess.ErrQueueFull:
		return "full"
	}
	return "stopped"
}

// guardPost protects the state-changing endpoints against requests forged
// by a web page: the Host header must name this loopback listener (which
// also defeats DNS rebinding), and a browser's Origin and Sec-Fetch-Site
// headers, when present, must say the request is same-origin.
func (s *Server) guardPost(w http.ResponseWriter, r *http.Request) bool {
	deny := func(why string) bool {
		s.log.Warn("rejected POST", "reason", why, "path", r.URL.Path)
		http.Error(w, "forbidden: "+why, http.StatusForbidden)
		return false
	}
	if s.assess == nil {
		http.Error(w, "assessment is not enabled", http.StatusNotFound)
		return false
	}
	if !s.allowedHosts[strings.ToLower(r.Host)] {
		return deny("unexpected Host header")
	}
	if o := r.Header.Get("Origin"); o != "" && !strings.EqualFold(o, "http://"+r.Host) {
		return deny("cross-origin request")
	}
	if f := r.Header.Get("Sec-Fetch-Site"); f != "" && f != "same-origin" && f != "none" {
		return deny("cross-site request")
	}
	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/x-www-form-urlencoded") {
		return deny("unexpected content type")
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxFormBytes)
	if err := r.ParseForm(); err != nil {
		return deny("invalid form")
	}
	return true
}

// defaultCheckout picks the checkout to assess when the form names none:
// the one assessed last, if still registered, else the first.
func (s *Server) defaultCheckout(v registry.ProjectView) (registry.Checkout, bool) {
	if len(v.Checkouts) == 0 {
		return registry.Checkout{}, false
	}
	if rec, err := s.assess.Record(v.ID); err == nil && rec != nil {
		last := rec.LastAttempt.CheckoutID
		if rec.Result != nil {
			last = rec.Result.Checkout.ID
		}
		for _, c := range v.Checkouts {
			if c.ID == last {
				return c, true
			}
		}
	}
	return v.Checkouts[0], true
}

func (s *Server) handleAssess(w http.ResponseWriter, r *http.Request) {
	if !s.guardPost(w, r) {
		return
	}
	id := r.PathValue("id")
	reg, err := s.registry.Load()
	if err != nil {
		http.Error(w, registryLoadError, http.StatusInternalServerError)
		return
	}
	for _, v := range reg.Views() {
		if v.ID != id {
			continue
		}
		ck, ok := s.defaultCheckout(v)
		if want := r.PostForm.Get("checkout"); want != "" {
			ok = false
			for _, c := range v.Checkouts {
				if c.ID == want {
					ck, ok = c, true
				}
			}
		}
		if !ok {
			http.Error(w, "unknown checkout", http.StatusBadRequest)
			return
		}
		code := enqueueCode(s.assess.Enqueue(assess.Job{ProjectID: v.ID, CheckoutID: ck.ID, Path: ck.Path}))
		target := "/"
		if r.PostForm.Get("return") == "project" {
			target = "/projects/" + url.PathEscape(v.ID)
		}
		http.Redirect(w, r, target+"?msg="+code, http.StatusSeeOther)
		return
	}
	http.NotFound(w, r)
}

func (s *Server) handleAssessAll(w http.ResponseWriter, r *http.Request) {
	if !s.guardPost(w, r) {
		return
	}
	reg, err := s.registry.Load()
	if err != nil {
		http.Error(w, registryLoadError, http.StatusInternalServerError)
		return
	}
	queued, skipped := 0, 0
	for _, v := range reg.Views() {
		ck, ok := s.defaultCheckout(v)
		if !ok {
			continue
		}
		if s.assess.Enqueue(assess.Job{ProjectID: v.ID, CheckoutID: ck.ID, Path: ck.Path}) == nil {
			queued++
		} else {
			skipped++
		}
	}
	code := "queued"
	switch {
	case queued == 0:
		code = "none"
	case skipped > 0:
		code = "partial"
	}
	http.Redirect(w, r, "/?msg="+code, http.StatusSeeOther)
}

func (s *Server) handleAssessmentAPI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if s.assess == nil {
		w.WriteHeader(http.StatusNotFound)
		_ = enc.Encode(errorResponse{Error: "assessment is not enabled"})
		return
	}
	rec, err := s.assess.Record(r.PathValue("id"))
	switch {
	case err != nil:
		w.WriteHeader(http.StatusInternalServerError)
		_ = enc.Encode(errorResponse{Error: "assessment record cannot be read"})
	case rec == nil:
		w.WriteHeader(http.StatusNotFound)
		_ = enc.Encode(errorResponse{Error: "no assessment stored for this project"})
	default:
		_ = enc.Encode(rec)
	}
}

type findingView struct {
	assess.Finding
	Icon       string
	LevelLabel string
	AI         *assess.AIText
}

type projectData struct {
	Project     ProjectDTO
	Now         string
	Scope       string
	Flash       string
	Enabled     bool
	Job         string
	Refresh     bool
	Record      *assess.Record
	Result      *assess.Result
	RecordError string
	AttemptNote string
	Fresh       statusView
	Sec         statusView
	Stale       []string
	CheckedAt   string
	AIAt        string
	AttemptAt   string
	AILabel     string
	Selected    string
	FreshFinds  []findingView
	SecFinds    []findingView
	Notes       []findingView
	Direct      []assess.PackageView
	Transitive  []assess.PackageView
	ShortHead   string
}

func aiLabel(ai assess.AIResult) string {
	switch ai.Status {
	case assess.AIOK:
		s := "DI paaiškinimai sugeneruoti modeliu " + ai.Model
		if ai.Reused {
			s += " (panaudoti anksčiau sugeneruoti: įvestis nepasikeitė)"
		}
		if ai.Dropped > 0 {
			s += fmt.Sprintf("; %d paaiškinimai atmesti patikros", ai.Dropped)
		}
		return s
	case assess.AINotNeeded:
		return "DI nekviestas: " + ai.Reason
	default:
		return ai.Reason
	}
}

func (s *Server) handleProject(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	id := r.PathValue("id")
	reg, err := s.registry.Load()
	if err != nil {
		s.log.Warn(registryLoadError)
		http.Error(w, registryLoadError, http.StatusInternalServerError)
		return
	}
	var view *registry.ProjectView
	for _, v := range reg.Views() {
		if v.ID == id {
			v := v
			view = &v
		}
	}
	if view == nil {
		http.NotFound(w, r)
		return
	}
	resp := ObserveProjects(r.Context(), []registry.ProjectView{*view}, s.registry.Path(), now, s.git)
	data := projectData{Project: resp.Projects[0], Now: fmtTime(now), Scope: ScopeNote, Enabled: s.assess != nil,
		Flash: flashMessages[r.URL.Query().Get("msg")]}
	data.Fresh = unverified("vertinimas šiame procese neįjungtas")
	data.Sec = data.Fresh
	if s.assess != nil {
		s.fillProject(r.Context(), &data, now)
	}
	if len(data.Project.Checkouts) > 0 && data.Selected == "" {
		data.Selected = data.Project.Checkouts[0].ID
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err := projectTemplate.Execute(w, data); err != nil {
		s.log.Warn("render project page", "err", err)
	}
}

func (s *Server) fillProject(ctx context.Context, data *projectData, now time.Time) {
	state := s.assess.State(data.Project.ID)
	data.Job, data.Refresh = jobLabel(state), state != assess.JobIdle
	rec, err := s.assess.Record(data.Project.ID)
	if err != nil {
		data.RecordError = "Išsaugotas vertinimo įrašas neperskaitomas; paleiskite „Atnaujinti“."
		data.Fresh = unverified("vertinimo įrašas neperskaitomas")
		data.Sec = data.Fresh
		return
	}
	data.Record = rec
	data.AttemptNote = attemptNote(rec)
	if rec != nil {
		data.AttemptAt = fmtTime(rec.LastAttempt.At)
		data.Selected = rec.LastAttempt.CheckoutID
	}
	if rec == nil || rec.Result == nil {
		reason := "dar nevertinta; paspauskite „Atnaujinti“"
		if data.AttemptNote != "" {
			reason = data.AttemptNote
		}
		data.Fresh = unverified(reason)
		data.Sec = data.Fresh
		return
	}
	res := rec.Result
	data.Result = res
	data.Selected = res.Checkout.ID
	data.Fresh, data.Sec = areaView(res.Freshness), areaView(res.Security)
	data.CheckedAt, data.AIAt, data.AILabel = fmtTime(res.SourcesCheckedAt), fmtTime(res.AI.GeneratedAt), aiLabel(res.AI)
	if len(res.Checkout.Head) >= 12 {
		data.ShortHead = res.Checkout.Head[:12]
	}
	cur := current(findCheckout(&data.Project, res.Checkout.ID))
	if cur.Registered && ctx.Err() == nil {
		// The card re-reads the assessed files so a change is visible even
		// without a commit. Only hashes are compared; nothing is re-evaluated.
		if facts, err := s.assess.Engine.Collect(res.Checkout.Path); err == nil {
			cur.FilesHash = facts.FilesHash
		}
	}
	data.Stale = assess.Staleness(res, now, cur)
	for _, f := range res.Findings {
		fv := findingView{Finding: f}
		if t, ok := res.AI.Texts[f.ID]; ok && res.AI.Status == assess.AIOK {
			fv.AI = &t
		}
		switch {
		case f.Level == assess.LevelInfo:
			fv.Icon, fv.LevelLabel = "ℹ️", "Pastaba"
			data.Notes = append(data.Notes, fv)
			continue
		case f.Level == assess.LevelRed:
			fv.Icon, fv.LevelLabel = "🔴", "Spręsti nedelsiant"
		default:
			fv.Icon, fv.LevelLabel = "🟡", "Planine tvarka"
		}
		if f.Area == assess.AreaSecurity {
			data.SecFinds = append(data.SecFinds, fv)
		} else {
			data.FreshFinds = append(data.FreshFinds, fv)
		}
	}
	for _, p := range res.Packages {
		if p.Direct {
			data.Direct = append(data.Direct, p)
		} else {
			data.Transitive = append(data.Transitive, p)
		}
	}
}
