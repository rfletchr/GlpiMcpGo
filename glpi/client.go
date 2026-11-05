// Package glpi is a small read-only client for the GLPI legacy REST API
// (/apirest.php). It has no MCP imports so it can be used from tests, a CLI or
// a batch job.
package glpi

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
)

const maxBody = 32 << 20

// nameRe guards every value interpolated into a URL path (item types, sub item
// types) so a tool argument can never escape the API prefix.
var nameRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]*$`)

// Config holds connection settings.
type Config struct {
	BaseURL     string // site or .../apirest.php URL
	UserToken   string // user's "API token" (remote access key)
	AppToken    string // optional; required only if the API client demands it
	CAFile      string // optional PEM bundle to trust
	InsecureTLS bool   // skip certificate verification
	ResolveIP   string // optional: connect to this IP for the URL's host name
	AllowWrite  bool   // enables the (allow-listed) write endpoints; off by default
}

// APIError is an error reported by GLPI, e.g. ERROR_SESSION_TOKEN_INVALID.
type APIError struct {
	Status  int
	Code    string
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("GLPI %s (HTTP %d): %s", e.Code, e.Status, e.Message)
}

// Client talks to GLPI. Safe for concurrent use.
type Client struct {
	cfg  Config
	http *http.Client

	mu      sync.Mutex
	session string
	profile map[string]any // cached getActiveProfile for the current session

	optMu    sync.Mutex
	optCache map[string]map[int]SearchOption
	ddCache  map[string][]ddItem
}

// New builds a client. It does not touch the network; call Ping to validate.
func New(cfg Config) (*Client, error) {
	if cfg.BaseURL == "" || cfg.UserToken == "" {
		return nil, errors.New("missing credentials: set GLPI_URL and GLPI_USER_TOKEN")
	}
	base := strings.TrimRight(cfg.BaseURL, "/")
	if !strings.HasSuffix(base, "/apirest.php") {
		base += "/apirest.php"
	}
	cfg.BaseURL = base

	tlsCfg := &tls.Config{InsecureSkipVerify: cfg.InsecureTLS}
	if cfg.CAFile != "" {
		pem, err := os.ReadFile(cfg.CAFile)
		if err != nil {
			return nil, fmt.Errorf("reading GLPI_CA_FILE: %w", err)
		}
		pool, err := x509.SystemCertPool()
		if err != nil || pool == nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, errors.New("GLPI_CA_FILE contains no usable certificates")
		}
		tlsCfg.RootCAs = pool
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = tlsCfg
	if cfg.ResolveIP != "" {
		// Like curl --resolve: connect to the given IP but keep the URL's host
		// name for TLS (SNI/verification) and the Host header.
		u, err := url.Parse(cfg.BaseURL)
		if err != nil || u.Hostname() == "" {
			return nil, fmt.Errorf("invalid GLPI_URL %q", cfg.BaseURL)
		}
		host, ip := u.Hostname(), cfg.ResolveIP
		dialer := &net.Dialer{Timeout: 30 * time.Second}
		tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			h, port, err := net.SplitHostPort(addr)
			if err == nil && strings.EqualFold(h, host) {
				addr = net.JoinHostPort(ip, port)
			}
			return dialer.DialContext(ctx, network, addr)
		}
	}

	return &Client{
		cfg:      cfg,
		http:     &http.Client{Transport: tr, Timeout: 60 * time.Second},
		optCache: map[string]map[int]SearchOption{},
	}, nil
}

// ---------------------------------------------------------------------------
// Query building (ordered: GLPI criteria are order-sensitive)
// ---------------------------------------------------------------------------

type kv struct{ k, v string }

// Query is an ordered list of query parameters.
type Query []kv

// Add appends a parameter.
func (q *Query) Add(k, v string) { *q = append(*q, kv{k, v}) }

// Encode renders the query string in insertion order.
func (q Query) Encode() string {
	parts := make([]string, len(q))
	for i, p := range q {
		parts[i] = url.QueryEscape(p.k) + "=" + url.QueryEscape(p.v)
	}
	return strings.Join(parts, "&")
}

// ---------------------------------------------------------------------------
// Transport + session handling
// ---------------------------------------------------------------------------

func parseAPIError(status int, body []byte) error {
	var arr []string
	if json.Unmarshal(body, &arr) == nil && len(arr) > 0 {
		e := &APIError{Status: status, Code: arr[0]}
		if len(arr) > 1 {
			e.Message = arr[1]
		}
		return e
	}
	snippet := strings.TrimSpace(string(body))
	if len(snippet) > 200 {
		snippet = snippet[:200] + "..."
	}
	return &APIError{Status: status, Code: "HTTP_ERROR", Message: snippet}
}

func (c *Client) raw(ctx context.Context, path string, q Query, hdr map[string]string) ([]byte, error) {
	return c.do(ctx, http.MethodGet, path, q, hdr, nil)
}

func (c *Client) do(ctx context.Context, method, path string, q Query, hdr map[string]string, payload []byte) ([]byte, error) {
	u := c.cfg.BaseURL + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	var rdr io.Reader
	if payload != nil {
		rdr = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rdr)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxBody {
		return nil, fmt.Errorf("response from %s exceeds %d bytes", path, maxBody)
	}
	if resp.StatusCode >= 400 {
		return nil, parseAPIError(resp.StatusCode, body)
	}
	return body, nil
}

func (c *Client) sessionToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.session != "" {
		return c.session, nil
	}
	hdr := map[string]string{"Authorization": "user_token " + c.cfg.UserToken}
	if c.cfg.AppToken != "" {
		hdr["App-Token"] = c.cfg.AppToken
	}
	body, err := c.raw(ctx, "/initSession", nil, hdr)
	if err != nil {
		return "", fmt.Errorf("initSession: %w", err)
	}
	var r struct {
		SessionToken string `json:"session_token"`
	}
	if err := json.Unmarshal(body, &r); err != nil || r.SessionToken == "" {
		return "", fmt.Errorf("initSession: unexpected response %.200s", body)
	}
	c.session = r.SessionToken
	return c.session, nil
}

func (c *Client) dropSession(tok string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.session == tok {
		c.session = ""
		c.profile = nil
	}
}

// get performs an authenticated GET, re-establishing the session once if GLPI
// reports the cached token as invalid.
func (c *Client) get(ctx context.Context, path string, q Query) ([]byte, error) {
	for attempt := 0; ; attempt++ {
		tok, err := c.sessionToken(ctx)
		if err != nil {
			return nil, err
		}
		hdr := map[string]string{"Session-Token": tok}
		if c.cfg.AppToken != "" {
			hdr["App-Token"] = c.cfg.AppToken
		}
		body, err := c.raw(ctx, path, q, hdr)
		var ae *APIError
		if errors.As(err, &ae) && ae.Code == "ERROR_SESSION_TOKEN_INVALID" && attempt == 0 {
			c.dropSession(tok)
			continue
		}
		return body, err
	}
}

// Ping opens a session, validating the credentials.
func (c *Client) Ping(ctx context.Context) error {
	_, err := c.sessionToken(ctx)
	return err
}

// Close ends the GLPI session (best effort).
func (c *Client) Close(ctx context.Context) {
	c.mu.Lock()
	tok := c.session
	c.session = ""
	c.mu.Unlock()
	if tok == "" {
		return
	}
	hdr := map[string]string{"Session-Token": tok}
	if c.cfg.AppToken != "" {
		hdr["App-Token"] = c.cfg.AppToken
	}
	c.raw(ctx, "/killSession", nil, hdr)
}

func checkName(kind, s string) error {
	if !nameRe.MatchString(s) {
		return fmt.Errorf("invalid %s %q: expected a GLPI class name such as Project or ProjectTask", kind, s)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Session info
// ---------------------------------------------------------------------------

// activeProfile returns the session's active profile (cached per session token).
func (c *Client) activeProfile(ctx context.Context) (map[string]any, error) {
	c.mu.Lock()
	if c.profile != nil {
		p := c.profile
		c.mu.Unlock()
		return p, nil
	}
	c.mu.Unlock()
	body, err := c.get(ctx, "/getActiveProfile", nil)
	if err != nil {
		return nil, err
	}
	var prof struct {
		Active map[string]any `json:"active_profile"`
	}
	if err := json.Unmarshal(body, &prof); err != nil || prof.Active == nil {
		return nil, errors.New("getActiveProfile: unexpected response")
	}
	c.mu.Lock()
	c.profile = prof.Active
	c.mu.Unlock()
	return prof.Active, nil
}

const (
	projectReadMy  = 1    // Project::READMY ("See (my)")
	projectReadAll = 1024 // Project::READALL ("See all")
)

func projectRights(p map[string]any) (project, task int) {
	pr, _ := p["project"].(float64)
	tr, _ := p["projecttask"].(float64)
	return int(pr), int(tr)
}

// taskVisibilityWarnings explains an incomplete/empty ProjectTask result caused
// by the profile's project rights. Best effort: returns nil if unknown.
func (c *Client) taskVisibilityWarnings(ctx context.Context) []string {
	p, err := c.activeProfile(ctx)
	if err != nil {
		return nil
	}
	pr, _ := projectRights(p)
	var w []string
	if pr&projectReadAll != 0 && pr&projectReadMy != 0 {
		w = append(w, "INCOMPLETE: this profile has both project 'See all' (1024) and 'See (my)' (1); GLPI then lists ProjectTasks only for the user's own team, so task results (and sub-items) are partial or empty. A profile with 'See all' and without 'See (my)' lists every task")
	}
	return w
}

// Session describes who the API is acting as. Useful for diagnosing
// "why can't I see project X" (profile rights / entities).
func (c *Client) Session(ctx context.Context) (map[string]any, error) {
	out := map[string]any{}
	p, err := c.activeProfile(ctx)
	if err != nil {
		return nil, err
	}
	out["profile"] = map[string]any{"id": p["id"], "name": p["name"], "interface": p["interface"]}
	pr, tr := projectRights(p)
	out["project_rights"] = map[string]any{"project": pr, "projecttask": tr}
	var warnings []string
	if pr&projectReadAll == 0 {
		warnings = append(warnings, "profile lacks project 'See all' (1024): only projects where this user is on the team are visible")
	}
	warnings = append(warnings, c.taskVisibilityWarnings(ctx)...)
	if len(warnings) > 0 {
		out["warnings"] = warnings
	}
	if body, err := c.get(ctx, "/getActiveEntities", nil); err == nil {
		var ents struct {
			Active map[string]any `json:"active_entity"`
		}
		if json.Unmarshal(body, &ents) == nil && ents.Active != nil {
			out["entities"] = ents.Active
		}
	}
	// Best effort: restricted profiles may not be allowed to read the config.
	if body, err := c.get(ctx, "/getGlpiConfig", nil); err == nil {
		var cfg struct {
			Cfg struct {
				Version string `json:"version"`
			} `json:"cfg_glpi"`
		}
		if json.Unmarshal(body, &cfg) == nil && cfg.Cfg.Version != "" {
			out["glpi_version"] = cfg.Cfg.Version
		}
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Items
// ---------------------------------------------------------------------------

func rangeParam(start, limit int) string {
	return fmt.Sprintf("%d-%d", start, start+limit-1)
}

// GetItem fetches one item as raw JSON.
func (c *Client) GetItem(ctx context.Context, itemType string, id int, expandDropdowns bool) (json.RawMessage, error) {
	if err := checkName("item type", itemType); err != nil {
		return nil, err
	}
	if id <= 0 {
		return nil, errors.New("id must be a positive integer")
	}
	var q Query
	q.Add("expand_dropdowns", fmt.Sprint(expandDropdowns))
	q.Add("get_hateoas", "false")
	return c.get(ctx, fmt.Sprintf("/%s/%d", itemType, id), q)
}

// SubItems lists the sub items of an item, e.g. Project 12 -> ProjectTask.
func (c *Client) SubItems(ctx context.Context, itemType string, id int, subType string, expandDropdowns bool, start, limit int) (json.RawMessage, error) {
	if err := checkName("item type", itemType); err != nil {
		return nil, err
	}
	if err := checkName("sub item type", subType); err != nil {
		return nil, err
	}
	if id <= 0 {
		return nil, errors.New("id must be a positive integer")
	}
	var q Query
	q.Add("expand_dropdowns", fmt.Sprint(expandDropdowns))
	q.Add("get_hateoas", "false")
	q.Add("range", rangeParam(start, limit))
	raw, err := c.get(ctx, fmt.Sprintf("/%s/%d/%s", itemType, id, subType), q)
	if err != nil || subType != "ProjectTask" {
		return raw, err
	}
	if w := c.taskVisibilityWarnings(ctx); len(w) > 0 {
		return json.Marshal(map[string]any{"warnings": w, "items": json.RawMessage(raw)})
	}
	return raw, nil
}

// ---------------------------------------------------------------------------
// Writes: off by default and limited to an explicit allow-list of endpoints.
// ---------------------------------------------------------------------------

var writeRoutes = []struct {
	method string
	re     *regexp.Regexp
}{
	{http.MethodPost, regexp.MustCompile(`^/Project$`)},
	{http.MethodPut, regexp.MustCompile(`^/Project/\d+$`)},
	{http.MethodPost, regexp.MustCompile(`^/ProjectTask$`)},
	{http.MethodPut, regexp.MustCompile(`^/ProjectTask/\d+$`)},
	{http.MethodPost, regexp.MustCompile(`^/ProjectTaskTeam$`)},
	{http.MethodDelete, regexp.MustCompile(`^/ProjectTaskTeam/\d+$`)}, // team-member link rows only
}

func writeAllowed(method, path string) bool {
	for _, r := range writeRoutes {
		if r.method == method && r.re.MatchString(path) {
			return true
		}
	}
	return false
}

// WriteEnabled reports whether this client may write.
func (c *Client) WriteEnabled() bool { return c.cfg.AllowWrite }

// send performs an allow-listed write. input is wrapped as GLPI's {"input": ...}.
func (c *Client) send(ctx context.Context, method, path string, input any) ([]byte, error) {
	if !c.cfg.AllowWrite {
		return nil, errors.New("writes are disabled (GLPI_ALLOW_WRITE is not set)")
	}
	if !writeAllowed(method, path) {
		return nil, fmt.Errorf("refusing %s %s: not an allowed write endpoint", method, path)
	}
	var payload []byte
	if input != nil {
		var err error
		if payload, err = json.Marshal(map[string]any{"input": input}); err != nil {
			return nil, err
		}
	}
	for attempt := 0; ; attempt++ {
		tok, err := c.sessionToken(ctx)
		if err != nil {
			return nil, err
		}
		hdr := map[string]string{"Session-Token": tok}
		if c.cfg.AppToken != "" {
			hdr["App-Token"] = c.cfg.AppToken
		}
		body, err := c.do(ctx, method, path, nil, hdr, payload)
		var ae *APIError
		// An invalid session is rejected before anything runs, so one retry is safe.
		if errors.As(err, &ae) && ae.Code == "ERROR_SESSION_TOKEN_INVALID" && attempt == 0 {
			c.dropSession(tok)
			continue
		}
		return body, err
	}
}
