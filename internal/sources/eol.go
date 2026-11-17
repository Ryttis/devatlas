package sources

import (
	"context"
	"encoding/json"
	"regexp"
)

// Product is the support schedule of one endoflife.date product.
type Product struct {
	Slug     string    `json:"slug"`
	Label    string    `json:"label"`
	URL      string    `json:"url"`
	Releases []Release `json:"releases"`
}

// Release is one release cycle. The dates are kept, not the service's
// boolean verdicts alone, so support can be re-evaluated against today's
// date. EOLFrom is empty when the service gives no date; IsEOL then carries
// its verdict. The same holds for active support (EOAS).
type Release struct {
	Name     string `json:"name"`
	EOLFrom  string `json:"eol_from,omitempty"`
	IsEOL    bool   `json:"is_eol"`
	EOASFrom string `json:"eoas_from,omitempty"`
	IsEOAS   bool   `json:"is_eoas"`
	Latest   string `json:"latest,omitempty"`
}

var slugRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,60}$`)
var dateRE = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

// EOL fetches the release cycles of a product from the endoflife.date v1
// API. Only the product name is sent; it comes from a fixed table.
func (r *Run) EOL(ctx context.Context, slug string) (*Product, error) {
	const source = "endoflife.date"
	if !slugRE.MatchString(slug) {
		return nil, &Error{Source: source, Reason: "netinkamas produkto vardas"}
	}
	if v, ok := r.c.cached("eol:" + slug); ok {
		return v.(*Product), nil
	}
	data, err := r.do(ctx, source, "GET", r.c.EOL+"/api/v1/products/"+slug, nil, "application/json", maxSmallBody)
	if err != nil {
		return nil, err
	}
	var doc struct {
		Result struct {
			Label    string `json:"label"`
			Releases []struct {
				Name     string  `json:"name"`
				IsEOL    *bool   `json:"isEol"`
				EOLFrom  *string `json:"eolFrom"`
				IsEOAS   *bool   `json:"isEoas"`
				EOASFrom *string `json:"eoasFrom"`
				Latest   *struct {
					Name string `json:"name"`
				} `json:"latest"`
			} `json:"releases"`
		} `json:"result"`
	}
	if err := json.Unmarshal(data, &doc); err != nil || len(doc.Result.Releases) == 0 {
		return nil, &Error{Source: source, Reason: "atsakymo formatas neatpažintas"}
	}
	p := &Product{Slug: slug, Label: cleanText(doc.Result.Label, 60), URL: "https://endoflife.date/" + slug}
	date := func(s *string) string {
		if s != nil && dateRE.MatchString(*s) {
			return *s
		}
		return ""
	}
	for _, rel := range doc.Result.Releases {
		out := Release{Name: cleanText(rel.Name, 40), EOLFrom: date(rel.EOLFrom), EOASFrom: date(rel.EOASFrom)}
		if rel.IsEOL != nil {
			out.IsEOL = *rel.IsEOL
		}
		if rel.IsEOAS != nil {
			out.IsEOAS = *rel.IsEOAS
		}
		if rel.Latest != nil {
			out.Latest = cleanText(rel.Latest.Name, 40)
		}
		p.Releases = append(p.Releases, out)
	}
	r.c.store("eol:"+slug, p)
	return p, nil
}
