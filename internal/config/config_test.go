package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testKey = "sk-test-SECRET-123456"

func writeEnv(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadReadsFileIntoStructureAndScrubsEnvironment(t *testing.T) {
	t.Setenv(EnvAIAPIKey, "from-process-env")
	t.Setenv(EnvAIModel, "env-model")
	p := writeEnv(t, "# comment\nexport DEVATLAS_AI_BASE_URL=\"https://api.example.com/v1/\"\nDEVATLAS_AI_MODEL=file-model # trailing\nDEVATLAS_AI_API_KEY='"+testKey+"'\nOTHER=1\n")
	ai, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if ai.BaseURL != "https://api.example.com/v1" || ai.Model != "file-model" || ai.APIKey.Reveal() != testKey || !ai.Enabled() {
		t.Fatalf("unexpected configuration: %+v", ai)
	}
	// Neither the file values nor the inherited ones may stay in the
	// environment, where a child process would inherit them.
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, EnvPrefix) {
			t.Errorf("variable left in the process environment: %s", strings.SplitN(kv, "=", 2)[0])
		}
	}
}

func TestLoadFallsBackToEnvironmentAndMissingFileMeansDisabled(t *testing.T) {
	ai, err := Load(filepath.Join(t.TempDir(), "absent.env"))
	if err != nil || ai.Enabled() {
		t.Fatalf("missing file must mean disabled, got %+v, %v", ai, err)
	}
	t.Setenv(EnvAIBaseURL, "http://127.0.0.1:11434/v1")
	t.Setenv(EnvAIModel, "local")
	ai, err = Load("")
	if err != nil || !ai.Enabled() || ai.APIKey.IsSet() {
		t.Fatalf("environment fallback failed: %+v, %v", ai, err)
	}
}

func TestSecretNeverPrints(t *testing.T) {
	ai := AI{BaseURL: "https://x.example/v1", Model: "m", APIKey: NewSecret(testKey)}
	var logs bytes.Buffer
	slog.New(slog.NewTextHandler(&logs, nil)).Info("cfg", "ai", ai, "key", ai.APIKey)
	js, _ := json.Marshal(ai)
	for name, out := range map[string]string{
		"%v": fmt.Sprintf("%v", ai), "%+v": fmt.Sprintf("%+v", ai), "%#v": fmt.Sprintf("%#v", ai),
		"%s": fmt.Sprintf("%s", ai.APIKey), "json": string(js), "slog": logs.String(),
	} {
		if strings.Contains(out, testKey) {
			t.Errorf("%s output contains the key: %s", name, out)
		}
	}
}

func TestValidateRejectsUnsafeConfiguration(t *testing.T) {
	for _, tc := range []AI{
		{BaseURL: "https://x.example/v1"},
		{Model: "m"},
		{BaseURL: "http://api.example.com/v1", Model: "m"},
		{BaseURL: "https://user:pw@x.example/v1", Model: "m"},
		{BaseURL: "ftp://x.example", Model: "m"},
		{BaseURL: "https://x.example/v1", Model: "bad model"},
	} {
		if err := tc.Validate(); err == nil {
			t.Errorf("expected an error for %+v", tc)
		} else if strings.Contains(err.Error(), "pw") {
			t.Errorf("error text contains a credential: %v", err)
		}
	}
	for _, ok := range []AI{{}, {BaseURL: "http://localhost:1234/v1", Model: "m"}, {BaseURL: "http://192.168.1.5:8080/v1", Model: "m"}} {
		if err := ok.Validate(); err != nil {
			t.Errorf("unexpected error for %+v: %v", ok, err)
		}
	}
}

func TestParseFileErrorsDoNotQuoteValues(t *testing.T) {
	p := writeEnv(t, "GOOD=1\n"+testKey+"\n")
	_, err := ParseFile(p)
	if err == nil || strings.Contains(err.Error(), testKey) {
		t.Fatalf("expected an error without the value, got %v", err)
	}
}
