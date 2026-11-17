package assess

import (
	"encoding/json"
	"strings"
	"testing"
)

func sampleInput() aiInput {
	return buildAIInput([]Finding{
		{ID: "F1", Area: AreaSecurity, Level: LevelRed, Rule: "SEC-CRITICAL", Title: "acme/lib 1.0.0: GHSA-aaaa-bbbb-cccc",
			Evidence: []Evidence{{"Paketas", "acme/lib 1.0.0"}, {"Pažeidžiamumas", "GHSA-aaaa-bbbb-cccc, CVE-2024-1111"},
				{"Deklaracija", "php:8.2 (docker/internal/Dockerfile, deklaruota image žyma)"},
				{"Pastaba", "see https://internal.example/wiki"}, {"Kita", "token=abc123"}, {"Adresas", "10.1.2.3"}}, Action: "Atnaujinti."},
		{ID: "F2", Area: AreaFreshness, Level: LevelYellow, Rule: "FRESH-EOL-SOON", Title: "PHP 8.2", Action: "Planuoti."},
		{ID: "F3", Area: AreaFreshness, Level: LevelInfo, Rule: "FRESH-MAJOR-AVAILABLE", Title: "note"},
	})
}

func TestAIInputExcludesPathsAddressesAndNotes(t *testing.T) {
	in := sampleInput()
	if len(in.Findings) != 2 {
		t.Fatalf("info findings are not sent: %+v", in.Findings)
	}
	js, _ := json.Marshal(in)
	for _, leak := range []string{"Dockerfile", "internal.example", "abc123", "10.1.2.3"} {
		if strings.Contains(string(js), leak) {
			t.Errorf("AI input contains %q: %s", leak, js)
		}
	}
	if !strings.Contains(string(js), "acme/lib 1.0.0") {
		t.Error("public package evidence must be kept")
	}
}

func TestValidateAI(t *testing.T) {
	in := sampleInput()
	good := `{"findings":[
	 {"id":"F1","explanation":"Kritinis pažeidžiamumas GHSA-aaaa-bbbb-cccc (CVE-2024-1111).","action":"Atnaujinti paketą."},
	 {"id":"F2","explanation":"Palaikymas baigiasi.","action":"Planuoti atnaujinimą."},
	 {"id":"F9","explanation":"Išgalvotas radinys.","action":"Nieko."},
	 {"id":"F3","explanation":"Nesiųstas radinys.","action":"Nieko."}]}`
	texts, dropped, err := validateAI("```json\n"+good+"\n```", in)
	if err != nil || len(texts) != 2 || dropped != 2 {
		t.Fatalf("texts=%v dropped=%d err=%v", texts, dropped, err)
	}
	if _, ok := texts["F9"]; ok {
		t.Error("an answer must not add findings")
	}

	for name, item := range map[string]string{
		"link":           `{"id":"F1","explanation":"Žr. https://evil.example/fix","action":"a"}`,
		"www":            `{"id":"F1","explanation":"Žr. www.evil.example","action":"a"}`,
		"fabricated CVE": `{"id":"F1","explanation":"Tai CVE-2099-9999.","action":"a"}`,
		"CVE of other":   `{"id":"F2","explanation":"Susiję su GHSA-aaaa-bbbb-cccc.","action":"a"}`,
		"empty action":   `{"id":"F1","explanation":"x","action":"  "}`,
		"too long":       `{"id":"F1","explanation":"` + strings.Repeat("ž", aiMaxTextRunes+1) + `","action":"a"}`,
		"duplicate":      `{"id":"F2","explanation":"second answer for the same finding","action":"b"}`,
	} {
		doc := `{"findings":[{"id":"F2","explanation":"ok","action":"ok"},` + item + `]}`
		texts, dropped, err := validateAI(doc, in)
		if err != nil || dropped != 1 || len(texts) != 1 || texts["F2"].Explanation != "ok" {
			t.Errorf("%s: texts=%v dropped=%d err=%v", name, texts, dropped, err)
		}
	}

	for name, doc := range map[string]string{
		"not json":       `Sure! Here are the findings.`,
		"unknown field":  `{"findings":[{"id":"F1","explanation":"x","action":"a","level":"green"}]}`,
		"status field":   `{"status":"green","findings":[]}`,
		"trailing":       `{"findings":[{"id":"F1","explanation":"x","action":"a"}]} {"x":1}`,
		"nothing valid":  `{"findings":[{"id":"F9","explanation":"x","action":"a"}]}`,
		"sources field":  `{"findings":[{"id":"F1","explanation":"x","action":"a","sources":["https://x"]}]}`,
		"empty findings": `{"findings":[]}`,
	} {
		_, _, err := validateAI(doc, in)
		ae, ok := err.(*aiError)
		if !ok || !ae.rejected {
			t.Errorf("%s: expected a rejection, got %v", name, err)
		}
	}
}
