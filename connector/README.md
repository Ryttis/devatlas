# DevAtlas agent status connector

This separate Go program lets an authenticated MCP client read the agent
runner's recorded state. The DevAtlas inventory server stays on loopback and
does not need to be exposed. The connector does not call a model or start jobs.

- `list_agent_runs`: up to 20 runs, newest update first (default 10).
- `get_agent_run`: one run by its ID. Returns the stage, recorded update time,
  base and head SHAs, confirmed Go checks, and review verdict, when available.

The connector reads only `state.json`. Task prompts, code, local paths, raw
errors, logs, and the text of review findings are not returned. A nonterminal
stage describes the last recorded update; it does not prove a job is still
running. Runs created by older manual pilot scripts have no `state.json` and
are not included.

## Build and check

Use a supported Go version, at least 1.25 for this module. Go 1.27.3 from the
runner host works for both modules. From the repository root:

```sh
cd connector
go build -o ../bin/devatlas-mcp .
go vet ./...
go test -race ./...
```

The tests simulate an HTTPS OAuth introspection endpoint and make real MCP
requests in process; they do not call ChatGPT or use a real account.

## Deploy on the runner host

1. Use a dedicated HTTPS hostname and an OAuth provider that supports MCP's
   authorization-code flow with PKCE and access-token introspection. Configure
   a client for ChatGPT as described in the provider and [OpenAI auth guide][auth],
   and a confidential client for introspection. Grant only `runs:read` for
   this resource. Restrict authorization to the intended account.
2. Set the variables below in the service's private environment file. The
   introspection endpoint must return `active`, `iss`, `sub`, `aud`, `scope`,
   `exp`, and `token_type: Bearer` for valid access tokens. The exact issuer,
   resource audience, and owner subject must match. The connector rejects
   missing or invalid credentials and other users on every MCP request.

   ```text
   DEVATLAS_MCP_RESOURCE_URL=https://devatlas.example.org/mcp
   DEVATLAS_MCP_OAUTH_ISSUER=https://identity.example.org/realms/devatlas
   DEVATLAS_MCP_INTROSPECTION_URL=https://identity.example.org/realms/devatlas/protocol/openid-connect/token/introspect
   DEVATLAS_MCP_INTROSPECTION_CLIENT_ID=devatlas-resource
   DEVATLAS_MCP_INTROSPECTION_CLIENT_SECRET=<store privately>
   DEVATLAS_MCP_OWNER_SUBJECT=<exact stable subject from this issuer>
   ```

   Run `bin/devatlas-mcp` on the agent runner host, under the account that
   can read its private state directory. Use the same `XDG_STATE_HOME` as the
   runner if configured. The process listens only on `127.0.0.1:18643`.
3. Configure the HTTPS reverse proxy to forward only `/mcp` and
   `/.well-known/oauth-protected-resource/mcp` to that loopback listener.
   Forward `Authorization` unchanged and set the upstream `Host` to
   `127.0.0.1:18643` so the SDK can keep its loopback host protection on.
   Do not log authorization headers or publish the inventory server. Check
   that an unauthenticated MCP request returns `401` with a resource-metadata
   challenge and that the metadata URL resolves over HTTPS.
4. On [ChatGPT Plugins](https://chatgpt.com/plugins) in the browser, choose
   **Add custom MCP server**, enter the public `/mcp` URL, and configure OAuth
   for the chosen provider. After sign-in, test "Use DevAtlas to show the latest
   agent runs." Check that the plugin is available in the same account and
   workspace on iPhone before treating mobile access as verified.

The work computer and phone do not need an SSH connection to use a configured
connector. The runner host, HTTPS route, and identity provider must be online.
This code does not provision any of those services or register a plugin.

[auth]: https://developers.openai.com/plugins/build/auth
