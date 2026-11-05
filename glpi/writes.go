package glpi

// The write side of the friendly layer. Every operation is a narrow, typed
// function: it resolves names, validates, builds a plan (before -> after), and
// by default only PREVIEWS it. Nothing is sent unless Apply is true, and the
// client refuses writes entirely unless Config.AllowWrite is set.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"log"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// Plan: the set of changes an operation would make
// ---------------------------------------------------------------------------

type change struct{ label, from, to string }

type plan struct {
	cur     map[string]any // expanded current record; nil when creating
	payload map[string]any
	changes []change
}

func newPlan(cur map[string]any) *plan {
	return &plan{cur: cur, payload: map[string]any{}}
}

func (p *plan) set(field, label string, val any, from, to string) {
	p.payload[field] = val
	p.changes = append(p.changes, change{label, from, to})
}

func (p *plan) curStr(field string) string {
	if p.cur == nil {
		return ""
	}
	return asString(p.cur[field])
}

func shown(s string) string {
	if s == "" || s == "0" {
		return "(none)"
	}
	return s
}

func (p *plan) name(v string) error {
	v = strings.TrimSpace(v)
	if v == "" {
		return errors.New("name must not be empty")
	}
	if len([]rune(v)) > 255 {
		return errors.New("name is longer than 255 characters")
	}
	if p.cur == nil || p.curStr("name") != v {
		p.set("name", "name", v, shown(p.curStr("name")), v)
	}
	return nil
}

var reParaSplit = regexp.MustCompile(`\n\s*\n`)

// richText turns plain text into the HTML GLPI stores for descriptions.
func richText(plain string) string {
	var b strings.Builder
	for _, para := range reParaSplit.Split(strings.TrimSpace(plain), -1) {
		lines := strings.Split(para, "\n")
		for i := range lines {
			lines[i] = html.EscapeString(strings.TrimSpace(lines[i]))
		}
		fmt.Fprintf(&b, "<p>%s</p>", strings.Join(lines, "<br>"))
	}
	return b.String()
}

func (p *plan) description(plain string) {
	plain = strings.TrimSpace(plain)
	old := StripHTML(p.curStr("content"))
	if p.cur != nil && old == plain {
		return
	}
	p.set("content", "description", richText(plain), fmt.Sprintf("%d chars", len([]rune(old))), fmt.Sprintf("%d chars: %s", len([]rune(plain)), clip(plain, 80)))
}

func (p *plan) percent(v int, autoField string) error {
	if v < 0 || v > 100 {
		return fmt.Errorf("percent_done must be 0-100, got %d", v)
	}
	if p.cur != nil && asInt(p.cur["percent_done"]) == v {
		return nil
	}
	p.set("percent_done", "percent done", v, fmt.Sprintf("%d", asInt(p.cur["percent_done"])), fmt.Sprintf("%d", v))
	if p.cur != nil && asInt(p.cur[autoField]) == 1 {
		p.set(autoField, "auto percent", 0, "on", "off (needed so a manual percent sticks)")
	}
	return nil
}

func parseWhen(label, s string) (string, error) {
	s = strings.TrimSpace(s)
	for _, layout := range []string{"2006-01-02", "2006-01-02 15:04", "2006-01-02 15:04:05"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.Format("2006-01-02 15:04:05"), nil
		}
	}
	return "", fmt.Errorf("%s must be YYYY-MM-DD (optionally with HH:MM), got %q", label, s)
}

func (p *plan) date(field, label, v string) error {
	when, err := parseWhen(label, v)
	if err != nil {
		return err
	}
	old := p.curStr(field)
	if old == when || (len(v) == 10 && dateOnly(old) == v) {
		return nil
	}
	p.set(field, label, when, shown(dateOnly(old)), dateOnly(when))
	return nil
}

// checkOrder rejects an end date before the start, using effective values.
func (p *plan) checkOrder() error {
	eff := func(f string) string {
		if v, ok := p.payload[f].(string); ok {
			return v
		}
		return p.curStr(f)
	}
	s, e := eff("plan_start_date"), eff("plan_end_date")
	if s != "" && e != "" && e < s {
		return fmt.Errorf("planned end %s is before planned start %s", dateOnly(e), dateOnly(s))
	}
	return nil
}

func (p *plan) preview(headline, tool string, notes []string) string {
	var b strings.Builder
	b.WriteString("PREVIEW — nothing has been changed\n")
	b.WriteString(headline + "\n")
	for _, ch := range p.changes {
		if p.cur == nil {
			fmt.Fprintf(&b, "  %s: %s\n", ch.label, ch.to)
		} else {
			fmt.Fprintf(&b, "  %s: %s → %s\n", ch.label, ch.from, ch.to)
		}
	}
	for _, n := range notes {
		fmt.Fprintf(&b, "  note: %s\n", n)
	}
	fmt.Fprintf(&b, "To apply, call %s again with the same arguments and apply=true.", tool)
	return b.String()
}

func audit(op string, detail any) {
	b, _ := json.Marshal(map[string]any{"op": op, "detail": detail, "at": time.Now().UTC().Format(time.RFC3339)})
	log.Printf("AUDIT %s", b)
}

func changesForAudit(chs []change) []string {
	out := make([]string, len(chs))
	for i, c := range chs {
		out[i] = fmt.Sprintf("%s: %s -> %s", c.label, c.from, c.to)
	}
	return out
}

// checkUpdateResult validates GLPI's PUT response: [{"<id>": true, "message": ""}].
func checkUpdateResult(body []byte, id int) error {
	var res []map[string]any
	if err := json.Unmarshal(body, &res); err != nil || len(res) == 0 {
		return fmt.Errorf("unexpected response from GLPI: %.200s", body)
	}
	for _, m := range res {
		if ok, isBool := m[strconv.Itoa(id)].(bool); isBool && !ok {
			return fmt.Errorf("GLPI rejected the update: %s", asString(m["message"]))
		}
	}
	return nil
}

// verify re-reads the record and reports any written field GLPI did not keep
// as sent (it silently overrides some, e.g. derived state or percent).
func (c *Client) verify(ctx context.Context, itemType string, id int, payload map[string]any) []string {
	raw, err := c.GetItem(ctx, itemType, id, false)
	if err != nil {
		return []string{"could not re-read the record to verify: " + err.Error()}
	}
	var got map[string]any
	if json.Unmarshal(raw, &got) != nil {
		return nil
	}
	var diffs []string
	for k, want := range payload {
		if k == "content" {
			continue // GLPI normalises HTML
		}
		if asString(got[k]) != asString(want) {
			diffs = append(diffs, fmt.Sprintf("%s: sent %s, GLPI stored %s", k, asString(want), asString(got[k])))
		}
	}
	return diffs
}

func appliedFooter(diffs []string) string {
	if len(diffs) == 0 {
		return ""
	}
	return "\n⚠ GLPI stored different values than sent:\n  " + strings.Join(diffs, "\n  ")
}

func (c *Client) requireWrite() error {
	if !c.cfg.AllowWrite {
		return errors.New("writes are disabled (GLPI_ALLOW_WRITE is not set)")
	}
	return nil
}

// ---------------------------------------------------------------------------
// update_task
// ---------------------------------------------------------------------------

// TaskUpdate changes selected fields of one task. Nil fields are left alone.
type TaskUpdate struct {
	ID           int
	Name         *string
	Description  *string
	State        *string
	PercentDone  *int
	PlannedStart *string
	PlannedEnd   *string
	Apply        bool
}

// UpdateTask previews or applies a task update.
func (c *Client) UpdateTask(ctx context.Context, u TaskUpdate) (string, error) {
	if err := c.requireWrite(); err != nil {
		return "", err
	}
	cur, err := c.rawItem(ctx, "ProjectTask", u.ID)
	if err != nil {
		return "", err
	}
	p := newPlan(cur)
	if u.Name != nil {
		if err := p.name(*u.Name); err != nil {
			return "", err
		}
	}
	if u.Description != nil {
		p.description(*u.Description)
	}
	if u.State != nil {
		it, err := c.resolveDropdown(ctx, "ProjectState", "state", *u.State)
		if err != nil {
			return "", err
		}
		if !strings.EqualFold(p.curStr("projectstates_id"), it.Name) {
			p.set("projectstates_id", "state", it.ID, shown(p.curStr("projectstates_id")), it.Name)
			if asInt(cur["auto_projectstates"]) == 1 {
				p.set("auto_projectstates", "auto state", 0, "on", "off (needed so a manual state sticks)")
			}
		}
	}
	if u.PercentDone != nil {
		if err := p.percent(*u.PercentDone, "auto_percent_done"); err != nil {
			return "", err
		}
	}
	if u.PlannedStart != nil {
		if err := p.date("plan_start_date", "planned start", *u.PlannedStart); err != nil {
			return "", err
		}
	}
	if u.PlannedEnd != nil {
		if err := p.date("plan_end_date", "planned end", *u.PlannedEnd); err != nil {
			return "", err
		}
	}
	if err := p.checkOrder(); err != nil {
		return "", err
	}
	if len(p.changes) == 0 {
		return "", errors.New("nothing to change: every given value already matches the task")
	}
	headline := fmt.Sprintf("update task #%d %q", u.ID, asString(cur["name"]))
	if !u.Apply {
		return p.preview(headline, "glpi_update_task", nil), nil
	}
	body, err := c.send(ctx, "PUT", fmt.Sprintf("/ProjectTask/%d", u.ID), p.payload)
	if err != nil {
		return "", err
	}
	if err := checkUpdateResult(body, u.ID); err != nil {
		return "", err
	}
	audit("update_task", map[string]any{"id": u.ID, "changes": changesForAudit(p.changes)})
	diffs := c.verify(ctx, "ProjectTask", u.ID, p.payload)
	card, _ := c.TaskCard(ctx, u.ID, nil)
	return "APPLIED — " + headline + "\n" + card + appliedFooter(diffs), nil
}

// ---------------------------------------------------------------------------
// update_project
// ---------------------------------------------------------------------------

// ProjectUpdate changes selected fields of one project. Nil fields are left alone.
type ProjectUpdate struct {
	ID           int
	Name         *string
	Description  *string
	State        *string
	PercentDone  *int
	PlannedStart *string
	PlannedEnd   *string
	Manager      *string
	Apply        bool
}

// UpdateProject previews or applies a project update.
func (c *Client) UpdateProject(ctx context.Context, u ProjectUpdate) (string, error) {
	if err := c.requireWrite(); err != nil {
		return "", err
	}
	cur, err := c.rawItem(ctx, "Project", u.ID)
	if err != nil {
		return "", err
	}
	p := newPlan(cur)
	if u.Name != nil {
		if err := p.name(*u.Name); err != nil {
			return "", err
		}
	}
	if u.Description != nil {
		p.description(*u.Description)
	}
	if u.State != nil {
		it, err := c.resolveDropdown(ctx, "ProjectState", "state", *u.State)
		if err != nil {
			return "", err
		}
		if !strings.EqualFold(p.curStr("projectstates_id"), it.Name) {
			p.set("projectstates_id", "state", it.ID, shown(p.curStr("projectstates_id")), it.Name)
		}
	}
	if u.Manager != nil {
		who, err := c.ResolveUser(ctx, *u.Manager)
		if err != nil {
			return "", err
		}
		if !strings.EqualFold(p.curStr("users_id"), who.Login) {
			p.set("users_id", "manager", who.ID, shown(p.curStr("users_id")), who.Login)
		}
	}
	if u.PercentDone != nil {
		if err := p.percent(*u.PercentDone, "auto_percent_done"); err != nil {
			return "", err
		}
	}
	if u.PlannedStart != nil {
		if err := p.date("plan_start_date", "planned start", *u.PlannedStart); err != nil {
			return "", err
		}
	}
	if u.PlannedEnd != nil {
		if err := p.date("plan_end_date", "planned end", *u.PlannedEnd); err != nil {
			return "", err
		}
	}
	if err := p.checkOrder(); err != nil {
		return "", err
	}
	if len(p.changes) == 0 {
		return "", errors.New("nothing to change: every given value already matches the project")
	}
	headline := fmt.Sprintf("update project #%d %q", u.ID, asString(cur["name"]))
	if !u.Apply {
		return p.preview(headline, "glpi_update_project", nil), nil
	}
	body, err := c.send(ctx, "PUT", fmt.Sprintf("/Project/%d", u.ID), p.payload)
	if err != nil {
		return "", err
	}
	if err := checkUpdateResult(body, u.ID); err != nil {
		return "", err
	}
	audit("update_project", map[string]any{"id": u.ID, "changes": changesForAudit(p.changes)})
	diffs := c.verify(ctx, "Project", u.ID, p.payload)
	card, _ := c.ProjectCard(ctx, u.ID, nil)
	return "APPLIED — " + headline + "\n" + card + appliedFooter(diffs), nil
}

// ---------------------------------------------------------------------------
// create_project / create_task
// ---------------------------------------------------------------------------

func (c *Client) rawIDs(ctx context.Context, itemType string, id int) (map[string]any, error) {
	raw, err := c.GetItem(ctx, itemType, id, false)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("%s %d: unexpected response", itemType, id)
	}
	return m, nil
}

// duplicateIDs returns ids of existing items with exactly this name.
func (c *Client) duplicateIDs(ctx context.Context, itemType, nameUID string, extra []Criterion, name string) ([]int, error) {
	crit := append([]Criterion{{Field: nameUID, SearchType: "contains", Value: "^" + name + "$", Link: "AND"}}, extra...)
	res, err := c.Search(ctx, itemType, SearchParams{Criteria: crit, Fields: []string{nameUID}, Limit: 5})
	if err != nil {
		return nil, err
	}
	var ids []int
	for _, r := range res.Rows {
		ids = append(ids, asInt(r["id"]), asInt(r[itemType+".id"]))
	}
	var out []int
	for _, id := range ids {
		if id > 0 {
			out = append(out, id)
		}
	}
	return out, nil
}

func parseCreatedID(body []byte) (int, error) {
	var r struct {
		ID      any    `json:"id"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return 0, fmt.Errorf("unexpected response from GLPI: %.200s", body)
	}
	id := asInt(r.ID)
	if id <= 0 {
		return 0, fmt.Errorf("GLPI did not create the item: %s", r.Message)
	}
	return id, nil
}

// ProjectCreate describes a new project.
type ProjectCreate struct {
	Name, Description, Code, State, Type, Manager, PlannedStart, PlannedEnd string
	ParentID                                                                int
	Apply                                                                   bool
}

// CreateProject previews or applies project creation.
func (c *Client) CreateProject(ctx context.Context, u ProjectCreate) (string, error) {
	if err := c.requireWrite(); err != nil {
		return "", err
	}
	p := newPlan(nil)
	if err := p.name(u.Name); err != nil {
		return "", err
	}
	name := strings.TrimSpace(u.Name)
	if dups, err := c.duplicateIDs(ctx, "Project", "Project.name", nil, name); err != nil {
		return "", fmt.Errorf("duplicate check failed: %w", err)
	} else if len(dups) > 0 {
		return "", fmt.Errorf("a project named %q already exists (#%d); use glpi_update_project, or choose another name", name, dups[0])
	}
	if u.Description != "" {
		p.description(u.Description)
	}
	if u.Code != "" {
		p.set("code", "code", strings.TrimSpace(u.Code), "", strings.TrimSpace(u.Code))
	}
	if u.ParentID > 0 {
		parent, err := c.rawIDs(ctx, "Project", u.ParentID)
		if err != nil {
			return "", fmt.Errorf("parent project #%d: %w", u.ParentID, err)
		}
		pe, _ := c.rawItem(ctx, "Project", u.ParentID)
		p.set("projects_id", "parent", u.ParentID, "", fmt.Sprintf("#%d %s", u.ParentID, asString(pe["name"])))
		p.payload["entities_id"] = parent["entities_id"]
	}
	if u.State != "" {
		it, err := c.resolveDropdown(ctx, "ProjectState", "state", u.State)
		if err != nil {
			return "", err
		}
		p.set("projectstates_id", "state", it.ID, "", it.Name)
	}
	if u.Type != "" {
		it, err := c.resolveDropdown(ctx, "ProjectType", "type", u.Type)
		if err != nil {
			return "", err
		}
		p.set("projecttypes_id", "type", it.ID, "", it.Name)
	}
	if u.Manager != "" {
		who, err := c.ResolveUser(ctx, u.Manager)
		if err != nil {
			return "", err
		}
		p.set("users_id", "manager", who.ID, "", who.Login)
	}
	if u.PlannedStart != "" {
		if err := p.date("plan_start_date", "planned start", u.PlannedStart); err != nil {
			return "", err
		}
	}
	if u.PlannedEnd != "" {
		if err := p.date("plan_end_date", "planned end", u.PlannedEnd); err != nil {
			return "", err
		}
	}
	if err := p.checkOrder(); err != nil {
		return "", err
	}
	headline := fmt.Sprintf("create project %q", name)
	if !u.Apply {
		return p.preview(headline, "glpi_create_project", nil), nil
	}
	body, err := c.send(ctx, "POST", "/Project", p.payload)
	if err != nil {
		return "", err
	}
	id, err := parseCreatedID(body)
	if err != nil {
		return "", err
	}
	audit("create_project", map[string]any{"id": id, "changes": changesForAudit(p.changes)})
	diffs := c.verify(ctx, "Project", id, p.payload)
	card, _ := c.ProjectCard(ctx, id, nil)
	return fmt.Sprintf("APPLIED — created project #%d\n%s%s", id, card, appliedFooter(diffs)), nil
}

// TaskCreate describes a new task.
type TaskCreate struct {
	ProjectID                                                    int
	Name, Description, State, Assignee, PlannedStart, PlannedEnd string
	ParentTaskID                                                 int
	Apply                                                        bool
}

// CreateTask previews or applies task creation (and optional assignment).
func (c *Client) CreateTask(ctx context.Context, u TaskCreate) (string, error) {
	if err := c.requireWrite(); err != nil {
		return "", err
	}
	if u.ProjectID <= 0 {
		return "", errors.New("project_id is required")
	}
	proj, err := c.rawIDs(ctx, "Project", u.ProjectID)
	if err != nil {
		return "", fmt.Errorf("project #%d: %w", u.ProjectID, err)
	}
	projExp, _ := c.rawItem(ctx, "Project", u.ProjectID)
	p := newPlan(nil)
	if err := p.name(u.Name); err != nil {
		return "", err
	}
	name := strings.TrimSpace(u.Name)
	if dups, err := c.duplicateIDs(ctx, "ProjectTask", "ProjectTask.name",
		[]Criterion{{Field: "ProjectTask.Project.name", SearchType: "equals", Value: strconv.Itoa(u.ProjectID), Link: "AND"}}, name); err != nil {
		return "", fmt.Errorf("duplicate check failed: %w", err)
	} else if len(dups) > 0 {
		return "", fmt.Errorf("project #%d already has a task named %q (#%d); use glpi_update_task, or choose another name", u.ProjectID, name, dups[0])
	}
	p.set("projects_id", "project", u.ProjectID, "", fmt.Sprintf("#%d %s", u.ProjectID, asString(projExp["name"])))
	p.payload["entities_id"] = proj["entities_id"]
	if u.ParentTaskID > 0 {
		parent, err := c.rawIDs(ctx, "ProjectTask", u.ParentTaskID)
		if err != nil {
			return "", fmt.Errorf("parent task #%d: %w", u.ParentTaskID, err)
		}
		if asInt(parent["projects_id"]) != u.ProjectID {
			return "", fmt.Errorf("parent task #%d belongs to project #%d, not #%d", u.ParentTaskID, asInt(parent["projects_id"]), u.ProjectID)
		}
		p.set("projecttasks_id", "parent task", u.ParentTaskID, "", fmt.Sprintf("#%d %s", u.ParentTaskID, asString(parent["name"])))
	}
	if u.Description != "" {
		p.description(u.Description)
	}
	if u.State != "" {
		it, err := c.resolveDropdown(ctx, "ProjectState", "state", u.State)
		if err != nil {
			return "", err
		}
		p.set("projectstates_id", "state", it.ID, "", it.Name)
		p.set("auto_projectstates", "auto state", 0, "", "off (so the chosen state sticks)")
	}
	var who Person
	if u.Assignee != "" {
		if who, err = c.ResolveUser(ctx, u.Assignee); err != nil {
			return "", err
		}
		p.changes = append(p.changes, change{"assignee", "", who.Login})
	}
	if u.PlannedStart != "" {
		if err := p.date("plan_start_date", "planned start", u.PlannedStart); err != nil {
			return "", err
		}
	}
	if u.PlannedEnd != "" {
		if err := p.date("plan_end_date", "planned end", u.PlannedEnd); err != nil {
			return "", err
		}
	}
	if err := p.checkOrder(); err != nil {
		return "", err
	}
	headline := fmt.Sprintf("create task %q in project #%d", name, u.ProjectID)
	if !u.Apply {
		return p.preview(headline, "glpi_create_task", nil), nil
	}
	body, err := c.send(ctx, "POST", "/ProjectTask", p.payload)
	if err != nil {
		return "", err
	}
	id, err := parseCreatedID(body)
	if err != nil {
		return "", err
	}
	audit("create_task", map[string]any{"id": id, "project": u.ProjectID, "changes": changesForAudit(p.changes)})
	var tail string
	if u.Assignee != "" {
		_, err := c.send(ctx, "POST", "/ProjectTaskTeam", map[string]any{"projecttasks_id": id, "itemtype": "User", "items_id": who.ID})
		if err != nil {
			tail = fmt.Sprintf("\n⚠ the task was created but assigning %s failed: %v — use glpi_assign_task(task_id=%d)", who.Login, err, id)
		} else {
			audit("assign_task", map[string]any{"task": id, "user": who.Login, "action": "add"})
		}
	}
	diffs := c.verify(ctx, "ProjectTask", id, p.payload)
	card, _ := c.TaskCard(ctx, id, nil)
	return fmt.Sprintf("APPLIED — created task #%d\n%s%s%s", id, card, appliedFooter(diffs), tail), nil
}

// ---------------------------------------------------------------------------
// assign_task
// ---------------------------------------------------------------------------

func parseItemList(raw []byte) ([]map[string]any, error) {
	var items []map[string]any
	if err := json.Unmarshal(raw, &items); err == nil {
		return items, nil
	}
	var wrap struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(raw, &wrap); err != nil {
		return nil, errors.New("unexpected sub-item response")
	}
	return wrap.Items, nil
}

// TaskAssign adds or removes one person on a task's team.
type TaskAssign struct {
	TaskID   int
	Assignee string
	Action   string // "add" (default) or "remove"
	Apply    bool
}

// AssignTask previews or applies a team change.
func (c *Client) AssignTask(ctx context.Context, u TaskAssign) (string, error) {
	if err := c.requireWrite(); err != nil {
		return "", err
	}
	action := strings.ToLower(strings.TrimSpace(u.Action))
	if action == "" {
		action = "add"
	}
	if action != "add" && action != "remove" {
		return "", fmt.Errorf("action must be add or remove, got %q", u.Action)
	}
	task, err := c.rawItem(ctx, "ProjectTask", u.TaskID)
	if err != nil {
		return "", err
	}
	who, err := c.ResolveUser(ctx, u.Assignee)
	if err != nil {
		return "", err
	}
	raw, err := c.SubItems(ctx, "ProjectTask", u.TaskID, "ProjectTaskTeam", false, 0, 100)
	if err != nil {
		return "", fmt.Errorf("reading task team: %w", err)
	}
	rows, err := parseItemList(raw)
	if err != nil {
		return "", err
	}
	rowID := 0
	for _, r := range rows {
		if asString(r["itemtype"]) == "User" && asInt(r["items_id"]) == who.ID {
			rowID = asInt(r["id"])
		}
	}
	before, _ := c.teamNames(ctx, "ProjectTask", u.TaskID)
	var after []string
	switch action {
	case "add":
		if rowID != 0 {
			return "", fmt.Errorf("%s is already on task #%d's team", who.Login, u.TaskID)
		}
		after = append(append([]string{}, before...), who.Login)
	case "remove":
		if rowID == 0 {
			return "", fmt.Errorf("%s is not on task #%d's team (team: %s)", who.Login, u.TaskID, orDash(strings.Join(before, ", ")))
		}
		for _, n := range before {
			if !strings.EqualFold(n, who.Login) {
				after = append(after, n)
			}
		}
	}
	headline := fmt.Sprintf("%s %s on task #%d %q", action, who.Login, u.TaskID, asString(task["name"]))
	if !u.Apply {
		return fmt.Sprintf("PREVIEW — nothing has been changed\n%s\n  team: %s → %s\nTo apply, call glpi_assign_task again with the same arguments and apply=true.",
			headline, orDash(strings.Join(before, ", ")), orDash(strings.Join(after, ", "))), nil
	}
	if action == "add" {
		_, err = c.send(ctx, "POST", "/ProjectTaskTeam", map[string]any{"projecttasks_id": u.TaskID, "itemtype": "User", "items_id": who.ID})
	} else {
		_, err = c.send(ctx, "DELETE", fmt.Sprintf("/ProjectTaskTeam/%d", rowID), nil)
	}
	if err != nil {
		return "", err
	}
	audit("assign_task", map[string]any{"task": u.TaskID, "user": who.Login, "action": action})
	now, _ := c.teamNames(ctx, "ProjectTask", u.TaskID)
	return fmt.Sprintf("APPLIED — %s\n  team now: %s", headline, orDash(strings.Join(now, ", "))), nil
}
