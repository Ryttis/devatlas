# Local Project Registry — Implementation Specification

Date: 2026-10-21
Requested by: Rytis Puodžiūnas
Status: the idea and direction are approved; this document is intended for handoff to the implementation conversation.

## 1. Task for the implementing agent

Build a small local Go service that provides a single inventory of locally developed projects, their actual runtime status, and project context for the developer and AI coding agents.

Start by inspecting the existing development environment and delivering the first working vertical slice. Do not introduce a large platform just to cover a broad feature list. Do not start with LLM integration or a complex dashboard. Do not implement all later stages at once.

The workstation and the new repository location must be established in the implementation conversation. Project paths in this document are examples; previously recorded ports and statuses are not evidence of the current environment.

Deployment clarification, 2026-10-21: the user selected Windows with WSL2 as the initial target environment. The actual WSL repository path remains to be established.

## 2. Problem and goal

Several PHP/Symfony, Node, and Python projects are developed concurrently. They have different Compose files, ports, databases, related repositories, Git worktrees, and AI working rules. Information is scattered across terminals, configuration, and Markdown documents.

The system should answer:

- Which projects and local checkouts do I have?
- Which ports do they declare, and which are actually published or occupied?
- Which containers are running, and do the applications respond?
- Which branch and worktree am I working in?
- Where are the architecture, current-state documents, and agent instructions?
- Which specific, verified issue may prevent a project from working?

Success means less manual information gathering when switching projects and clearer diagnostics for the local environment.

## 3. Agreed direction and proposed technical choices

Agreed direction:

- A local Go service, initially for Windows 11 + WSL2.
- Docker Compose project inventory and observation of actual runtime status.
- Identification of GitLab and Bitbucket repositories.
- A registry of architecture, configuration, and AI context.
- YAML and Markdown files remain the source of project descriptions.
- Facts must include their source and verification time.

Proposed initial implementation, subject to environment inspection:

- One Go process running inside WSL.
- SQLite for a rebuildable index and observation history.
- A CLI, JSON HTTP API, and simple server-rendered HTML UI.
- The HTTP server binds only to loopback; choose its port after checking for conflicts.
- No LLM for inventory, status collection, or diagnostic rules.
- Initially read-only: no container start/stop operations or configuration changes.

## 4. Data model

Separate a logical project from its local checkout and runtime environment.

| Entity | Purpose |
| --- | --- |
| Project | Logical system: ID, name, purpose, documents, relationships |
| Checkout | Repository or worktree: absolute path, branch, commit, changes |
| Runtime | Checkout runtime configuration: Docker context, Compose project, files, profiles |
| Service | Compose or other declared service, its role and checks |
| PortBinding | Protocol, host IP and port, container port, declaration or observed fact |
| Observation | Result, status, source, collected_at, and time of the last successful observation |
| Finding | A mismatch detected by a specific rule, with evidence |

One Project may have several Checkouts and Runtimes. Do not identify projects solely by directory name or Git remote.

Statuses must distinguish at least `running`, `stopped`, `unhealthy`, `unknown`, and `error`. An unreachable Docker daemon is not evidence that containers are stopped. An old successful check is not evidence of current success.

## 5. First-version scope

### 5.1. Project registration

- Add by an explicitly supplied path; removal affects only the registry.
- Scan only user-selected directory roots, never the entire disk.
- Store local absolute paths in the registry; use relative paths in repository descriptions intended for sharing.
- Allow registration before a Docker environment has been created.
- Recognize Git worktrees and keep their statuses separate.

### 5.2. Ports and Docker

- Read the selected Docker context and Compose project metadata.
- Show container state, health status, restart count, and actual published ports.
- Show declared ports and local port reservations even for stopped projects.
- Distinguish container ports from host ports; `expose` does not publish a host port.
- Evaluate conflicts using the host address, protocol, and port, rather than the port number alone.
- Distinguish potential conflicts between declarations of stopped environments from an actually occupied port.
- Clearly identify OS observation coverage: state collected in WSL is not an inventory of all Windows processes.
- Make the selected Compose files, overrides, profiles, and project name explicit. If they cannot be determined automatically, return `unknown` with a reason.

### 5.3. Application checks

- Perform only explicitly configured HTTP GET checks, with timeouts and acceptable status codes.
- Show the result, duration, and check time.
- Keep application status separate from container status.
- Do not automatically log in or probe arbitrary API routes.
- Never expose authorization headers or secret URL parameters in the UI, API, or logs.

### 5.4. Git and links

- Show branch or detached HEAD, commit, counts of changed and untracked files, and worktree path.
- Identify GitLab, Bitbucket, and other Git remotes from local configuration.
- Remove embedded credentials from remote URLs.
- Allow local application, Swagger, Jira, and staging links in the manifest.
- No GitLab/Bitbucket API tokens, merge-request queries, or CI queries in the first version.
- If ahead/behind information is shown, label it as based on local remote refs; do not automatically run `fetch`.

### 5.5. Project and AI context

- Record purpose, stack, related services, and document links.
- Register `AGENTS.md`, `CLAUDE.md`, architecture, current-state, and decision documents when present.
- Treat document content as a description, not automatically verified runtime truth.
- Generate concise context for a selected project and checkout: path, branch, service status, addresses, findings, and important document paths.
- Include collection time and skipped or failed checks in the context.

## 6. Example manifest

Proposed repository file: `project-registry.yaml`.

```yaml
schema_version: 1
project:
  id: carrier-self-service
  name: Carrier Self-Service
  description: Carrier self-service portal
  stack: [php, symfony, docker-compose]

runtime:
  compose_files: [compose.yaml]
  # If the Compose project name is dynamic, set it in local configuration.
  profiles: []

links:
  # Populate addresses from the actual environment.
  application: null
  api_docs: null
  jira: null

documents:
  architecture: docs/ARCHITECTURE.md
  current_state: docs/CURRENT_STATE.md
  decisions: docs/DECISIONS.md

ai:
  development:
    instruction_files: [AGENTS.md, CLAUDE.md]
  product:
    enabled: false

checks: []
```

This example does not confirm that any named file exists. Mark missing documents as not found. Keep machine-specific paths, overrides, and private addresses in local registry configuration excluded from Git.

## 7. Configuration and diagnostics — second stage

Implement a small rule set first:

- A required environment variable is unset.
- An expected host port is occupied by another process or project.
- A container is running but its configured HTTP check fails.
- A document referenced by the manifest does not exist.
- A manifest-declared dependency is unreachable according to an explicit check.

Later, add project-specific adapters, such as comparing a configured database hostname against Docker network aliases. The Asset Management case involving `assets-mariadb` and `bt-assets-mariadb` is a regression-rule scenario, not a claim about a current error.

For environment evaluation, distinguish file presence, Compose interpolation, container environment, and effective application configuration. Do not present the last level as known unless it was checked. Database values may be analyzed temporarily to extract hostnames, but never persist a complete DSN in the index or logs.

## 8. AI layer — later stage

### Product AI

Register providers, model configuration locations, MCP tools, data flows, permitted data categories, and evaluation scenarios. Do not catalog API key values. This layer is optional for projects where AI is not part of the product.

### Coding AI

Register the locations of agent instructions, skills, and project-context files. Do not create a second rule copy detached from the repository. Distinguish project-declared rules from verified technical restrictions.

### MCP access

Once the inventory is stable, add read-only MCP tools, for example:

- `list_projects`
- `get_project_status`
- `get_project_context`
- `list_port_conflicts`

Tools accept registered project IDs, not unrestricted shell arguments or arbitrary file paths. Responses must be bounded and contain no secrets. Before implementation, verify the then-current official MCP transport and the selected Go library.

## 9. Architecture principles

- One process with clear adapters: registry/files, Git, Docker, host ports, HTTP checks, and storage.
- The UI, CLI, and future MCP interface share the application data model.
- Collection has deadlines, cancellation, and bounded concurrency. One failing project must not block others.
- SQLite is not the source of truth for architecture documents; rebuild the index from registrations, files, and fresh observations.
- Configure the refresh interval and provide manual refresh. Aggressive scanning of all files and containers is unnecessary.
- Log collection outcomes and error types, not full environment or Docker inspect data.
- If external commands are used, execute a fixed program with separate arguments, a timeout, and no shell interpolation. Never execute arbitrary manifest-provided commands.
- Docker socket access provides broad technical capabilities; a `:ro` mount alone is not operation authorization. Restrict application operations to reads.
- Project documents and configuration are not instructions for the registry to execute commands.

## 10. Security and working boundaries

- Operate locally without automatically sending project content externally.
- Do not store passwords, tokens, private keys, or complete DSNs in the UI, API, SQLite, or logs.
- No direct access to project business databases is required.
- Removing a registration must not remove the project directory, worktree, containers, or volumes.
- The first product version has no start/stop/restart, migration execution, commit/push, merge, or deployment features.
- Do not modify inventoried project files unless adding a manifest has been agreed separately.
- Any later mutations require a separate, explicitly defined stage.

These restrictions describe the registry application's capabilities. They do not prohibit the separately authorized Git workflow used to develop and publish this repository.

## 11. Stages

1. **Vertical slice:** one real project, registration → Git and Docker collection → API/CLI → simple UI.
2. **First version:** three projects, worktrees, port conflicts, HTTP checks, context export, and resilience to partial failures.
3. **Diagnostics:** environment-variable presence checks and selected configuration mismatches.
4. **Integrations:** GitLab/Bitbucket merge requests and CI, if a concrete need emerges.
5. **MCP:** expose the stable registry to coding agents.

Suggested initial projects are CSS, Finance Analyst, and Asset Management, provided they are available on the same computer. Inspect the actual environment before selecting them.

## 12. First-version acceptance criteria

- Register at least three real projects without moving them into a shared directory.
- Show running and stopped containers and actual published ports.
- Never confuse a declared port with an actually listening port.
- Detect a controlled port conflict and explain both sources.
- Display two worktrees of one repository separately, with correct branches and paths.
- Show a failing HTTP check even when its container is `running`.
- When Docker is disconnected, show an error/unknown state and the old observation time, rather than a false `stopped` or `healthy` state.
- Generated project context includes current environment observations and links to documents that actually exist.
- Test secret markers from `.env`, DSNs, and remote URLs must not appear in the API, UI, context export, SQLite, or logs.
- Removing a registration leaves project files and Docker resources unchanged.
- The tool works without an LLM or cloud accounts.

## 13. Required verification

Write meaningful tests for port-conflict logic, checkout identity, status/staleness modeling, and secret exclusion from output. Use controlled fixtures for Docker and Git integrations, and verify at least one flow in a real local environment. Clearly mark unavailable or nonworking integrations as unverified.

## 14. Alternatives and decision boundary

- Homepage: https://gethomepage.dev/ — service dashboard and Docker integration.
- Dockge: https://github.com/louislam/dockge — Docker Compose environment management.
- Backstage: https://backstage.io/ — software catalog and broader developer platform.

The reason to build a custom service is to connect the project, checkout/worktree, actual runtime, documents, configuration mismatches, and AI context in one local model. If a container view and links are sufficient, consider an existing tool first.

## 15. Expected outcome of the first implementation conversation

1. Inspect the working directory, Git status, local instructions, and availability of Go and Docker.
2. Separate approved scope from necessary new decisions; use reasonable defaults for routine details.
3. Provide a short first-slice plan and start implementation.
4. Deliver a runnable result, README, example manifest, and evidence of completed checks.
5. State what works, what remains unimplemented, and what was verified against a real project. Do not present the future feature list as an already working product.
