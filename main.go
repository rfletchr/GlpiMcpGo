package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	minimcp "github.com/rfletchr/MiniMCP"

	"github.com/rfletchr/GlpiMcpGo/glpi"
)

//go:embed prompts/glpi_guide.md
var guide string

// ---------------------------------------------------------------------------
// GLPI client — lazy, validated on first use
// ---------------------------------------------------------------------------

var (
	client  *glpi.Client
	once    sync.Once
	initErr error
)

func getClient() (*glpi.Client, error) {
	once.Do(func() {
		c, err := glpi.New(glpi.Config{
			BaseURL:     os.Getenv("GLPI_URL"),
			UserToken:   os.Getenv("GLPI_USER_TOKEN"),
			AppToken:    os.Getenv("GLPI_APP_TOKEN"),
			CAFile:      os.Getenv("GLPI_CA_FILE"),
			InsecureTLS: os.Getenv("GLPI_INSECURE_TLS") == "1",
			ResolveIP:   os.Getenv("GLPI_RESOLVE_IP"),
			AllowWrite:  os.Getenv("GLPI_ALLOW_WRITE") == "1",
		})
		if err != nil {
			initErr = err
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := c.Ping(ctx); err != nil {
			initErr = fmt.Errorf("GLPI connection failed: %w", err)
			return
		}
		log.Printf("GLPI connected")
		client = c
	})
	return client, initErr
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func ctx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 60*time.Second)
}

func jsonOut(v any) (string, bool) {
	data, err := json.Marshal(v)
	if err != nil {
		return err.Error(), true
	}
	return string(data), false
}

// pageToStart converts a 1-based page and a limit into a 0-based row offset.
func pageToStart(page, limit int) (start, lim int) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	if page < 1 {
		page = 1
	}
	return (page - 1) * limit, limit
}

// projectItemTypes is a hint only; the client accepts any GLPI class name.
var projectItemTypes = []string{
	"Project", "ProjectTask", "ProjectState", "ProjectType", "ProjectTaskType",
	"ProjectTaskTeam", "ProjectTeam", "User", "Group", "Entity",
}

// ---------------------------------------------------------------------------
// Tool handlers
// ---------------------------------------------------------------------------

func handleInitialize(_ json.RawMessage) (string, bool) { return guide, false }

func handleItemTypes(_ json.RawMessage) (string, bool) { return jsonOut(projectItemTypes) }

func handleSession(_ json.RawMessage) (string, bool) {
	c, err := getClient()
	if err != nil {
		return err.Error(), true
	}
	cx, cancel := ctx()
	defer cancel()
	info, err := c.Session(cx)
	if err != nil {
		return err.Error(), true
	}
	return jsonOut(info)
}

func handleSearchOptions(args json.RawMessage) (string, bool) {
	var p struct {
		ItemType string `json:"item_type"`
	}
	if err := json.Unmarshal(args, &p); err != nil || p.ItemType == "" {
		return "item_type is required", true
	}
	c, err := getClient()
	if err != nil {
		return err.Error(), true
	}
	cx, cancel := ctx()
	defer cancel()
	opts, err := c.SearchOptions(cx, p.ItemType)
	if err != nil {
		return err.Error(), true
	}
	type row struct {
		ID       int      `json:"id"`
		UID      string   `json:"uid"`
		Name     string   `json:"name"`
		DataType string   `json:"datatype,omitempty"`
		Types    []string `json:"searchtypes,omitempty"`
	}
	rows := []row{}
	for _, id := range glpi.SortedIDs(opts) {
		o := opts[id]
		rows = append(rows, row{ID: id, UID: o.UID, Name: o.Name, DataType: o.DataType, Types: o.AvailableSearchTypes})
	}
	return jsonOut(rows)
}

func handleSearch(args json.RawMessage) (string, bool) {
	var p struct {
		ItemType string   `json:"item_type"`
		Criteria string   `json:"criteria"`
		Fields   []string `json:"fields"`
		Sort     string   `json:"sort"`
		Order    string   `json:"order"`
		Limit    int      `json:"limit"`
		Page     int      `json:"page"`
	}
	if err := json.Unmarshal(args, &p); err != nil || p.ItemType == "" {
		return "item_type is required", true
	}
	criteria, err := glpi.ParseCriteria(p.Criteria)
	if err != nil {
		return err.Error(), true
	}
	c, err := getClient()
	if err != nil {
		return err.Error(), true
	}
	start, limit := pageToStart(p.Page, p.Limit)
	cx, cancel := ctx()
	defer cancel()
	res, err := c.Search(cx, p.ItemType, glpi.SearchParams{
		Criteria: criteria, Fields: p.Fields, Sort: p.Sort, Order: p.Order, Start: start, Limit: limit,
	})
	if err != nil {
		return err.Error(), true
	}
	return jsonOut(res)
}

func handleGet(args json.RawMessage) (string, bool) {
	p := struct {
		ItemType        string `json:"item_type"`
		ID              int    `json:"id"`
		ExpandDropdowns *bool  `json:"expand_dropdowns"`
	}{}
	if err := json.Unmarshal(args, &p); err != nil || p.ItemType == "" || p.ID == 0 {
		return "item_type and id are required", true
	}
	expand := p.ExpandDropdowns == nil || *p.ExpandDropdowns
	c, err := getClient()
	if err != nil {
		return err.Error(), true
	}
	cx, cancel := ctx()
	defer cancel()
	raw, err := c.GetItem(cx, p.ItemType, p.ID, expand)
	if err != nil {
		return err.Error(), true
	}
	return string(raw), false
}

func handleSubItems(args json.RawMessage) (string, bool) {
	p := struct {
		ItemType        string `json:"item_type"`
		ID              int    `json:"id"`
		SubItemType     string `json:"sub_item_type"`
		ExpandDropdowns *bool  `json:"expand_dropdowns"`
		Limit           int    `json:"limit"`
		Page            int    `json:"page"`
	}{}
	if err := json.Unmarshal(args, &p); err != nil || p.ItemType == "" || p.ID == 0 || p.SubItemType == "" {
		return "item_type, id and sub_item_type are required", true
	}
	expand := p.ExpandDropdowns == nil || *p.ExpandDropdowns
	c, err := getClient()
	if err != nil {
		return err.Error(), true
	}
	start, limit := pageToStart(p.Page, p.Limit)
	cx, cancel := ctx()
	defer cancel()
	raw, err := c.SubItems(cx, p.ItemType, p.ID, p.SubItemType, expand, start, limit)
	if err != nil {
		return err.Error(), true
	}
	return string(raw), false
}

// text wraps a friendly-layer result as a tool response.
func text(s string, err error) (string, bool) {
	if err != nil {
		return err.Error(), true
	}
	return s, false
}

func handleProjects(args json.RawMessage) (string, bool) {
	var p struct {
		State    string `json:"state"`
		Type     string `json:"type"`
		Manager  string `json:"manager"`
		Text     string `json:"text"`
		ParentID int    `json:"parent_id"`
		Overdue  bool   `json:"overdue"`
		Limit    int    `json:"limit"`
		Page     int    `json:"page"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return "invalid arguments: " + err.Error(), true
	}
	c, err := getClient()
	if err != nil {
		return err.Error(), true
	}
	cx, cancel := ctx()
	defer cancel()
	return text(c.Projects(cx, glpi.ProjectFilter{
		State: p.State, Type: p.Type, Manager: p.Manager, Text: p.Text,
		ParentID: p.ParentID, Overdue: p.Overdue, Limit: p.Limit, Page: p.Page,
	}))
}

func handleProject(args json.RawMessage) (string, bool) {
	var p struct {
		ID      int      `json:"id"`
		Include []string `json:"include"`
	}
	if err := json.Unmarshal(args, &p); err != nil || p.ID == 0 {
		return "id is required", true
	}
	c, err := getClient()
	if err != nil {
		return err.Error(), true
	}
	cx, cancel := ctx()
	defer cancel()
	return text(c.ProjectCard(cx, p.ID, p.Include))
}

func handleTasks(args json.RawMessage) (string, bool) {
	var p struct {
		ProjectID int    `json:"project_id"`
		State     string `json:"state"`
		Assignee  string `json:"assignee"`
		DueBefore string `json:"due_before"`
		OpenOnly  bool   `json:"open_only"`
		Overdue   bool   `json:"overdue"`
		Limit     int    `json:"limit"`
		Page      int    `json:"page"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return "invalid arguments: " + err.Error(), true
	}
	c, err := getClient()
	if err != nil {
		return err.Error(), true
	}
	cx, cancel := ctx()
	defer cancel()
	return text(c.Tasks(cx, glpi.TaskFilter{
		ProjectID: p.ProjectID, State: p.State, Assignee: p.Assignee, DueBefore: p.DueBefore,
		OpenOnly: p.OpenOnly, Overdue: p.Overdue, Limit: p.Limit, Page: p.Page,
	}))
}

func handleTask(args json.RawMessage) (string, bool) {
	var p struct {
		ID      int      `json:"id"`
		Include []string `json:"include"`
	}
	if err := json.Unmarshal(args, &p); err != nil || p.ID == 0 {
		return "id is required", true
	}
	c, err := getClient()
	if err != nil {
		return err.Error(), true
	}
	cx, cancel := ctx()
	defer cancel()
	return text(c.TaskCard(cx, p.ID, p.Include))
}

func handlePeople(args json.RawMessage) (string, bool) {
	var p struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(args, &p); err != nil || p.Text == "" {
		return "text is required", true
	}
	c, err := getClient()
	if err != nil {
		return err.Error(), true
	}
	cx, cancel := ctx()
	defer cancel()
	return text(c.People(cx, p.Text))
}

// ---------------------------------------------------------------------------
// Write handlers (registered only when GLPI_ALLOW_WRITE=1)
// ---------------------------------------------------------------------------

func writeClient() (*glpi.Client, context.Context, context.CancelFunc, string) {
	c, err := getClient()
	if err != nil {
		return nil, nil, nil, err.Error()
	}
	cx, cancel := ctx()
	return c, cx, cancel, ""
}

func handleUpdateTask(args json.RawMessage) (string, bool) {
	var p struct {
		ID           int     `json:"id"`
		Name         *string `json:"name"`
		Description  *string `json:"description"`
		State        *string `json:"state"`
		PercentDone  *int    `json:"percent_done"`
		PlannedStart *string `json:"planned_start"`
		PlannedEnd   *string `json:"planned_end"`
		Apply        bool    `json:"apply"`
	}
	if err := json.Unmarshal(args, &p); err != nil || p.ID == 0 {
		return "id is required (other fields must have the documented types)", true
	}
	c, cx, cancel, msg := writeClient()
	if c == nil {
		return msg, true
	}
	defer cancel()
	return text(c.UpdateTask(cx, glpi.TaskUpdate{ID: p.ID, Name: p.Name, Description: p.Description, State: p.State,
		PercentDone: p.PercentDone, PlannedStart: p.PlannedStart, PlannedEnd: p.PlannedEnd, Apply: p.Apply}))
}

func handleUpdateProject(args json.RawMessage) (string, bool) {
	var p struct {
		ID           int     `json:"id"`
		Name         *string `json:"name"`
		Description  *string `json:"description"`
		State        *string `json:"state"`
		PercentDone  *int    `json:"percent_done"`
		PlannedStart *string `json:"planned_start"`
		PlannedEnd   *string `json:"planned_end"`
		Manager      *string `json:"manager"`
		Apply        bool    `json:"apply"`
	}
	if err := json.Unmarshal(args, &p); err != nil || p.ID == 0 {
		return "id is required (other fields must have the documented types)", true
	}
	c, cx, cancel, msg := writeClient()
	if c == nil {
		return msg, true
	}
	defer cancel()
	return text(c.UpdateProject(cx, glpi.ProjectUpdate{ID: p.ID, Name: p.Name, Description: p.Description, State: p.State,
		PercentDone: p.PercentDone, PlannedStart: p.PlannedStart, PlannedEnd: p.PlannedEnd, Manager: p.Manager, Apply: p.Apply}))
}

func handleCreateProject(args json.RawMessage) (string, bool) {
	var p struct {
		Name         string `json:"name"`
		Description  string `json:"description"`
		Code         string `json:"code"`
		State        string `json:"state"`
		Type         string `json:"type"`
		Manager      string `json:"manager"`
		ParentID     int    `json:"parent_id"`
		PlannedStart string `json:"planned_start"`
		PlannedEnd   string `json:"planned_end"`
		Apply        bool   `json:"apply"`
	}
	if err := json.Unmarshal(args, &p); err != nil || p.Name == "" {
		return "name is required", true
	}
	c, cx, cancel, msg := writeClient()
	if c == nil {
		return msg, true
	}
	defer cancel()
	return text(c.CreateProject(cx, glpi.ProjectCreate{Name: p.Name, Description: p.Description, Code: p.Code, State: p.State,
		Type: p.Type, Manager: p.Manager, ParentID: p.ParentID, PlannedStart: p.PlannedStart, PlannedEnd: p.PlannedEnd, Apply: p.Apply}))
}

func handleCreateTask(args json.RawMessage) (string, bool) {
	var p struct {
		ProjectID    int    `json:"project_id"`
		Name         string `json:"name"`
		Description  string `json:"description"`
		State        string `json:"state"`
		Assignee     string `json:"assignee"`
		ParentTaskID int    `json:"parent_task_id"`
		PlannedStart string `json:"planned_start"`
		PlannedEnd   string `json:"planned_end"`
		Apply        bool   `json:"apply"`
	}
	if err := json.Unmarshal(args, &p); err != nil || p.ProjectID == 0 || p.Name == "" {
		return "project_id and name are required", true
	}
	c, cx, cancel, msg := writeClient()
	if c == nil {
		return msg, true
	}
	defer cancel()
	return text(c.CreateTask(cx, glpi.TaskCreate{ProjectID: p.ProjectID, Name: p.Name, Description: p.Description, State: p.State,
		Assignee: p.Assignee, ParentTaskID: p.ParentTaskID, PlannedStart: p.PlannedStart, PlannedEnd: p.PlannedEnd, Apply: p.Apply}))
}

func handleAssignTask(args json.RawMessage) (string, bool) {
	var p struct {
		TaskID   int    `json:"task_id"`
		Assignee string `json:"assignee"`
		Action   string `json:"action"`
		Apply    bool   `json:"apply"`
	}
	if err := json.Unmarshal(args, &p); err != nil || p.TaskID == 0 || p.Assignee == "" {
		return "task_id and assignee are required", true
	}
	c, cx, cancel, msg := writeClient()
	if c == nil {
		return msg, true
	}
	defer cancel()
	return text(c.AssignTask(cx, glpi.TaskAssign{TaskID: p.TaskID, Assignee: p.Assignee, Action: p.Action, Apply: p.Apply}))
}

// ---------------------------------------------------------------------------
// Server
// ---------------------------------------------------------------------------

func buildServer() *minimcp.Dispatcher {
	d := minimcp.NewDispatcher()
	s := minimcp.NewServer("glpi-mcp", "0.1.0")
	s.SetInstructions("Read-only access to GLPI projects and project tasks. Start with glpi_projects / glpi_tasks (compact, name-based). Call glpi_initialize for the lower-level glpi_search tools.")

	str := func(desc string) *minimcp.Schema { return &minimcp.Schema{Type: "string", Description: desc} }
	num := func(desc string) *minimcp.Schema { return &minimcp.Schema{Type: "number", Description: desc} }

	boolean := func(desc string) *minimcp.Schema { return &minimcp.Schema{Type: "boolean", Description: desc} }
	include := func(desc string) *minimcp.Schema {
		return &minimcp.Schema{Type: "array", Description: desc, Items: &minimcp.Schema{Type: "string"}}
	}

	s.AddTool(minimcp.Tool{
		Name:        "glpi_projects",
		Description: "START HERE for projects. Lists projects, one compact line each (id, name, state, % done, manager, due date). Filter by state/type/manager by NAME (resolved for you; unknown names return the valid options). Follow up with glpi_project(id).",
		InputSchema: &minimcp.Schema{
			Type: "object",
			Properties: minimcp.Properties{
				"state":     str("State name, e.g. Processing, Closed."),
				"type":      str("Project type name."),
				"manager":   str("Manager login or name fragment (see glpi_people)."),
				"text":      str("Substring of the project name."),
				"parent_id": num("List only the sub-projects of this project id."),
				"overdue":   boolean("Only projects past their planned end date that are not in a finished state."),
				"limit":     num("Rows per page (default 20, max 100)."),
				"page":      num("Page number, 1-indexed."),
			},
		},
	}, handleProjects)

	s.AddTool(minimcp.Tool{
		Name:        "glpi_project",
		Description: "Summary card for one project: state, manager, dates, team, task counts (total/open/overdue), sub-project count. Long text is not included, only its size. Use include to expand: tasks, children, description, comment.",
		InputSchema: &minimcp.Schema{
			Type: "object",
			Properties: minimcp.Properties{
				"id":      num("Project id (from glpi_projects)."),
				"include": include("Any of: tasks, children, description, comment."),
			},
			Required: []string{"id"},
		},
	}, handleProject)

	s.AddTool(minimcp.Tool{
		Name:        "glpi_tasks",
		Description: "Lists project tasks, one compact line each (id, name, project, state, % done, due date), soonest due first. Filter by project, state name, assignee, due date, open/overdue. Follow up with glpi_task(id). Results carry a warning if GLPI is hiding tasks from this profile.",
		InputSchema: &minimcp.Schema{
			Type: "object",
			Properties: minimcp.Properties{
				"project_id": num("Only tasks of this project id."),
				"state":      str("State name, e.g. Processing."),
				"assignee":   str("Task team member login or name fragment (see glpi_people)."),
				"due_before": str("YYYY-MM-DD: planned end before this date."),
				"open_only":  boolean("Only tasks under 100% done."),
				"overdue":    boolean("Only open tasks past their planned end date."),
				"limit":      num("Rows per page (default 20, max 100)."),
				"page":       num("Page number, 1-indexed."),
			},
		},
	}, handleTasks)

	s.AddTool(minimcp.Tool{
		Name:        "glpi_task",
		Description: "Summary card for one project task: project, state, dates, creator, assigned team. Long text is only sized; use include to read it: description, comment.",
		InputSchema: &minimcp.Schema{
			Type: "object",
			Properties: minimcp.Properties{
				"id":      num("Task id (from glpi_tasks)."),
				"include": include("Any of: description, comment."),
			},
			Required: []string{"id"},
		},
	}, handleTask)

	s.AddTool(minimcp.Tool{
		Name:        "glpi_people",
		Description: "Find GLPI users by login or name fragment. Returns id, login and full name.",
		InputSchema: &minimcp.Schema{
			Type:       "object",
			Properties: minimcp.Properties{"text": str("Login, first name or last name fragment.")},
			Required:   []string{"text"},
		},
	}, handlePeople)

	s.AddTool(minimcp.Tool{
		Name:        "glpi_initialize",
		Description: "Call before any other glpi_* tool. Returns the workflow, criteria syntax and gotchas.",
		InputSchema: &minimcp.Schema{Type: "object"},
	}, handleInitialize)

	s.AddTool(minimcp.Tool{
		Name:        "glpi_session",
		Description: "Show who the server acts as: active profile, entities and GLPI version. Use when expected projects are missing (visibility is profile/entity/team based).",
		InputSchema: &minimcp.Schema{Type: "object"},
	}, handleSession)

	s.AddTool(minimcp.Tool{
		Name:        "glpi_item_types",
		Description: "List the GLPI item types relevant to project work. Any GLPI class name is accepted by the other tools.",
		InputSchema: &minimcp.Schema{Type: "object"},
	}, handleItemTypes)

	s.AddTool(minimcp.Tool{
		Name:        "glpi_search_options",
		Description: "List the columns of an item type: numeric id, uid (e.g. Project.name), display name and data type. Use before glpi_search.",
		InputSchema: &minimcp.Schema{
			Type:       "object",
			Properties: minimcp.Properties{"item_type": str("GLPI class name, e.g. Project, ProjectTask")},
			Required:   []string{"item_type"},
		},
	}, handleSearchOptions)

	s.AddTool(minimcp.Tool{
		Name:        "glpi_search",
		Description: "Search items. criteria is a JSON string: an array of [field, searchtype, value, link?] or {field, searchtype, value, link} items. Fields accept a numeric id, uid or unique display name. Rows are keyed by uid.",
		InputSchema: &minimcp.Schema{
			Type: "object",
			Properties: minimcp.Properties{
				"item_type": str("GLPI class name, e.g. Project, ProjectTask"),
				"criteria":  str(`JSON array, e.g. [["Project.name","contains","kitchen"],["Project.ProjectState.name","equals","In progress","AND"]]. Omit or "[]" for no filter.`),
				"fields":    {Type: "array", Description: "Columns to return (id, uid or name). Omit for GLPI's default columns. The id column is added automatically.", Items: &minimcp.Schema{Type: "string"}},
				"sort":      str("Column to sort by."),
				"order":     str("ASC (default) or DESC."),
				"limit":     num("Rows per page (default 50, max 200)."),
				"page":      num("Page number, 1-indexed (default 1)."),
			},
			Required: []string{"item_type"},
		},
	}, handleSearch)

	s.AddTool(minimcp.Tool{
		Name:        "glpi_get",
		Description: "Fetch one item by id with all its fields.",
		InputSchema: &minimcp.Schema{
			Type: "object",
			Properties: minimcp.Properties{
				"item_type":        str("GLPI class name"),
				"id":               num("Item id"),
				"expand_dropdowns": {Type: "boolean", Description: "Return names instead of ids for dropdown fields (default true)."},
			},
			Required: []string{"item_type", "id"},
		},
	}, handleGet)

	s.AddTool(minimcp.Tool{
		Name:        "glpi_sub_items",
		Description: "List the sub items of an item, e.g. item_type=Project, id=12, sub_item_type=ProjectTask.",
		InputSchema: &minimcp.Schema{
			Type: "object",
			Properties: minimcp.Properties{
				"item_type":        str("Parent GLPI class name"),
				"id":               num("Parent id"),
				"sub_item_type":    str("Child GLPI class name"),
				"expand_dropdowns": {Type: "boolean", Description: "Return names instead of ids for dropdown fields (default true)."},
				"limit":            num("Rows per page (default 50, max 200)."),
				"page":             num("Page number, 1-indexed (default 1)."),
			},
			Required: []string{"item_type", "id", "sub_item_type"},
		},
	}, handleSubItems)

	if client != nil && client.WriteEnabled() {
		registerWriteTools(s)
	}

	s.Register(d)
	return d
}

// registerWriteTools adds the write tools. Every one previews by default and
// only changes GLPI when called again with apply=true.
func registerWriteTools(s *minimcp.Server) {
	str := func(desc string) *minimcp.Schema { return &minimcp.Schema{Type: "string", Description: desc} }
	num := func(desc string) *minimcp.Schema { return &minimcp.Schema{Type: "number", Description: desc} }
	apply := &minimcp.Schema{Type: "boolean", Description: "false/omitted: PREVIEW only, nothing changes. true: apply. Repeat the identical call with apply=true after reviewing the preview."}
	const note = " PREVIEWS by default (shows before → after, changes nothing); call again with the same arguments and apply=true to write. One item per call."

	s.AddTool(minimcp.Tool{
		Name:        "glpi_update_task",
		Description: "Change fields of ONE existing project task: name, description, state (by name), percent_done (0-100), planned_start/planned_end (YYYY-MM-DD). Only the fields you pass change." + note,
		InputSchema: &minimcp.Schema{Type: "object", Properties: minimcp.Properties{
			"id": num("Task id (from glpi_tasks)."), "name": str("New name."), "description": str("New description as plain text (replaces the old one)."),
			"state": str("State name, e.g. Processing."), "percent_done": num("0-100."),
			"planned_start": str("YYYY-MM-DD."), "planned_end": str("YYYY-MM-DD."), "apply": apply,
		}, Required: []string{"id"}},
	}, handleUpdateTask)

	s.AddTool(minimcp.Tool{
		Name:        "glpi_update_project",
		Description: "Change fields of ONE existing project: name, description, state (by name), percent_done, planned_start/planned_end, manager (login or name). Only the fields you pass change." + note,
		InputSchema: &minimcp.Schema{Type: "object", Properties: minimcp.Properties{
			"id": num("Project id (from glpi_projects)."), "name": str("New name."), "description": str("New description as plain text (replaces the old one)."),
			"state": str("State name."), "percent_done": num("0-100."), "manager": str("Manager login or name fragment."),
			"planned_start": str("YYYY-MM-DD."), "planned_end": str("YYYY-MM-DD."), "apply": apply,
		}, Required: []string{"id"}},
	}, handleUpdateProject)

	s.AddTool(minimcp.Tool{
		Name:        "glpi_create_project",
		Description: "Create ONE project. Refuses if a project with the exact same name exists." + note,
		InputSchema: &minimcp.Schema{Type: "object", Properties: minimcp.Properties{
			"name": str("Project name (required)."), "description": str("Plain-text description."), "code": str("Project code."),
			"state": str("State name."), "type": str("Project type name."), "manager": str("Manager login or name fragment."),
			"parent_id":     num("Make it a sub-project of this project id."),
			"planned_start": str("YYYY-MM-DD."), "planned_end": str("YYYY-MM-DD."), "apply": apply,
		}, Required: []string{"name"}},
	}, handleCreateProject)

	s.AddTool(minimcp.Tool{
		Name:        "glpi_create_task",
		Description: "Create ONE task in a project, optionally assigned to one person. Refuses if that project already has a task with the exact same name." + note,
		InputSchema: &minimcp.Schema{Type: "object", Properties: minimcp.Properties{
			"project_id": num("Project id (required)."), "name": str("Task name (required)."), "description": str("Plain-text description."),
			"state": str("State name."), "assignee": str("Person's login or name fragment."),
			"parent_task_id": num("Make it a sub-task of this task id (must be in the same project)."),
			"planned_start":  str("YYYY-MM-DD."), "planned_end": str("YYYY-MM-DD."), "apply": apply,
		}, Required: []string{"project_id", "name"}},
	}, handleCreateTask)

	s.AddTool(minimcp.Tool{
		Name:        "glpi_assign_task",
		Description: "Add (default) or remove ONE person on a task's team. Refuses to add someone already on it or remove someone who is not." + note,
		InputSchema: &minimcp.Schema{Type: "object", Properties: minimcp.Properties{
			"task_id": num("Task id."), "assignee": str("Person's login or name fragment."),
			"action": {Type: "string", Description: "add (default) or remove.", Enum: []any{"add", "remove"}}, "apply": apply,
		}, Required: []string{"task_id", "assignee"}},
	}, handleAssignTask)
}

// loadDotEnv reads a .env file from the working directory and sets any
// KEY=VALUE pairs as environment variables, without overriding variables that
// are already set. Missing file is silently ignored.
func loadDotEnv() {
	data, err := os.ReadFile(".env")
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		if _, set := os.LookupEnv(key); set {
			continue
		}
		os.Setenv(key, strings.TrimSpace(value))
	}
}

func main() {
	useHTTP := flag.Bool("http", false, "Run as streamable-HTTP server instead of stdio")
	addr := flag.String("addr", ":3000", "Listen address for HTTP mode")
	flag.Parse()

	loadDotEnv()

	c, err := getClient()
	if err != nil {
		log.Fatalf("GLPI init failed: %v", err)
	}
	defer func() {
		cx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		c.Close(cx)
	}()

	d := buildServer()

	if *useHTTP {
		log.Printf("Starting HTTP server on %s", *addr)
		if err := http.ListenAndServe(*addr, minimcp.NewHTTPHandler(d)); err != nil {
			log.Fatal(err)
		}
		return
	}
	if err := minimcp.ServeStdio(d, os.Stdin, os.Stdout); err != nil {
		log.Fatal(err)
	}
}
