package runhistory

import (
	"net/url"
	"strings"
	"testing"
)

func TestRedactionURIUserinfoPunctuation(t *testing.T) {
	for _, scheme := range []string{"ssh", "git+ssh", "mongodb+srv"} {
		for _, password := range []string{"prefix'TAIL_SENTINEL", "prefix%27TAIL_SENTINEL", "prefix!$&'()*+,;=:TAIL_SENTINEL"} {
			uri := scheme + "://alice:" + password + "@example.invalid/repo"
			u, err := url.Parse(uri)
			if err != nil || u.User == nil {
				t.Fatalf("invalid URI fixture %q: %v", uri, err)
			}
			for _, input := range []string{uri, "'" + uri + "'", "\"" + uri + "\""} {
				got := redact("before " + input + " after docs/check.go:42")
				for _, secret := range []string{"alice", "prefix", "TAIL_SENTINEL", "example.invalid"} {
					if strings.Contains(got, secret) {
						t.Errorf("%q leaked %q: %s", input, secret, got)
					}
				}
				if !strings.Contains(got, "[paslėpta]") || !strings.HasPrefix(got, "before ") || !strings.HasSuffix(got, " after docs/check.go:42") {
					t.Errorf("redaction or surrounding prose lost: %q", got)
				}
			}
		}
	}
	for _, input := range []string{"alice:prefix'TAIL_SENTINEL@example.invalid", "//alice:prefix'TAIL_SENTINEL@example.invalid/repo"} {
		got := redact(input)
		if strings.Contains(got, "alice") || strings.Contains(got, "prefix") || strings.Contains(got, "TAIL_SENTINEL") {
			t.Errorf("scheme-less credentials leaked: %q", got)
		}
	}
}
