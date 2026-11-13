package runhistory

import (
	"regexp"
	"strings"
)

var redactions = []*regexp.Regexp{
	regexp.MustCompile(`(?s)-----BEGIN [^-]*PRIVATE KEY-----.*?(-----END [^-]*PRIVATE KEY-----|$)`),
	// Any scheme: the whole URI is masked, so userinfo never depends on a scheme list.
	regexp.MustCompile(`(?i)\b[a-z][a-z0-9+.-]*://[^\s<>"']+`),
	// user:password@host without a recognizable scheme prefix.
	regexp.MustCompile(`[^\s/:@<>"']+:[^\s/@<>"']+@[A-Za-z0-9\[][A-Za-z0-9.:\]-]*`),
	regexp.MustCompile(`(?i)\b(?:Bearer|Basic)\s+[A-Za-z0-9+/_.=~-]+`),
	regexp.MustCompile(`(?i)\b[A-Za-z0-9_]*(?:secret|token|password|passwd|api[_-]?key)[A-Za-z0-9_]*["']?\s*[:=]\s*(?:"[^"\n]*"|'[^'\n]*'|[^\s,;]+)`),
	regexp.MustCompile(`\b(?:sk-[A-Za-z0-9_-]{8,}|gh[pousr]_[A-Za-z0-9_]{8,}|github_pat_[A-Za-z0-9_]+|eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+)`),
	regexp.MustCompile(`/(?:home|root|Users)/[^\s<>"']+`),
}

// Defense in depth for common credential formats in agent prose, not a claim
// that arbitrary secrets can be detected. Only owner-authenticated users may
// read reports. Raw logs, prompts, environment and unknown JSON fields are not read.
func redact(s string) string {
	for _, pattern := range redactions {
		s = pattern.ReplaceAllString(s, "[paslėpta]")
	}
	s = strings.Map(func(r rune) rune {
		if (r < 32 && r != '\n' && r != '\t') || r == 127 || (r >= 0x202a && r <= 0x202e) || (r >= 0x2066 && r <= 0x2069) {
			return -1
		}
		return r
	}, s)
	r := []rune(s)
	if len(r) > 4000 {
		return string(r[:4000]) + "… [sutrumpinta]"
	}
	return s
}
