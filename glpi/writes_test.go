package glpi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// writeServer is a fake GLPI that records every non-GET request.
type writeServer struct {
	mu     sync.Mutex
	writes []string
	bodies []string
	task   string // JSON of ProjectTask/5
}

func (w *writeServer) handler() http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/apirest.php")
		if r.Method != http.MethodGet {
			b, _ := io.ReadAll(r.Body)
			w.mu.Lock()
			w.writes = append(w.writes, r.Method+" "+path)
			w.bodies = append(w.bodies, string(b))
			w.mu.Unlock()
		}
		switch {
		case path == "/initSession":
			rw.Write([]byte(`{"session_token":"s"}`))
		case path == "/ProjectState":
			rw.Write([]byte(`[{"id":1,"name":"New","is_finished":0},{"id":2,"name":"Processing","is_finished":0},{"id":3,"name":"Closed","is_finished":1}]`))
		case path == "/ProjectTask/5" && r.Method == http.MethodGet:
			rw.Write([]byte(w.task))
		case path == "/ProjectTask/5" && r.Method == http.MethodPut:
			rw.Write([]byte(`[{"5":true,"message":""}]`))
		default:
			rw.WriteHeader(404)
			rw.Write([]byte(`["ERROR_ITEM_NOT_FOUND","nope"]`))
		}
	})
}

const taskJSON = `{"id":5,"name":"Plumbing","content":"<p>old</p>","projects_id":"Kitchen","projectstates_id":0,"auto_projectstates":1,"percent_done":40,"auto_percent_done":1,"plan_start_date":"2026-11-01 09:00:00","plan_end_date":"2026-11-20 17:00:00"}`

func writeClientFor(t *testing.T, ws *writeServer, allow bool) *Client {
	t.Helper()
	srv := httptest.NewServer(ws.handler())
	t.Cleanup(srv.Close)
	c, err := New(Config{BaseURL: srv.URL, UserToken: "u", AllowWrite: allow})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func sp(s string) *string { return &s }
func ip(i int) *int       { return &i }

func TestWritesDisabledByDefault(t *testing.T) {
	ws := &writeServer{task: taskJSON}
	c := writeClientFor(t, ws, false)
	if _, err := c.UpdateTask(context.Background(), TaskUpdate{ID: 5, PercentDone: ip(50), Apply: true}); err == nil || !strings.Contains(err.Error(), "disabled") {
		t.Errorf("expected disabled error, got %v", err)
	}
	if _, err := c.send(context.Background(), "PUT", "/ProjectTask/5", map[string]any{}); err == nil {
		t.Error("send must refuse when writes are disabled")
	}
	if len(ws.writes) != 0 {
		t.Errorf("a write reached the server: %v", ws.writes)
	}
}

func TestPreviewNeverWrites(t *testing.T) {
	ws := &writeServer{task: taskJSON}
	c := writeClientFor(t, ws, true)
	out, err := c.UpdateTask(context.Background(), TaskUpdate{ID: 5, State: sp("closed"), PercentDone: ip(100)})
	if err != nil {
		t.Fatal(err)
	}
	if len(ws.writes) != 0 {
		t.Fatalf("preview sent writes: %v", ws.writes)
	}
	for _, want := range []string{"PREVIEW", "state: (none) → Closed", "percent done: 40 → 100", "auto state: on → off", "auto percent: on → off", "apply=true"} {
		if !strings.Contains(out, want) {
			t.Errorf("preview missing %q:\n%s", want, out)
		}
	}
}

func TestApplySendsOneExactPut(t *testing.T) {
	ws := &writeServer{task: taskJSON}
	c := writeClientFor(t, ws, true)
	out, err := c.UpdateTask(context.Background(), TaskUpdate{ID: 5, State: sp("Closed"), PercentDone: ip(100), Apply: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(ws.writes) != 1 || ws.writes[0] != "PUT /ProjectTask/5" {
		t.Fatalf("expected exactly one PUT /ProjectTask/5, got %v", ws.writes)
	}
	var body struct {
		Input map[string]any `json:"input"`
	}
	if err := json.Unmarshal([]byte(ws.bodies[0]), &body); err != nil {
		t.Fatal(err)
	}
	want := map[string]float64{"projectstates_id": 3, "auto_projectstates": 0, "percent_done": 100, "auto_percent_done": 0}
	if len(body.Input) != len(want) {
		t.Errorf("payload has unexpected fields: %v", body.Input)
	}
	for k, v := range want {
		if body.Input[k] != v {
			t.Errorf("payload[%s] = %v, want %v", k, body.Input[k], v)
		}
	}
	if !strings.HasPrefix(out, "APPLIED") {
		t.Errorf("unexpected result: %s", out)
	}
}

func TestNoOpAndBadInputRejected(t *testing.T) {
	ws := &writeServer{task: taskJSON}
	c := writeClientFor(t, ws, true)
	ctx := context.Background()
	cases := map[string]TaskUpdate{
		"no-op percent":    {ID: 5, PercentDone: ip(40), Apply: true},
		"no-op date":       {ID: 5, PlannedEnd: sp("2026-11-20"), Apply: true},
		"percent too high": {ID: 5, PercentDone: ip(101), Apply: true},
		"bad date":         {ID: 5, PlannedEnd: sp("20/11/2026"), Apply: true},
		"end before start": {ID: 5, PlannedEnd: sp("2026-10-01"), Apply: true},
		"unknown state":    {ID: 5, State: sp("Bogus"), Apply: true},
		"empty name":       {ID: 5, Name: sp("  "), Apply: true},
		"nothing at all":   {ID: 5, Apply: true},
		"missing task":     {ID: 99, PercentDone: ip(10), Apply: true},
	}
	for name, u := range cases {
		if _, err := c.UpdateTask(ctx, u); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
	if len(ws.writes) != 0 {
		t.Errorf("rejected requests still wrote: %v", ws.writes)
	}
}

func TestAllowListBlocksOtherEndpoints(t *testing.T) {
	ws := &writeServer{task: taskJSON}
	c := writeClientFor(t, ws, true)
	for _, tc := range [][2]string{
		{"DELETE", "/Project/1"}, {"DELETE", "/ProjectTask/5"}, {"PUT", "/User/1"}, {"POST", "/User"},
		{"PUT", "/Profile_User/1"}, {"POST", "/Project/1"}, {"PUT", "/ProjectTask/5/../User"},
		{"PATCH", "/ProjectTask/5"}, {"DELETE", "/ProjectTaskTeam"}, {"DELETE", "/ProjectTaskTeam/x"},
	} {
		if _, err := c.send(context.Background(), tc[0], tc[1], nil); err == nil || !strings.Contains(err.Error(), "not an allowed write endpoint") {
			t.Errorf("%s %s should be refused, got %v", tc[0], tc[1], err)
		}
	}
	if len(ws.writes) != 0 {
		t.Errorf("a blocked write reached the server: %v", ws.writes)
	}
	// ...while the allowed ones pass the gate.
	for _, tc := range [][2]string{{"POST", "/Project"}, {"PUT", "/Project/3"}, {"POST", "/ProjectTask"},
		{"PUT", "/ProjectTask/5"}, {"POST", "/ProjectTaskTeam"}, {"DELETE", "/ProjectTaskTeam/9"}} {
		if !writeAllowed(tc[0], tc[1]) {
			t.Errorf("%s %s should be allowed", tc[0], tc[1])
		}
	}
}

func TestRichTextEscapes(t *testing.T) {
	got := richText("a <b>&\nline2\n\nsecond")
	if got != "<p>a &lt;b&gt;&amp;<br>line2</p><p>second</p>" {
		t.Errorf("richText = %q", got)
	}
}
