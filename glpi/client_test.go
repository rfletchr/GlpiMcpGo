package glpi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

const optionsJSON = `{
  "common": "Characteristics",
  "1": {"name": "Name", "table": "glpi_projects", "field": "name", "uid": "Project.name", "datatype": "itemlink"},
  "2": {"name": "ID", "table": "glpi_projects", "field": "id", "uid": "Project.id", "datatype": "number"},
  "12": {"name": "Status", "table": "glpi_projectstates", "field": "name", "uid": "Project.ProjectState.name"},
  "13": {"name": "Name", "table": "glpi_other", "field": "name", "uid": "Project.Other.name"},
  "14": {"name": "Father", "table": "glpi_projects", "field": "name", "uid": "Project.name"}
}`

func newTestClient(t *testing.T, h http.Handler) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c, err := New(Config{BaseURL: srv.URL, UserToken: "utok", AppToken: "atok"})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestSearchTranslatesAndReinitsSession(t *testing.T) {
	var sessions, searches atomic.Int32
	var lastQuery string
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/apirest.php/initSession":
			if r.Header.Get("Authorization") != "user_token utok" || r.Header.Get("App-Token") != "atok" {
				w.WriteHeader(401)
				w.Write([]byte(`["ERROR_GLPI_LOGIN","bad"]`))
				return
			}
			n := sessions.Add(1)
			w.Write([]byte(`{"session_token":"s` + string(rune('0'+n)) + `"}`))
		case "/apirest.php/listSearchOptions/Project":
			w.Write([]byte(optionsJSON))
		case "/apirest.php/search/Project":
			// Invalidate the very first session to force one re-init.
			if searches.Add(1) == 1 {
				w.WriteHeader(401)
				w.Write([]byte(`["ERROR_SESSION_TOKEN_INVALID","gone"]`))
				return
			}
			lastQuery = r.URL.RawQuery
			w.WriteHeader(206)
			w.Write([]byte(`{"totalcount":3,"count":1,"data":[{"1":"Alpha","2":7,"12":"Open"}]}`))
		default:
			w.WriteHeader(404)
		}
	}))

	res, err := c.Search(context.Background(), "Project", SearchParams{
		Criteria: []Criterion{{Field: "Project.name", SearchType: "contains", Value: "Al", Link: "AND"}},
		Fields:   []string{"Project.name", "Project.ProjectState.name"},
		Sort:     "Project.name",
	})
	if err != nil {
		t.Fatal(err)
	}
	if sessions.Load() != 2 {
		t.Errorf("expected 2 initSession calls, got %d", sessions.Load())
	}
	if res.TotalCount != 3 || res.Returned != 1 {
		t.Errorf("bad counts: %+v", res)
	}
	row := res.Rows[0]
	if row["Project.name"] != "Alpha" || row["Project.ProjectState.name"] != "Open" || row["Project.id"] != float64(7) {
		t.Errorf("row not translated: %v", row)
	}
	for _, want := range []string{"criteria%5B0%5D%5Bfield%5D=1", "forcedisplay%5B0%5D=2", "sort=1", "range=0-49"} {
		if !strings.Contains(lastQuery, want) {
			t.Errorf("query %q missing %q", lastQuery, want)
		}
	}
}

func TestAmbiguousAndUnknownFields(t *testing.T) {
	opts, err := parseSearchOptions([]byte(optionsJSON))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveField(opts, "Name"); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Errorf("expected ambiguity error, got %v", err)
	}
	if id, err := ResolveField(opts, "project.id"); err != nil || id != 2 {
		t.Errorf("uid lookup: %d %v", id, err)
	}
	if id, err := ResolveField(opts, "12"); err != nil || id != 12 {
		t.Errorf("numeric lookup: %d %v", id, err)
	}
	if _, err := ResolveField(opts, "nope"); err == nil {
		t.Error("expected unknown field error")
	}
}

func TestPathInjectionRejected(t *testing.T) {
	c := newTestClient(t, http.NotFoundHandler())
	for _, bad := range []string{"../User", "Project/1", "", "1Project", "Project?x=1"} {
		if _, err := c.GetItem(context.Background(), bad, 1, false); err == nil {
			t.Errorf("item type %q should be rejected", bad)
		}
	}
	if _, err := c.SubItems(context.Background(), "Project", 1, "../x", false, 0, 10); err == nil {
		t.Error("sub item type should be rejected")
	}
}

func TestParseCriteria(t *testing.T) {
	got, err := ParseCriteria(`[["name","contains","x"], {"field":12,"searchtype":"equals","value":3,"link":"OR"}]`)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Link != "AND" || got[1].Field != "12" || got[1].Value != "3" || got[1].Link != "OR" {
		t.Errorf("unexpected: %+v", got)
	}
	if _, err := ParseCriteria(`[["only","two"]]`); err == nil {
		t.Error("expected error for short triplet")
	}
}

func TestDuplicateUidGetsDistinctLabel(t *testing.T) {
	opts, err := parseSearchOptions([]byte(optionsJSON))
	if err != nil {
		t.Fatal(err)
	}
	if got := opts[1].Label(1); got != "Project.name" {
		t.Errorf("id 1 label = %q", got)
	}
	if got := opts[14].Label(14); got != "Father (14)" {
		t.Errorf("id 14 label = %q", got)
	}
	// The shared uid resolves to the first column; the duplicate by its label.
	if id, err := ResolveField(opts, "Project.name"); err != nil || id != 1 {
		t.Errorf("uid lookup: %d %v", id, err)
	}
	if id, err := ResolveField(opts, "Father (14)"); err != nil || id != 14 {
		t.Errorf("label lookup: %d %v", id, err)
	}
	if id, err := ResolveField(opts, "Father"); err != nil || id != 14 {
		t.Errorf("name lookup: %d %v", id, err)
	}
}

func TestSessionWarnsOnTaskVisibilityTrap(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/apirest.php/initSession":
			w.Write([]byte(`{"session_token":"s"}`))
		case "/apirest.php/getActiveProfile":
			w.Write([]byte(`{"active_profile":{"id":8,"name":"Read-Only","interface":"central","project":132129,"projecttask":33}}`))
		default:
			w.WriteHeader(403)
			w.Write([]byte(`["ERROR_RIGHT_MISSING","no"]`))
		}
	}))
	info, err := c.Session(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	w, _ := info["warnings"].([]string)
	if len(w) != 1 || !strings.Contains(w[0], "empty") {
		t.Errorf("expected the task-visibility warning, got %v", info["warnings"])
	}
}

func TestResolveRawColumnName(t *testing.T) {
	opts, _ := parseSearchOptions([]byte(`{"2":{"name":"ID","field":"id","uid":"Project.id"},"7":{"name":"Percent done","field":"percent_done","uid":"Project.percent_done"},"9":{"name":"Done","field":"percent_done2","uid":"Project.x"}}`))
	if id, err := ResolveField(opts, "percent_done"); err != nil || id != 7 {
		t.Errorf("raw column: %d %v", id, err)
	}
}

func TestUnsupportedSearchTypeRejected(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/apirest.php/initSession":
			w.Write([]byte(`{"session_token":"s"}`))
		case "/apirest.php/listSearchOptions/ProjectTask":
			w.Write([]byte(`{"5":{"name":"Percent done","field":"percent_done","uid":"ProjectTask.percent_done","available_searchtypes":["contains","equals"]}}`))
		default:
			t.Errorf("search must not be sent for a rejected criterion: %s", r.URL.Path)
		}
	}))
	_, err := c.Search(context.Background(), "ProjectTask", SearchParams{
		Criteria: []Criterion{{Field: "percent_done", SearchType: "lessthan", Value: "100", Link: "AND"}},
	})
	if err == nil || !strings.Contains(err.Error(), `"<100"`) {
		t.Errorf("expected rejection with numeric hint, got %v", err)
	}
}

func TestNormalizeCriterion(t *testing.T) {
	dd := SearchOption{DataType: "dropdown"}
	txt := SearchOption{DataType: "itemlink"}
	cases := []struct {
		name          string
		o             SearchOption
		in            Criterion
		wantST, wantV string
	}{
		{"dropdown equals name", dd, Criterion{SearchType: "equals", Value: "Processing"}, "contains", "^Processing$"},
		{"dropdown notequals name", dd, Criterion{SearchType: "notequals", Value: "New"}, "notcontains", "^New$"},
		{"dropdown equals id untouched", dd, Criterion{SearchType: "equals", Value: "2"}, "equals", "2"},
		{"text equals untouched", txt, Criterion{SearchType: "equals", Value: "Abc"}, "equals", "Abc"},
		{"empty forces null", dd, Criterion{SearchType: "empty", Value: ""}, "empty", "null"},
	}
	for _, tc := range cases {
		got := normalizeCriterion(tc.o, tc.in)
		if got.SearchType != tc.wantST || got.Value != tc.wantV {
			t.Errorf("%s: got %q %q, want %q %q", tc.name, got.SearchType, got.Value, tc.wantST, tc.wantV)
		}
	}
}

func TestDefaultColumnsDropLongTextAndTaskWarning(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/apirest.php/initSession":
			w.Write([]byte(`{"session_token":"s"}`))
		case "/apirest.php/getActiveProfile":
			w.Write([]byte(`{"active_profile":{"id":3,"name":"Admin","project":1151,"projecttask":1145}}`))
		case "/apirest.php/listSearchOptions/ProjectTask":
			w.Write([]byte(`{"1":{"name":"Name","uid":"ProjectTask.name","datatype":"itemlink"},"21":{"name":"Description","uid":"ProjectTask.content","datatype":"text"}}`))
		case "/apirest.php/search/ProjectTask":
			w.Write([]byte(`{"totalcount":1,"data":[{"1":"T","21":"<p style='x'>long html</p>"}]}`))
		case "/apirest.php/Project/2/ProjectTask":
			w.Write([]byte(`[]`))
		}
	}))
	res, err := c.Search(context.Background(), "ProjectTask", SearchParams{})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := res.Rows[0]["ProjectTask.content"]; ok || len(res.Omitted) != 1 || res.Omitted[0] != "ProjectTask.content" {
		t.Errorf("long text not omitted: %+v", res)
	}
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "INCOMPLETE") {
		t.Errorf("missing visibility warning: %v", res.Warnings)
	}
	// Explicit fields keep the text column.
	res, err = c.Search(context.Background(), "ProjectTask", SearchParams{Fields: []string{"ProjectTask.content"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := res.Rows[0]["ProjectTask.content"]; !ok {
		t.Errorf("explicit text field was dropped: %v", res.Rows[0])
	}
	// Sub-items are wrapped so an empty list is not mistaken for "no tasks".
	raw, err := c.SubItems(context.Background(), "Project", 2, "ProjectTask", true, 0, 50)
	if err != nil || !strings.Contains(string(raw), `"warnings"`) || !strings.Contains(string(raw), `"items":[]`) {
		t.Errorf("sub items not wrapped: %s %v", raw, err)
	}
}

func TestTaskIdsParsedFromItemLinks(t *testing.T) {
	var sawGiveItems bool
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/apirest.php/initSession":
			w.Write([]byte(`{"session_token":"s"}`))
		case "/apirest.php/listSearchOptions/ProjectTask":
			// A related-table id column must not count as the task's own id.
			w.Write([]byte(`{"1":{"name":"Name","field":"name","uid":"ProjectTask.name"},"119":{"name":"Docs","field":"id","uid":"ProjectTask.Document_Item.id"}}`))
		case "/apirest.php/search/ProjectTask":
			sawGiveItems = r.URL.Query().Get("giveItems") == "true"
			w.Write([]byte(`{"totalcount":1,"data":[{"1":"Alpha"}],"data_html":[{"1":"<a id='ProjectTask_51_51' href='/front/projecttask.form.php?id=51'>Alpha</a>"}]}`))
		}
	}))
	res, err := c.Search(context.Background(), "ProjectTask", SearchParams{Fields: []string{"ProjectTask.name"}})
	if err != nil {
		t.Fatal(err)
	}
	if !sawGiveItems || res.Rows[0]["id"] != 51 {
		t.Errorf("task id not extracted (giveItems=%v): %v", sawGiveItems, res.Rows[0])
	}
}

func TestStripHTMLAndTruncate(t *testing.T) {
	in := `<p style="x:1">Hello&nbsp;<b>world</b></p><style>p{}</style><p>Second &amp; last</p>`
	if got := StripHTML(in); got != "Hello world\nSecond & last" {
		t.Errorf("StripHTML = %q", got)
	}
	if got := StripHTML("&lt;p&gt;escaped &lt;b&gt;markup&lt;/b&gt;&lt;/p&gt;"); got != "escaped markup" {
		t.Errorf("entity-escaped = %q", got)
	}
	if got := truncate("abcdef", 3); !strings.HasPrefix(got, "abc…") || !strings.Contains(got, "6 chars") {
		t.Errorf("truncate = %q", got)
	}
}

func TestStripHTMLKeepsLiteralAngleBrackets(t *testing.T) {
	stored := richText("Second <b>literal</b> & more")
	if got := StripHTML(stored); got != "Second <b>literal</b> & more" {
		t.Errorf("round trip = %q", got)
	}
}
