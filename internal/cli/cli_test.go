package cli

import (
	"bytes"
	"strings"
	"testing"
)

func TestRunArgumentErrors(t *testing.T) {
	cases := []struct {
		name     string
		args     []string
		wantCode int
		wantErr  string // substring expected on stderr
	}{
		{"no command", nil, ExitUsage, "missing command"},
		{"unknown command", []string{"frobnicate"}, ExitUsage, `unknown command "frobnicate"`},
		{"version with extra args", []string{"version", "extra"}, ExitUsage, "unexpected arguments: extra"},
		{"serve unknown flag", []string{"serve", "-bogus"}, ExitUsage, "flag provided but not defined: -bogus"},
		{"serve non-numeric port", []string{"serve", "-port", "abc"}, ExitUsage, `invalid value "abc" for flag -port`},
		{"serve port too large", []string{"serve", "-port", "70000"}, ExitUsage, "outside the valid range"},
		{"serve negative port", []string{"serve", "-port", "-1"}, ExitUsage, "outside the valid range"},
		{"serve all interfaces", []string{"serve", "-host", "0.0.0.0"}, ExitUsage, "not a loopback address"},
		{"serve ipv6 any", []string{"serve", "-host", "::"}, ExitUsage, "not a loopback address"},
		{"serve lan address", []string{"serve", "-host", "192.168.1.10"}, ExitUsage, "not a loopback address"},
		{"serve hostname", []string{"serve", "-host", "localhost"}, ExitUsage, "not an IP address"},
		{"serve empty host", []string{"serve", "-host", ""}, ExitUsage, "bind host is empty"},
		{"serve bad log level", []string{"serve", "-log-level", "loud"}, ExitUsage, `unknown log level "loud"`},
		{"serve bad timeout", []string{"serve", "-shutdown-timeout", "0s"}, ExitUsage, "shutdown timeout must be positive"},
		{"serve positional arg", []string{"serve", "extra"}, ExitUsage, "unexpected arguments: extra"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := Run(tc.args, &stdout, &stderr)
			if code != tc.wantCode {
				t.Fatalf("exit code = %d, want %d; stderr:\n%s", code, tc.wantCode, stderr.String())
			}
			if !strings.Contains(stderr.String(), tc.wantErr) {
				t.Fatalf("stderr does not contain %q:\n%s", tc.wantErr, stderr.String())
			}
			if stdout.Len() != 0 {
				t.Fatalf("expected no stdout output on error, got:\n%s", stdout.String())
			}
		})
	}
}

func TestRunHelp(t *testing.T) {
	for _, args := range [][]string{{"help"}, {"-h"}, {"--help"}, {"serve", "-h"}, {"version", "-h"}} {
		var stdout, stderr bytes.Buffer
		if code := Run(args, &stdout, &stderr); code != ExitOK {
			t.Fatalf("%v: exit code = %d, want %d", args, code, ExitOK)
		}
		if !strings.Contains(stdout.String()+stderr.String(), "Usage:") {
			t.Fatalf("%v: expected usage text, got stdout=%q stderr=%q", args, stdout.String(), stderr.String())
		}
	}
}

func TestRunVersion(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"version"}, &stdout, &stderr); code != ExitOK {
		t.Fatalf("exit code = %d, want %d; stderr: %s", code, ExitOK, stderr.String())
	}
	out := stdout.String()
	if !strings.HasPrefix(out, "devatlas ") || !strings.Contains(out, "(commit ") || !strings.Contains(out, "go1.") {
		t.Fatalf("unexpected version output: %q", out)
	}
	if stderr.Len() != 0 {
		t.Fatalf("expected empty stderr, got: %s", stderr.String())
	}
}
