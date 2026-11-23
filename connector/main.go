package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const metadataPath = "/.well-known/oauth-protected-resource/mcp"

type listInput struct {
	Limit int `json:"limit,omitempty" jsonschema:"Maximum recent runs to return: 1–20; default 10"`
}

type getInput struct {
	ID string `json:"run_id" jsonschema:"Run ID returned by list_agent_runs"`
}

func connector(store runStore, cfg accessConfig, client *http.Client) http.Handler {
	server := mcp.NewServer(&mcp.Implementation{Name: "devatlas-agent-status", Version: "0.1.0"}, nil)
	annotation := &mcp.ToolAnnotations{ReadOnlyHint: true}
	mcp.AddTool(server, &mcp.Tool{Name: "list_agent_runs", Title: "List agent runs",
		Description: "Read the latest DevAtlas agent run states, Go check results, and review verdicts. Does not read task or review text or start a run.", Annotations: annotation},
		func(_ context.Context, _ *mcp.CallToolRequest, in listInput) (*mcp.CallToolResult, runList, error) {
			out, err := store.list(in.Limit)
			return nil, out, err
		})
	mcp.AddTool(server, &mcp.Tool{Name: "get_agent_run", Title: "Get agent run status",
		Description: "Read one DevAtlas agent run state and verdict by run ID. Nonterminal stages are the last recorded state, not proof that a process is still alive.", Annotations: annotation},
		func(_ context.Context, _ *mcp.CallToolRequest, in getInput) (*mcp.CallToolResult, runStatus, error) {
			out, err := store.get(in.ID)
			return nil, out, err
		})
	stream := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server },
		&mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true, MaxRequestBodyBytes: 32 << 10})
	publicURL, _ := url.Parse(cfg.resource) // validated at startup
	metadataURL := publicURL.ResolveReference(&url.URL{Path: metadataPath}).String()
	requireToken := auth.RequireBearerToken(cfg.verifyWith(client), &auth.RequireBearerTokenOptions{
		ResourceMetadataURL: metadataURL, Scopes: []string{readScope},
	})
	mux := http.NewServeMux()
	mux.Handle("/mcp", requireToken(stream))
	mux.HandleFunc("GET "+metadataPath, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(struct {
			Resource             string   `json:"resource"`
			AuthorizationServers []string `json:"authorization_servers"`
			ScopesSupported      []string `json:"scopes_supported"`
			BearerMethods        []string `json:"bearer_methods_supported"`
		}{cfg.resource, []string{cfg.issuer}, []string{readScope}, []string{"header"}})
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		mux.ServeHTTP(w, r)
	})
}

func httpsURL(value string) (*url.URL, error) {
	u, err := url.Parse(value)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil ||
		u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return nil, errors.New("expected an absolute HTTPS URL without credentials, query, or fragment")
	}
	return u, nil
}

func configFromEnv() (runStore, accessConfig, error) {
	cfg := accessConfig{
		resource:      os.Getenv("DEVATLAS_MCP_RESOURCE_URL"),
		issuer:        os.Getenv("DEVATLAS_MCP_OAUTH_ISSUER"),
		introspection: os.Getenv("DEVATLAS_MCP_INTROSPECTION_URL"),
		clientID:      os.Getenv("DEVATLAS_MCP_INTROSPECTION_CLIENT_ID"),
		clientSecret:  os.Getenv("DEVATLAS_MCP_INTROSPECTION_CLIENT_SECRET"),
		owner:         os.Getenv("DEVATLAS_MCP_OWNER_SUBJECT"),
	}
	for _, pair := range []struct{ key, value string }{
		{"DEVATLAS_MCP_RESOURCE_URL", cfg.resource}, {"DEVATLAS_MCP_OAUTH_ISSUER", cfg.issuer},
		{"DEVATLAS_MCP_INTROSPECTION_URL", cfg.introspection},
	} {
		u, err := httpsURL(pair.value)
		if err != nil || (pair.key == "DEVATLAS_MCP_RESOURCE_URL" && u.EscapedPath() != "/mcp") {
			return runStore{}, cfg, errors.New("invalid " + pair.key)
		}
	}
	if cfg.clientID == "" || cfg.clientSecret == "" || cfg.owner == "" {
		return runStore{}, cfg, errors.New("introspection client ID, secret and owner subject are required")
	}
	state := os.Getenv("XDG_STATE_HOME")
	if state == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return runStore{}, cfg, errors.New("cannot determine the runner state directory")
		}
		state = filepath.Join(home, ".local", "state")
	}
	if !filepath.IsAbs(state) {
		return runStore{}, cfg, errors.New("XDG_STATE_HOME must be absolute")
	}
	return runStore{dir: filepath.Join(state, "devatlas", "agent-runs")}, cfg, nil
}

func main() {
	store, cfg, err := configFromEnv()
	if err != nil {
		log.Fatal(err)
	}
	client := &http.Client{Timeout: 5 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	server := &http.Server{Addr: "127.0.0.1:18643", Handler: connector(store, cfg, client),
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second,
		WriteTimeout: 20 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16 << 10}
	log.Printf("DevAtlas MCP listening on %s", server.Addr)
	log.Fatal(server.ListenAndServe())
}
