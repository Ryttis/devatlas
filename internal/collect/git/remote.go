package git

import (
	"net/url"
	"regexp"
	"sort"
	"strings"
)

// Providers recognised by exact host match. Self-hosted instances are
// reported as "unknown" because a host name alone does not prove which
// product runs on it.
var knownHosts = map[string]string{
	"github.com":           "github",
	"www.github.com":       "github",
	"gist.github.com":      "github",
	"ssh.github.com":       "github",
	"gitlab.com":           "gitlab",
	"www.gitlab.com":       "gitlab",
	"altssh.gitlab.com":    "gitlab",
	"bitbucket.org":        "bitbucket",
	"www.bitbucket.org":    "bitbucket",
	"altssh.bitbucket.org": "bitbucket",
}

// Provider constants.
const (
	ProviderLocal   = "local"
	ProviderUnknown = "unknown"
)

var sshSchemes = map[string]bool{"ssh": true, "git+ssh": true, "ssh+git": true}

// scpStyle matches "[user@]host:path" remotes. The host part must not
// contain "/" or ":" and the whole thing must not look like a Windows drive
// or a local path.
var scpStyle = regexp.MustCompile(`^(?:([^@/]+)@)?([^@/:]+):(.*)$`)

// Sanitize removes credentials and other sensitive components from a remote
// URL and classifies its host. Rules:
//
//   - URLs with a scheme are parsed. The password is always removed. For
//     ssh:// (and git+ssh://) the user name is kept because it addresses the
//     account used for SSH; for every other scheme the whole user info is
//     removed. Query strings and fragments are removed.
//   - scp-style "user@host:path" keeps user and host. A "user:password@"
//     prefix loses the password.
//   - Local paths and file:// URLs are returned unchanged with provider
//     "local".
//   - Anything that cannot be parsed is replaced by a placeholder so an
//     unparsed secret can never leak through.
//
// Sanitized reports whether anything was removed.
func Sanitize(raw string) Remote {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return Remote{URL: "", Provider: ProviderUnknown}
	}
	if strings.Contains(raw, "://") {
		return sanitizeURL(raw)
	}
	if m := scpStyle.FindStringSubmatch(raw); m != nil && !strings.HasPrefix(raw, ".") && !strings.HasPrefix(raw, "/") && !looksLikeDrive(raw) {
		user, host, path := m[1], strings.ToLower(m[2]), m[3]
		r := Remote{Host: host, Provider: providerFor(host)}
		if i := strings.IndexByte(user, ':'); i >= 0 {
			user = user[:i]
			r.Sanitized = true
		}
		if user != "" {
			r.URL = user + "@" + host + ":" + path
		} else {
			r.URL = host + ":" + path
		}
		return r
	}
	return Remote{URL: raw, Provider: ProviderLocal}
}

func sanitizeURL(raw string) Remote {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" && u.Scheme != "file" {
		return Remote{URL: "(unparseable remote URL, hidden)", Provider: ProviderUnknown, Sanitized: true}
	}
	r := Remote{}
	scheme := strings.ToLower(u.Scheme)
	if u.User != nil {
		_, hasPassword := u.User.Password()
		if sshSchemes[scheme] {
			if hasPassword {
				r.Sanitized = true
			}
			u.User = url.User(u.User.Username())
		} else {
			r.Sanitized = true
			u.User = nil
		}
	}
	if u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawFragment != "" {
		r.Sanitized = true
		u.RawQuery, u.ForceQuery, u.Fragment, u.RawFragment = "", false, "", ""
	}
	r.URL = u.String()
	if scheme == "file" {
		r.Provider = ProviderLocal
		return r
	}
	r.Host = strings.ToLower(u.Hostname())
	r.Provider = providerFor(r.Host)
	return r
}

func providerFor(host string) string {
	if host == "" {
		return ProviderLocal
	}
	if p, ok := knownHosts[host]; ok {
		return p
	}
	return ProviderUnknown
}

func looksLikeDrive(s string) bool {
	return len(s) >= 2 && s[1] == ':' && (s[0]|0x20 >= 'a' && s[0]|0x20 <= 'z') && (len(s) == 2 || s[2] == '\\' || s[2] == '/')
}

// parseRemoteConfig interprets "git config -z --get-regexp" output: each
// record is "key\nvalue" terminated by NUL.
func parseRemoteConfig(out []byte) []Remote {
	var remotes []Remote
	for _, rec := range strings.Split(string(out), "\x00") {
		if rec == "" {
			continue
		}
		key, value, _ := strings.Cut(rec, "\n")
		name, role, ok := remoteKey(key)
		if !ok {
			continue
		}
		r := Sanitize(value)
		r.Name, r.Role = name, role
		remotes = append(remotes, r)
	}
	sort.SliceStable(remotes, func(i, j int) bool {
		if remotes[i].Name != remotes[j].Name {
			return remotes[i].Name < remotes[j].Name
		}
		return remotes[i].Role < remotes[j].Role
	})
	if remotes == nil {
		remotes = []Remote{}
	}
	return remotes
}

func remoteKey(key string) (name, role string, ok bool) {
	rest, found := strings.CutPrefix(key, "remote.")
	if !found {
		return "", "", false
	}
	switch {
	case strings.HasSuffix(rest, ".pushurl"):
		return strings.TrimSuffix(rest, ".pushurl"), "push", true
	case strings.HasSuffix(rest, ".url"):
		return strings.TrimSuffix(rest, ".url"), "fetch", true
	}
	return "", "", false
}
