package glpi

// Task-shaped, LLM-friendly reads built on Search/GetItem: names instead of
// ids, compact text instead of JSON, long text stripped and measured, and a
// "next:" hint for the follow-up call. No MCP imports.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// Formatting helpers
// ---------------------------------------------------------------------------

var (
	reDropBlocks = regexp.MustCompile(`(?is)<(script|style)[^>]*>.*?</(script|style)>`)
	reBreaks     = regexp.MustCompile(`(?i)<\s*(br|/p|/div|/li|/tr|/h[1-6])\s*/?>`)
	reTags       = regexp.MustCompile(`(?s)<[^>]*>`)
	reSpaces     = regexp.MustCompile(`[ \t\x{a0}]+`)
	reBlankLines = regexp.MustCompile(`\n\s*\n+`)
)

// StripHTML converts GLPI rich text to plain text. GLPI sometimes stores the
// markup entity-escaped, so it unescapes before and after stripping tags.
func StripHTML(s string) string {
	// Only treat the text as entity-escaped markup when it has no real tags;
	// otherwise a literal "<b>" a user typed (stored as &lt;b&gt;) would be eaten.
	if !strings.Contains(s, "<") && strings.Contains(s, "&lt;") {
		s = html.UnescapeString(s)
	}
	s = reDropBlocks.ReplaceAllString(s, "")
	s = reBreaks.ReplaceAllString(s, "\n")
	s = reTags.ReplaceAllString(s, "")
	s = html.UnescapeString(s)
	s = reSpaces.ReplaceAllString(s, " ")
	lines := strings.Split(s, "\n")
	for i := range lines {
		lines[i] = strings.TrimSpace(lines[i])
	}
	s = strings.Join(lines, "\n")
	s = reBlankLines.ReplaceAllString(s, "\n")
	return strings.TrimSpace(s)
}

// truncate cuts to n runes, reporting the full size when it cut.
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + fmt.Sprintf("… [%d chars total]", len(r))
}

func clip(s string, n int) string {
	r := []rune(strings.Join(strings.Fields(s), " "))
	if len(r) <= n {
		return string(r)
	}
	return string(r[:n-1]) + "…"
}

func asString(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case int:
		return strconv.Itoa(t)
	case []any:
		parts := make([]string, 0, len(t))
		for _, e := range t {
			parts = append(parts, asString(e))
		}
		return strings.Join(parts, ", ")
	}
	return fmt.Sprint(v)
}

func asInt(v any) int {
	switch t := v.(type) {
	case float64:
		return int(t)
	case int:
		return t
	case string:
		n, _ := strconv.Atoi(t)
		return n
	}
	return 0
}

func dateOnly(v any) string {
	s := asString(v)
	if len(s) >= 10 {
		return s[:10]
	}
	return s
}

var priorityNames = map[int]string{1: "very low", 2: "low", 3: "medium", 4: "high", 5: "very high", 6: "major"}

func pageInfo(start, returned, total int, tool string, page int) string {
	if total == 0 {
		return ""
	}
	end := start + returned
	s := fmt.Sprintf("showing %d-%d of %d", start+1, end, total)
	if end < total {
		s += fmt.Sprintf("; next: %s(page=%d)", tool, page+1)
	}
	return s
}

func pageStart(page, limit int) (start, lim, pg int) {
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	if page < 1 {
		page = 1
	}
	return (page - 1) * limit, limit, page
}

// ---------------------------------------------------------------------------
// Dropdown values (states, types) resolved by name
// ---------------------------------------------------------------------------

type ddItem struct {
	ID       int
	Name     string
	Finished bool
}

// ListItems lists items of a type via GET /:itemtype (raw records).
func (c *Client) ListItems(ctx context.Context, itemType string, start, limit int) ([]map[string]any, error) {
	if err := checkName("item type", itemType); err != nil {
		return nil, err
	}
	var q Query
	q.Add("get_hateoas", "false")
	q.Add("range", rangeParam(start, limit))
	body, err := c.get(ctx, "/"+itemType, q)
	if err != nil {
		return nil, err
	}
	var out []map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("%s list: unexpected response: %w", itemType, err)
	}
	return out, nil
}

func (c *Client) dropdown(ctx context.Context, itemType string) ([]ddItem, error) {
	c.optMu.Lock()
	if v, ok := c.ddCache[itemType]; ok {
		c.optMu.Unlock()
		return v, nil
	}
	c.optMu.Unlock()
	rows, err := c.ListItems(ctx, itemType, 0, 200)
	if err != nil {
		return nil, err
	}
	items := make([]ddItem, 0, len(rows))
	for _, r := range rows {
		items = append(items, ddItem{ID: asInt(r["id"]), Name: asString(r["name"]), Finished: asInt(r["is_finished"]) == 1})
	}
	c.optMu.Lock()
	if c.ddCache == nil {
		c.ddCache = map[string][]ddItem{}
	}
	c.ddCache[itemType] = items
	c.optMu.Unlock()
	return items, nil
}

// resolveDropdown matches a name (exact, then unique substring) to an item.
func (c *Client) resolveDropdown(ctx context.Context, itemType, kind, input string) (ddItem, error) {
	items, err := c.dropdown(ctx, itemType)
	if err != nil {
		return ddItem{}, err
	}
	in := strings.ToLower(strings.TrimSpace(input))
	var partial []ddItem
	for _, it := range items {
		n := strings.ToLower(it.Name)
		if n == in {
			return it, nil
		}
		if strings.Contains(n, in) {
			partial = append(partial, it)
		}
	}
	if len(partial) == 1 {
		return partial[0], nil
	}
	names := make([]string, len(items))
	for i, it := range items {
		names[i] = it.Name
	}
	return ddItem{}, fmt.Errorf("unknown %s %q; valid: %s", kind, input, strings.Join(names, ", "))
}

func (c *Client) finishedStates(ctx context.Context) ([]ddItem, error) {
	items, err := c.dropdown(ctx, "ProjectState")
	if err != nil {
		return nil, err
	}
	var out []ddItem
	for _, it := range items {
		if it.Finished {
			out = append(out, it)
		}
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// People
// ---------------------------------------------------------------------------

// Person is a GLPI user.
type Person struct {
	ID    int
	Login string
	Name  string
}

func (c *Client) findPeople(ctx context.Context, text string, limit int) ([]Person, int, error) {
	crit := []Criterion{
		{Field: "User.name", SearchType: "contains", Value: text, Link: "AND"},
		{Field: "User.realname", SearchType: "contains", Value: text, Link: "OR"},
		{Field: "User.firstname", SearchType: "contains", Value: text, Link: "OR"},
	}
	res, err := c.Search(ctx, "User", SearchParams{
		Criteria: crit, Fields: []string{"User.name", "User.firstname", "User.realname"},
		Sort: "User.name", Limit: limit,
	})
	if err != nil {
		return nil, 0, err
	}
	var out []Person
	for _, r := range res.Rows {
		out = append(out, Person{
			ID:    asInt(r["User.id"]),
			Login: asString(r["User.name"]),
			Name:  strings.TrimSpace(asString(r["User.firstname"]) + " " + asString(r["User.realname"])),
		})
	}
	return out, res.TotalCount, nil
}

// ResolveUser finds exactly one user from a login or name fragment.
func (c *Client) ResolveUser(ctx context.Context, text string) (Person, error) {
	people, _, err := c.findPeople(ctx, text, 10)
	if err != nil {
		return Person{}, fmt.Errorf("cannot look up user %q: %w", text, err)
	}
	for _, p := range people {
		if strings.EqualFold(p.Login, text) {
			return p, nil
		}
	}
	switch len(people) {
	case 0:
		return Person{}, fmt.Errorf("no user matches %q (try people(text))", text)
	case 1:
		return people[0], nil
	}
	lines := make([]string, len(people))
	for i, p := range people {
		lines[i] = fmt.Sprintf("%s (%s)", p.Login, p.Name)
	}
	return Person{}, fmt.Errorf("%q matches several users: %s; use the login", text, strings.Join(lines, ", "))
}

// People formats a user lookup.
func (c *Client) People(ctx context.Context, text string) (string, error) {
	if strings.TrimSpace(text) == "" {
		return "", errors.New("text is required")
	}
	people, total, err := c.findPeople(ctx, text, 25)
	if err != nil {
		return "", err
	}
	if len(people) == 0 {
		return fmt.Sprintf("No users match %q.", text), nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d user(s) match %q\n", total, text)
	for _, p := range people {
		fmt.Fprintf(&b, "#%d  %s  %s\n", p.ID, p.Login, p.Name)
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

// ---------------------------------------------------------------------------
// Projects
// ---------------------------------------------------------------------------

// ProjectFilter selects projects. Zero values mean "no filter".
type ProjectFilter struct {
	State, Type, Manager, Text string
	ParentID                   int
	Overdue                    bool
	Limit, Page                int
}

func today() string { return time.Now().Format("2006-01-02") }

func projectLine(r map[string]any) string {
	var b strings.Builder
	fmt.Fprintf(&b, "#%d  %s", asInt(r["Project.id"]), clip(asString(r["Project.name"]), 60))
	if s := asString(r["Project.ProjectState.name"]); s != "" {
		fmt.Fprintf(&b, " | %s", s)
	}
	fmt.Fprintf(&b, " | %d%%", asInt(r["Project.percent_done"]))
	if m := asString(r["Project.User.name"]); m != "" {
		fmt.Fprintf(&b, " | mgr %s", m)
	}
	if d := dateOnly(r["Project.plan_end_date"]); d != "" {
		fmt.Fprintf(&b, " | due %s", d)
	}
	return b.String()
}

var projectFields = []string{
	"Project.name", "Project.ProjectState.name", "Project.percent_done",
	"Project.User.name", "Project.plan_end_date",
}

func (c *Client) projectCriteria(ctx context.Context, f ProjectFilter) ([]Criterion, []string, error) {
	var crit []Criterion
	var applied []string
	add := func(field, st, val string) {
		crit = append(crit, Criterion{Field: field, SearchType: st, Value: val, Link: "AND"})
	}
	if f.State != "" {
		it, err := c.resolveDropdown(ctx, "ProjectState", "state", f.State)
		if err != nil {
			return nil, nil, err
		}
		add("Project.ProjectState.name", "equals", strconv.Itoa(it.ID))
		applied = append(applied, "state="+it.Name)
	}
	if f.Type != "" {
		it, err := c.resolveDropdown(ctx, "ProjectType", "type", f.Type)
		if err != nil {
			return nil, nil, err
		}
		add("Project.ProjectType.name", "equals", strconv.Itoa(it.ID))
		applied = append(applied, "type="+it.Name)
	}
	if f.Manager != "" {
		p, err := c.ResolveUser(ctx, f.Manager)
		if err != nil {
			return nil, nil, err
		}
		add("Project.User.name", "equals", strconv.Itoa(p.ID))
		applied = append(applied, "manager="+p.Login)
	}
	if f.Text != "" {
		add("Project.name", "contains", f.Text)
		applied = append(applied, fmt.Sprintf("name~%q", f.Text))
	}
	if f.ParentID > 0 {
		add("Father", "equals", strconv.Itoa(f.ParentID))
		applied = append(applied, fmt.Sprintf("parent=#%d", f.ParentID))
	}
	if f.Overdue {
		add("Project.plan_end_date", "lessthan", today())
		fin, err := c.finishedStates(ctx)
		if err != nil {
			return nil, nil, err
		}
		for _, it := range fin {
			add("Project.ProjectState.name", "notequals", strconv.Itoa(it.ID))
		}
		applied = append(applied, "overdue")
	}
	return crit, applied, nil
}

// Projects lists projects, one line each.
func (c *Client) Projects(ctx context.Context, f ProjectFilter) (string, error) {
	crit, applied, err := c.projectCriteria(ctx, f)
	if err != nil {
		return "", err
	}
	start, limit, page := pageStart(f.Page, f.Limit)
	res, err := c.Search(ctx, "Project", SearchParams{
		Criteria: crit, Fields: projectFields, Sort: "Project.id", Start: start, Limit: limit,
	})
	if err != nil {
		return "", err
	}
	var b strings.Builder
	head := fmt.Sprintf("%d project(s)", res.TotalCount)
	if len(applied) > 0 {
		head += " [" + strings.Join(applied, ", ") + "]"
	}
	b.WriteString(head + "\n")
	if res.TotalCount == 0 {
		b.WriteString("No projects match. Loosen the filters, or use people(text) to check a manager name.\n")
	}
	for _, r := range res.Rows {
		b.WriteString(projectLine(r) + "\n")
	}
	if pi := pageInfo(start, res.Returned, res.TotalCount, "projects", page); pi != "" {
		b.WriteString(pi + "\n")
	}
	b.WriteString("detail: project(id)")
	return b.String(), nil
}

func (c *Client) rawItem(ctx context.Context, itemType string, id int) (map[string]any, error) {
	raw, err := c.GetItem(ctx, itemType, id, true)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("%s %d: unexpected response: %w", itemType, id, err)
	}
	return m, nil
}

func has(list []string, s string) bool {
	for _, v := range list {
		if strings.EqualFold(v, s) {
			return true
		}
	}
	return false
}

// ProjectCard summarises one project. include: tasks, children, description, comment.
func (c *Client) ProjectCard(ctx context.Context, id int, include []string) (string, error) {
	m, err := c.rawItem(ctx, "Project", id)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Project #%d: %s\n", id, asString(m["name"]))
	if v := asString(m["code"]); v != "" {
		fmt.Fprintf(&b, "code: %s\n", v)
	}
	fmt.Fprintf(&b, "state: %s | type: %s | priority: %s | done: %d%%\n",
		orDash(asString(m["projectstates_id"])), orDash(asString(m["projecttypes_id"])),
		priorityNames[asInt(m["priority"])], asInt(m["percent_done"]))
	fmt.Fprintf(&b, "manager: %s | parent: %s\n", orDash(asString(m["users_id"])), parentName(m["projects_id"]))
	fmt.Fprintf(&b, "planned: %s → %s | real: %s → %s\n",
		orDash(dateOnly(m["plan_start_date"])), orDash(dateOnly(m["plan_end_date"])),
		orDash(dateOnly(m["real_start_date"])), orDash(dateOnly(m["real_end_date"])))
	fmt.Fprintf(&b, "updated: %s\n", dateOnly(m["date_mod"]))

	if team, err := c.teamNames(ctx, "Project", id); err == nil {
		fmt.Fprintf(&b, "team: %s\n", orDash(strings.Join(team, ", ")))
	}

	// tasks
	if rows, total, warns, err := c.projectTasks(ctx, id, 200); err == nil {
		open, overdue := c.taskCounts(ctx, rows)
		fmt.Fprintf(&b, "tasks: %d total, %d open, %d overdue", total, open, overdue)
		if total > len(rows) {
			fmt.Fprintf(&b, " (counts from first %d)", len(rows))
		}
		b.WriteString(" — tasks(project_id) to list\n")
		for _, w := range warns {
			fmt.Fprintf(&b, "⚠ %s\n", w)
		}
		if has(include, "tasks") {
			for _, r := range rows {
				b.WriteString("  " + taskLine(r, false) + "\n")
			}
		}
	}

	// children
	kids, err := c.Search(ctx, "Project", SearchParams{
		Criteria: []Criterion{{Field: "Father", SearchType: "equals", Value: strconv.Itoa(id), Link: "AND"}},
		Fields:   projectFields, Sort: "Project.id", Limit: 30,
	})
	if err == nil {
		fmt.Fprintf(&b, "sub-projects: %d — projects(parent_id=%d) to list\n", kids.TotalCount, id)
		if has(include, "children") {
			for _, r := range kids.Rows {
				b.WriteString("  " + projectLine(r) + "\n")
			}
		}
	}

	b.WriteString(textBlock("description", asString(m["content"]), has(include, "description")))
	b.WriteString(textBlock("comment", asString(m["comment"]), has(include, "comment")))
	return strings.TrimRight(b.String(), "\n"), nil
}

func orDash(s string) string {
	if s == "" || s == "0" {
		return "-"
	}
	return s
}

func parentName(v any) string {
	s := asString(v)
	if s == "" || s == "0" {
		return "-"
	}
	return s
}

// textBlock prints rich text: a size hint by default, the text itself on request.
func textBlock(label, raw string, full bool) string {
	text := StripHTML(raw)
	if text == "" {
		return ""
	}
	if !full {
		return fmt.Sprintf("%s: %d chars (include=%s to read)\n", label, len([]rune(text)), label)
	}
	return fmt.Sprintf("%s:\n%s\n", label, truncate(text, 3000))
}

func (c *Client) teamNames(ctx context.Context, itemType string, id int) ([]string, error) {
	sub := itemType + "Team"
	raw, err := c.SubItems(ctx, itemType, id, sub, true, 0, 50)
	if err != nil {
		return nil, err
	}
	var items []map[string]any
	if err := json.Unmarshal(raw, &items); err != nil {
		// Task sub-items may be wrapped with warnings.
		var wrap struct {
			Items []map[string]any `json:"items"`
		}
		if json.Unmarshal(raw, &wrap) != nil {
			return nil, err
		}
		items = wrap.Items
	}
	var names []string
	for _, it := range items {
		n := asString(it["items_id"])
		if t := asString(it["itemtype"]); t != "" && t != "User" {
			n += " (" + t + ")"
		}
		names = append(names, n)
	}
	return names, nil
}

// ---------------------------------------------------------------------------
// Tasks
// ---------------------------------------------------------------------------

// TaskFilter selects project tasks. Zero values mean "no filter".
type TaskFilter struct {
	ProjectID   int
	State       string
	Assignee    string
	DueBefore   string // YYYY-MM-DD
	OpenOnly    bool
	Overdue     bool
	Limit, Page int
}

var taskFields = []string{
	"ProjectTask.name", "ProjectTask.Project.name", "ProjectTask.ProjectState.name",
	"ProjectTask.percent_done", "ProjectTask.plan_end_date",
}

func taskLine(r map[string]any, withProject bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "#%d  %s", asInt(r["id"]), clip(asString(r["ProjectTask.name"]), 60))
	if withProject {
		fmt.Fprintf(&b, " | in %s", clip(asString(r["ProjectTask.Project.name"]), 40))
	}
	if s := asString(r["ProjectTask.ProjectState.name"]); s != "" {
		fmt.Fprintf(&b, " | %s", s)
	}
	fmt.Fprintf(&b, " | %d%%", asInt(r["ProjectTask.percent_done"]))
	if d := dateOnly(r["ProjectTask.plan_end_date"]); d != "" {
		fmt.Fprintf(&b, " | due %s", d)
	}
	return b.String()
}

func (c *Client) taskCrit(ctx context.Context, f TaskFilter) ([]Criterion, []string, error) {
	var crit []Criterion
	var applied []string
	add := func(field, st, val string) {
		crit = append(crit, Criterion{Field: field, SearchType: st, Value: val, Link: "AND"})
	}
	if f.ProjectID > 0 {
		add("ProjectTask.Project.name", "equals", strconv.Itoa(f.ProjectID))
		applied = append(applied, fmt.Sprintf("project=#%d", f.ProjectID))
	}
	if f.State != "" {
		it, err := c.resolveDropdown(ctx, "ProjectState", "state", f.State)
		if err != nil {
			return nil, nil, err
		}
		add("ProjectTask.ProjectState.name", "equals", strconv.Itoa(it.ID))
		applied = append(applied, "state="+it.Name)
	}
	if f.Assignee != "" {
		p, err := c.ResolveUser(ctx, f.Assignee)
		if err != nil {
			return nil, nil, err
		}
		add("ProjectTask.ProjectTaskTeam.User.name", "contains", p.Login)
		applied = append(applied, "assignee="+p.Login)
	}
	if f.DueBefore != "" {
		add("ProjectTask.plan_end_date", "lessthan", f.DueBefore)
		applied = append(applied, "due<"+f.DueBefore)
	}
	if f.OpenOnly || f.Overdue {
		add("ProjectTask.percent_done", "contains", "<100")
		applied = append(applied, "open")
	}
	if f.Overdue {
		add("ProjectTask.plan_end_date", "lessthan", today())
		applied = append(applied, "overdue")
	}
	return crit, applied, nil
}

func (c *Client) projectTasks(ctx context.Context, projectID, limit int) ([]map[string]any, int, []string, error) {
	crit, _, err := c.taskCrit(ctx, TaskFilter{ProjectID: projectID})
	if err != nil {
		return nil, 0, nil, err
	}
	res, err := c.Search(ctx, "ProjectTask", SearchParams{Criteria: crit, Fields: taskFields, Limit: limit})
	if err != nil {
		return nil, 0, nil, err
	}
	return res.Rows, res.TotalCount, res.Warnings, nil
}

// taskCounts returns (open, overdue) for rows; open = not 100% and not in a finished state.
func (c *Client) taskCounts(ctx context.Context, rows []map[string]any) (open, overdue int) {
	fin, _ := c.finishedStates(ctx)
	finished := map[string]bool{}
	for _, f := range fin {
		finished[strings.ToLower(f.Name)] = true
	}
	now := today()
	for _, r := range rows {
		if asInt(r["ProjectTask.percent_done"]) >= 100 || finished[strings.ToLower(asString(r["ProjectTask.ProjectState.name"]))] {
			continue
		}
		open++
		if d := dateOnly(r["ProjectTask.plan_end_date"]); d != "" && d < now {
			overdue++
		}
	}
	return open, overdue
}

// Tasks lists project tasks, one line each.
func (c *Client) Tasks(ctx context.Context, f TaskFilter) (string, error) {
	crit, applied, err := c.taskCrit(ctx, f)
	if err != nil {
		return "", err
	}
	start, limit, page := pageStart(f.Page, f.Limit)
	res, err := c.Search(ctx, "ProjectTask", SearchParams{
		Criteria: crit, Fields: taskFields, Sort: "ProjectTask.plan_end_date", Start: start, Limit: limit,
	})
	if err != nil {
		return "", err
	}
	var b strings.Builder
	head := fmt.Sprintf("%d task(s)", res.TotalCount)
	if len(applied) > 0 {
		head += " [" + strings.Join(applied, ", ") + "]"
	}
	b.WriteString(head + "\n")
	for _, w := range res.Warnings {
		fmt.Fprintf(&b, "⚠ %s\n", w)
	}
	if res.TotalCount == 0 {
		b.WriteString("No tasks match.\n")
	}
	for _, r := range res.Rows {
		b.WriteString(taskLine(r, f.ProjectID == 0) + "\n")
	}
	if pi := pageInfo(start, res.Returned, res.TotalCount, "tasks", page); pi != "" {
		b.WriteString(pi + "\n")
	}
	b.WriteString("detail: task(id)")
	return b.String(), nil
}

// TaskCard summarises one task. include: description, comment.
func (c *Client) TaskCard(ctx context.Context, id int, include []string) (string, error) {
	m, err := c.rawItem(ctx, "ProjectTask", id)
	if err != nil {
		return "", err
	}
	state := orDash(asString(m["projectstates_id"]))
	if asInt(m["auto_projectstates"]) == 1 {
		state += " (auto: derived from percent done; lists show the effective state)"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Task #%d: %s\n", id, asString(m["name"]))
	fmt.Fprintf(&b, "project: %s", asString(m["projects_id"]))
	if asInt(m["projecttasks_id"]) != 0 {
		fmt.Fprintf(&b, " | parent task: #%d", asInt(m["projecttasks_id"]))
	}
	fmt.Fprintf(&b, "\nstate: %s | done: %d%% | milestone: %t\n", state, asInt(m["percent_done"]), asInt(m["is_milestone"]) == 1)
	fmt.Fprintf(&b, "planned: %s → %s | real: %s → %s\n",
		orDash(dateOnly(m["plan_start_date"])), orDash(dateOnly(m["plan_end_date"])),
		orDash(dateOnly(m["real_start_date"])), orDash(dateOnly(m["real_end_date"])))
	fmt.Fprintf(&b, "creator: %s | updated: %s\n", orDash(asString(m["users_id"])), dateOnly(m["date_mod"]))
	if team, err := c.teamNames(ctx, "ProjectTask", id); err == nil {
		fmt.Fprintf(&b, "assigned: %s\n", orDash(strings.Join(team, ", ")))
	}
	b.WriteString(textBlock("description", asString(m["content"]), has(include, "description")))
	b.WriteString(textBlock("comment", asString(m["comment"]), has(include, "comment")))
	return strings.TrimRight(b.String(), "\n"), nil
}
