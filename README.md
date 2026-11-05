# GLPI MCP Server

Read-only [MCP](https://modelcontextprotocol.io) server for GLPI projects and project tasks, built on
[MiniMCP](https://github.com/rfletchr/MiniMCP) (stdlib only). Uses GLPI's legacy REST API (`/apirest.php`)
with a service user's API token.

## Tools

**Friendly tools (start here)**: name-based, compact text, long text only as a size until requested.

| Tool | Description |
|---|---|
| `glpi_projects(state?, type?, manager?, text?, parent_id?, overdue?, limit?, page?)` | One line per project |
| `glpi_project(id, include?)` | Summary card; include = tasks, children, description, comment |
| `glpi_tasks(project_id?, state?, assignee?, due_before?, open_only?, overdue?, limit?, page?)` | One line per task |
| `glpi_task(id, include?)` | Summary card; include = description, comment |
| `glpi_people(text)` | Find users by login/name |

**Write tools** exist only when `GLPI_ALLOW_WRITE=1` (off by default; the token must also have GLPI rights to write).
Each previews by default, showing before → after and changing nothing; repeat the identical call with `apply=true` to write.
One item per call, names resolved to ids, unknown names/states/users rejected, no-op calls rejected, and the result is
re-read to flag any value GLPI did not keep. Applied writes log an `AUDIT` line.

| Tool | Description |
|---|---|
| `glpi_create_project(name, description?, code?, state?, type?, manager?, parent_id?, planned_start?, planned_end?)` | Refuses an exact-duplicate name |
| `glpi_update_project(id, name?, description?, state?, percent_done?, manager?, planned_start?, planned_end?)` | Only passed fields change |
| `glpi_create_task(project_id, name, description?, state?, assignee?, parent_task_id?, planned_start?, planned_end?)` | Refuses a duplicate name within the project |
| `glpi_update_task(id, name?, description?, state?, percent_done?, planned_start?, planned_end?)` | Switches off auto-state/percent when you set them manually, and says so |
| `glpi_assign_task(task_id, assignee, action?)` | add (default) or remove one person |

There is no delete. The client allow-lists exactly six endpoints (POST/PUT Project and ProjectTask, POST ProjectTaskTeam,
DELETE ProjectTaskTeam/:id for removing a team link); anything else is refused before it is sent.

**Generic tools** (any item type, arbitrary columns):

| Tool | Description |
|---|---|
| `glpi_initialize` | Workflow, criteria syntax, gotchas — call first |
| `glpi_session` | Active profile, entities, GLPI version (diagnose missing projects) |
| `glpi_item_types` | Item types relevant to project work |
| `glpi_search_options(item_type)` | Columns: numeric id, uid, name, datatype |
| `glpi_search(item_type, criteria?, fields?, sort?, order?, limit?, page?)` | Search; fields/criteria accept id, uid or name; rows keyed by uid |
| `glpi_get(item_type, id, expand_dropdowns?)` | One item |
| `glpi_sub_items(item_type, id, sub_item_type, ...)` | e.g. Project -> ProjectTask |

Read-only by construction: the client only issues GET requests. Also give the GLPI service user a read-only profile.

## Config

Copy `.env.example` to `.env`:

| Variable | Description |
|---|---|
| `GLPI_URL` | Site URL or full `.../apirest.php` URL |
| `GLPI_USER_TOKEN` | Service user's API token (User > Remote access keys) |
| `GLPI_APP_TOKEN` | Optional; only if the API client requires it |
| `GLPI_CA_FILE` | Optional PEM bundle for a private CA |
| `GLPI_INSECURE_TLS` | `1` skips certificate verification (not recommended) |

The service user's profile must be able to see the projects (team membership, or the right to see all projects),
and the API client must allow the server's IP.

## Running

```bash
go run .            # stdio (Claude Code)
go run . -http      # streamable HTTP on :3000
docker compose up -d   # HTTP on 127.0.0.1:3001
```

Claude Code `.mcp.json`: `{"mcpServers":{"glpi":{"command":"glpi-mcp","args":[]}}}`

## Binaries and releases

Pre-built binaries (linux/mac amd64+arm64, windows amd64) are attached to each
[GitHub release](https://github.com/rfletchr/GlpiMcpGo/releases). Download the one for your platform, put the
GLPI settings from the table above in `.env` next to it (or in the MCP client's `env` block), then either run it
over stdio (`./glpi-mcp-linux-amd64`) or as an HTTP server (`./glpi-mcp-linux-amd64 -http -addr 127.0.0.1:3001`).

Cutting a release (maintainer): tag with an annotated message, which becomes the release notes, and push the tag.
GitHub Actions runs `scripts/release.sh`: govulncheck, `go test`, cross-compile, `gh release create`.

```bash
git tag -a v0.1.0 -m "First release: read tools, friendly layer, gated write tools"
git push origin v0.1.0
```

## Tests

`go test ./...` runs the client against a fake GLPI (httptest). Real response shapes are unverified until
run against a live instance.

## Local test instance

`dev/docker-compose.yml` runs a throwaway GLPI 11 + MariaDB on `127.0.0.1:8088` (web login `glpi` / `glpi`).
After first start: enable the API (`php bin/console config:set enable_api 1`, `enable_api_login_external_token 1`),
create a service user with a Read-Only profile and an API token, and widen the default API client's IP range.
The service profile's project right must be "See all" **without** "See (my)", or GLPI lists no ProjectTasks.
