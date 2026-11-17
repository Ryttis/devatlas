package git

import (
	"strings"
	"testing"
)

func TestChildEnvironmentExcludesAICredentials(t *testing.T) {
	t.Setenv("DEVATLAS_AI_API_KEY", "sk-must-not-leak")
	t.Setenv("DEVATLAS_AI_MODEL", "m")
	for _, kv := range filteredEnv() {
		if strings.HasPrefix(kv, "DEVATLAS_AI_") {
			t.Errorf("git child environment contains %s", strings.SplitN(kv, "=", 2)[0])
		}
	}
}
