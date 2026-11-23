package httpserver

import (
	"bytes"
	"html/template"
	"net/http"
	"strconv"
	"time"

	"github.com/Ryttis/devatlas/internal/runhistory"
)

var consoleTemplate = template.Must(template.New("console.html").Funcs(template.FuncMap{
	"stamp": func(t time.Time) string { return t.UTC().Format("2006-01-02 15:04:05 UTC") },
	"short": func(s string) string {
		if len(s) > 12 {
			return s[:12]
		}
		if s == "" {
			return "—"
		}
		return s
	},
}).ParseFS(templateFS, "templates/console.html"))

type consoleData struct {
	Now                                          time.Time
	Entries                                      []runhistory.Entry
	Detail                                       *runhistory.Detail
	Warnings                                     []string
	Unavailable, Total, Passed, Attention, Other int
	Page, Pages, Previous, Next                  int
	Kind                                         string
}

func (s *Server) handleRuns(w http.ResponseWriter, r *http.Request) {
	kind := r.URL.Query().Get("kind")
	if kind != "" && kind != "run" && kind != "workflow" {
		http.Error(w, "Nežinomas filtras", http.StatusBadRequest)
		return
	}
	page := 1
	if raw := r.URL.Query().Get("page"); raw != "" {
		var err error
		page, err = strconv.Atoi(raw)
		if err != nil || page < 1 || page > 100000 {
			http.Error(w, "Netinkamas puslapis", http.StatusBadRequest)
			return
		}
	}
	listing := s.history.List(r.Context())
	d := consoleData{Now: time.Now(), Warnings: listing.Warnings, Unavailable: listing.Unavailable, Kind: kind, Page: page}
	for _, e := range listing.Entries {
		if kind != "" && e.Kind != kind {
			continue
		}
		d.Entries = append(d.Entries, e)
		switch e.Tone() {
		case "good":
			d.Passed++
		case "bad", "wait":
			d.Attention++
		default:
			d.Other++
		}
	}
	d.Total = len(d.Entries)
	d.Pages = (d.Total + 49) / 50
	if d.Pages == 0 {
		d.Pages = 1
	}
	if page > d.Pages {
		http.NotFound(w, r)
		return
	}
	start := (page - 1) * 50
	end := start + 50
	if end > d.Total {
		end = d.Total
	}
	d.Entries = d.Entries[start:end]
	if page > 1 {
		d.Previous = page - 1
	}
	if page < d.Pages {
		d.Next = page + 1
	}
	s.renderConsole(w, d)
}

func (s *Server) handleRun(w http.ResponseWriter, r *http.Request) {
	detail, err := s.history.Get(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Įrašas nerastas arba neprieinamas", http.StatusNotFound)
		return
	}
	s.renderConsole(w, consoleData{Now: time.Now(), Detail: &detail})
}

func (s *Server) renderConsole(w http.ResponseWriter, d consoleData) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; base-uri 'none'; frame-ancestors 'none'; form-action 'none'")
	var b bytes.Buffer
	if err := consoleTemplate.Execute(&b, d); err != nil {
		s.log.Warn("render run console")
		http.Error(w, "Nepavyko parodyti konsolės", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(b.Bytes())
}
