// Package config reads the DevAtlas .env file into a structure. Values are
// never exported into the process environment, so child processes such as
// git cannot inherit the API key.
package config

import (
	"bufio"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"strings"
)

// Variable names understood in the .env file and, as a fallback, in the
// process environment.
const (
	EnvAIBaseURL = "DEVATLAS_AI_BASE_URL"
	EnvAIModel   = "DEVATLAS_AI_MODEL"
	EnvAIAPIKey  = "DEVATLAS_AI_API_KEY"

	// EnvPrefix marks variables that must not reach child processes.
	EnvPrefix = "DEVATLAS_AI_"

	maxEnvFileBytes = 64 << 10
)

// Secret holds a credential. Every formatting, logging, and JSON path
// yields a placeholder; only Reveal returns the value.
type Secret struct{ v string }

// NewSecret wraps a credential.
func NewSecret(v string) Secret { return Secret{v: v} }

// Reveal returns the credential for use in an Authorization header.
func (s Secret) Reveal() string { return s.v }

// IsSet reports whether a credential is present.
func (s Secret) IsSet() bool { return s.v != "" }

func (s Secret) String() string {
	if s.v == "" {
		return ""
	}
	return "[redacted]"
}

// GoString keeps %#v from printing the value.
func (s Secret) GoString() string { return s.String() }

// MarshalText keeps JSON and YAML encoders from printing the value.
func (s Secret) MarshalText() ([]byte, error) { return []byte(s.String()), nil }

// LogValue keeps slog from printing the value.
func (s Secret) LogValue() slog.Value { return slog.StringValue(s.String()) }

// AI is the configuration of the OpenAI-compatible explanation adapter.
type AI struct {
	// BaseURL is the API root that has /chat/completions below it, for
	// example https://api.openai.com/v1.
	BaseURL string
	Model   string
	APIKey  Secret
}

// Enabled reports whether explanations can be requested.
func (a AI) Enabled() bool { return a.BaseURL != "" && a.Model != "" }

// Host returns the API host for display; it never includes credentials.
func (a AI) Host() string {
	u, err := url.Parse(a.BaseURL)
	if err != nil {
		return ""
	}
	return u.Host
}

// Validate checks a configured adapter. An entirely empty configuration is
// valid and means "disabled".
func (a AI) Validate() error {
	if a.BaseURL == "" && a.Model == "" && !a.APIKey.IsSet() {
		return nil
	}
	if a.BaseURL == "" || a.Model == "" {
		return fmt.Errorf("%s and %s must be set together", EnvAIBaseURL, EnvAIModel)
	}
	u, err := url.Parse(a.BaseURL)
	if err != nil || u.Host == "" {
		return fmt.Errorf("%s is not a valid URL", EnvAIBaseURL)
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("%s must not contain credentials, a query, or a fragment", EnvAIBaseURL)
	}
	switch u.Scheme {
	case "https":
	case "http":
		if !localHost(u.Hostname()) {
			return fmt.Errorf("%s uses http for a non-local host; use https", EnvAIBaseURL)
		}
	default:
		return fmt.Errorf("%s must use http or https", EnvAIBaseURL)
	}
	if len(a.Model) > 200 || strings.ContainsAny(a.Model, "\r\n\t ") {
		return fmt.Errorf("%s is not a valid model name", EnvAIModel)
	}
	return nil
}

// localHost accepts loopback and private addresses, where plain http is the
// norm for locally hosted models.
func localHost(h string) bool {
	if h == "localhost" || h == "host.docker.internal" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && (ip.IsLoopback() || ip.IsPrivate())
}

// Load reads the AI configuration. Values in the file at path win; missing
// ones fall back to the process environment. A missing file is not an error.
// The DEVATLAS_AI_* variables are then removed from the process environment
// so nothing started later can inherit them.
func Load(path string) (AI, error) {
	vals := map[string]string{}
	if path != "" {
		parsed, err := ParseFile(path)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return AI{}, err
		}
		vals = parsed
	}
	get := func(k string) string {
		if v, ok := vals[k]; ok {
			return v
		}
		return os.Getenv(k)
	}
	ai := AI{
		BaseURL: strings.TrimRight(strings.TrimSpace(get(EnvAIBaseURL)), "/"),
		Model:   strings.TrimSpace(get(EnvAIModel)),
		APIKey:  NewSecret(strings.TrimSpace(get(EnvAIAPIKey))),
	}
	ScrubEnv()
	if err := ai.Validate(); err != nil {
		return AI{}, err
	}
	return ai, nil
}

// ScrubEnv removes every DEVATLAS_AI_* variable from the process environment.
func ScrubEnv() {
	for _, kv := range os.Environ() {
		if k, _, _ := strings.Cut(kv, "="); strings.HasPrefix(k, EnvPrefix) {
			_ = os.Unsetenv(k)
		}
	}
}

// ParseFile parses KEY=VALUE lines. Blank lines and lines starting with '#'
// are ignored, an optional "export " prefix is accepted, and a value may be
// wrapped in single or double quotes. No interpolation or command
// substitution is performed. Error messages never include values.
func ParseFile(path string) (map[string]string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("environment file %s is not a regular file", path)
	}
	if info.Size() > maxEnvFileBytes {
		return nil, fmt.Errorf("environment file %s is larger than %d bytes", path, maxEnvFileBytes)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := map[string]string{}
	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		k, v, ok := strings.Cut(line, "=")
		k = strings.TrimSpace(k)
		if !ok || k == "" || strings.ContainsAny(k, " \t\"'") {
			return nil, fmt.Errorf("environment file %s line %d: expected KEY=VALUE", path, n)
		}
		v = strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
			v = v[1 : len(v)-1]
		} else if i := strings.Index(v, " #"); i >= 0 {
			v = strings.TrimSpace(v[:i])
		}
		out[k] = v
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("environment file %s: unreadable", path)
	}
	return out, nil
}
