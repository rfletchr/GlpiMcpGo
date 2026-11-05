# GLPI query guide

## Start with the friendly tools

`glpi_projects` -> `glpi_project(id)` -> `glpi_tasks(project_id)` -> `glpi_task(id)`, plus `glpi_people(text)`.
They take names (state, type, manager, assignee), return one compact line per item with the id, and give
long text only as a size until you ask (`include`). Only drop to `glpi_search` / `glpi_get` / `glpi_sub_items`
below for things they do not cover (other item types, arbitrary columns).

## Generic tools

This server reads GLPI through its legacy REST API, read-only, as one service user.
Results only include what that user's profile and entities allow.

## Workflow

1. `glpi_session` — who the server is acting as (profile, entities, GLPI version). If a
   project you expect is missing, check this first: GLPI hides projects the user is not a
   team member of unless the profile has the right to see all projects.
2. `glpi_search_options(item_type)` — the columns of an item type. GLPI addresses columns by
   number; each also has a `uid` like `Project.name` and a display name.
3. `glpi_search(item_type, criteria, fields, ...)` — list/filter. Fields and criteria accept a
   number, a uid, or a unique display name. Result rows are keyed by uid, not number.
4. `glpi_get(item_type, id)` — full record of one item.
5. `glpi_sub_items(item_type, id, sub_item_type)` — children, e.g. Project 12 -> ProjectTask.

## Item types for project work

`Project`, `ProjectTask`, `ProjectState`, `ProjectType`, `ProjectTaskType`, `ProjectTaskTeam`,
`ProjectTeam`, `User`, `Group`, `Entity`. Any GLPI class name is accepted; the list is a hint.

## Criteria

JSON array. Each item is `{"field","searchtype","value","link"}` or `[field, searchtype, value, link?]`.

- `searchtype`: `contains` (default), `notcontains`, `equals`, `notequals`, `lessthan`, `morethan`, `under`,
  `notunder`, `empty`. Each column supports only some: `glpi_search_options` lists them per column and the
  server rejects an unsupported one (GLPI itself would silently treat it as `contains` and return wrong rows).
- **Numbers** (percent_done, ids, counts) support `contains`/`equals`/`empty` only. Compare by putting the
  operator in the value: `["percent_done","contains","<100"]`, `">=40"`.
- **Dates** support `lessthan`/`morethan`/`equals`: `["plan_start_date","morethan","2026-11-05"]`.
- `link`: `AND` (default), `OR`, `AND NOT`, `OR NOT`. Criteria are evaluated in order; the first link is ignored.
- Dropdown columns (state, manager, type...): `equals`/`notequals` accept the displayed name ("Processing") or the
  numeric id; the server rewrites a name to GLPI's exact-match form (`^Processing$`). `contains` is a partial match.
  (Raw GLPI compares dropdown `equals` to the id only, and silently ignores `notequals` with a name.)
- `empty`: the server sends the value GLPI needs; just pass `["Project.ProjectState.name","empty",""]`.
- `[]` or omitted = no filter.

Example: `[["Project.name","contains","kitchen"],["Project.ProjectState.name","equals",3,"AND"]]`

## Paging

`limit` (default 50, max 200) and `page` (1-based). The result has `total`, so loop pages
rather than asking for everything at once.

## Gotchas

- **Empty ProjectTask results while Projects work** is a GLPI profile quirk: if the profile has both
  project "See all" and "See (my)", GLPI lists tasks only for the user's own team. `glpi_session`
  flags this in `warnings`. The fix is in GLPI (service profile: "See all" only), not in the query.
- GLPI reuses a uid for several columns (Project "Father" also reports `Project.name`). Duplicates are
  labelled `Name (id)` in results; the plain uid always means the first column.
- Without `fields`, long text columns (descriptions, comments: often large HTML) are left out and named in
  `omitted_columns`. Ask for them in `fields`, or use `glpi_get`.
- Team columns such as `Project.ProjectTeam.User.name` may return user ids, not names. For names use
  `glpi_sub_items(Project, id, ProjectTeam)`, or the manager column `Project.User.name`.
- A task's state can differ between `glpi_search` ("New") and `glpi_get` (`projectstates_id: 0`): search shows
  GLPI's derived/default state, the record stores 0.
- `warnings` containing INCOMPLETE on ProjectTask results (and sub-items, which are then wrapped as
  `{"warnings":[...],"items":[...]}`) means the result is partial; see the first gotcha.
- A display name like `Name` is ambiguous when several tables expose it; use the uid or number.
- Dates are `YYYY-MM-DD HH:MM:SS` strings in the server's timezone.
- `glpi_get` with `expand_dropdowns=true` (default) returns names for state/type/user ids
  instead of bare integers.
- Percent done and other numeric project fields come back as numbers; empty values as null.
