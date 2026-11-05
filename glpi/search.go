package glpi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// SearchOption describes one searchable/displayable column of an item type.
// GLPI identifies columns by number; UID ("Project.name") and Name ("Name") are
// the human-readable handles.
type SearchOption struct {
	Name     string `json:"name"`
	Table    string `json:"table"`
	Field    string `json:"field"`
	DataType string `json:"datatype"`
	UID      string `json:"uid"`
	// AvailableSearchTypes are the searchtypes GLPI honours for this column. Any
	// other searchtype is silently treated as "contains", so Search rejects it.
	AvailableSearchTypes []string `json:"available_searchtypes"`

	label string // unique within the item type; set by assignLabels
}

// Label is the key used for this column in translated results. It is the uid
// unless GLPI reuses that uid for several columns (e.g. Project "Father" shares
// Project.name with the real name column); later duplicates become "Name (id)".
func (o SearchOption) Label(id int) string {
	if o.label != "" {
		return o.label
	}
	switch {
	case o.UID != "":
		return o.UID
	case o.Name != "":
		return o.Name
	}
	return strconv.Itoa(id)
}

func assignLabels(opts map[int]SearchOption) {
	seen := map[string]bool{}
	for _, id := range SortedIDs(opts) {
		o := opts[id]
		label := o.Label(id)
		if seen[strings.ToLower(label)] {
			if o.Name != "" {
				label = fmt.Sprintf("%s (%d)", o.Name, id)
			} else {
				label = fmt.Sprintf("%s#%d", label, id)
			}
		}
		seen[strings.ToLower(label)] = true
		o.label = label
		opts[id] = o
	}
}

// SearchOptions returns (and caches) the columns of an item type, keyed by
// GLPI's numeric field id.
func (c *Client) SearchOptions(ctx context.Context, itemType string) (map[int]SearchOption, error) {
	if err := checkName("item type", itemType); err != nil {
		return nil, err
	}
	c.optMu.Lock()
	if opts, ok := c.optCache[itemType]; ok {
		c.optMu.Unlock()
		return opts, nil
	}
	c.optMu.Unlock()

	body, err := c.get(ctx, "/listSearchOptions/"+itemType, nil)
	if err != nil {
		return nil, err
	}
	opts, err := parseSearchOptions(body)
	if err != nil {
		return nil, err
	}
	c.optMu.Lock()
	c.optCache[itemType] = opts
	c.optMu.Unlock()
	return opts, nil
}

func parseSearchOptions(body []byte) (map[int]SearchOption, error) {
	var rawMap map[string]json.RawMessage
	if err := json.Unmarshal(body, &rawMap); err != nil {
		return nil, fmt.Errorf("listSearchOptions: unexpected response: %w", err)
	}
	opts := map[int]SearchOption{}
	for k, v := range rawMap {
		id, err := strconv.Atoi(k)
		if err != nil {
			continue // section headers such as "common": "Characteristics"
		}
		var o SearchOption
		if json.Unmarshal(v, &o) != nil {
			continue
		}
		opts[id] = o
	}
	if len(opts) == 0 {
		return nil, errors.New("listSearchOptions returned no columns (unknown item type, or no right to search it)")
	}
	assignLabels(opts)
	return opts, nil
}

// SortedIDs returns the option ids in ascending order.
func SortedIDs(opts map[int]SearchOption) []int {
	ids := make([]int, 0, len(opts))
	for id := range opts {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	return ids
}

// ResolveField turns a user supplied column (number, uid or display name) into
// GLPI's numeric id.
func ResolveField(opts map[int]SearchOption, s string) (int, error) {
	s = strings.TrimSpace(s)
	if id, err := strconv.Atoi(s); err == nil {
		if _, ok := opts[id]; !ok {
			return 0, fmt.Errorf("unknown field id %d (see glpi_search_options)", id)
		}
		return id, nil
	}
	for _, match := range []func(int, SearchOption) string{
		func(id int, o SearchOption) string { return o.Label(id) }, // unique by construction
		func(_ int, o SearchOption) string { return o.UID },
		func(_ int, o SearchOption) string { return o.Name },
		func(_ int, o SearchOption) string { return o.Field }, // raw DB column, e.g. percent_done
	} {
		var hits []int
		for _, id := range SortedIDs(opts) {
			if v := match(id, opts[id]); v != "" && strings.EqualFold(v, s) {
				hits = append(hits, id)
			}
		}
		switch len(hits) {
		case 1:
			return hits[0], nil
		case 0:
			continue
		default:
			names := make([]string, len(hits))
			for i, id := range hits {
				names[i] = fmt.Sprintf("%d (%s)", id, opts[id].Label(id))
			}
			return 0, fmt.Errorf("field %q is ambiguous: use the uid or numeric id: %s", s, strings.Join(names, ", "))
		}
	}
	return 0, fmt.Errorf("unknown field %q (see glpi_search_options)", s)
}

// Criterion is one search condition.
type Criterion struct {
	Field      string // id, uid or name
	SearchType string // contains, equals, notequals, lessthan, morethan, under, notunder
	Value      string
	Link       string // AND, OR, AND NOT, OR NOT
}

// ParseCriteria accepts a JSON array whose items are either objects
// {"field","searchtype","value","link"} or arrays [field, searchtype, value, link?].
func ParseCriteria(raw string) ([]Criterion, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal([]byte(raw), &items); err != nil {
		return nil, fmt.Errorf("criteria must be a JSON array: %w", err)
	}
	str := func(v any) string {
		if s, ok := v.(string); ok {
			return s
		}
		return fmt.Sprint(v)
	}
	out := make([]Criterion, 0, len(items))
	for i, item := range items {
		var c Criterion
		trimmed := strings.TrimSpace(string(item))
		if strings.HasPrefix(trimmed, "[") {
			var arr []any
			if err := json.Unmarshal(item, &arr); err != nil || len(arr) < 3 || len(arr) > 4 {
				return nil, fmt.Errorf("criteria[%d]: expected [field, searchtype, value, link?]", i)
			}
			c = Criterion{Field: str(arr[0]), SearchType: str(arr[1]), Value: str(arr[2])}
			if len(arr) == 4 {
				c.Link = str(arr[3])
			}
		} else {
			var m map[string]any
			if err := json.Unmarshal(item, &m); err != nil {
				return nil, fmt.Errorf("criteria[%d]: expected an object or array: %w", i, err)
			}
			if v, ok := m["field"]; ok {
				c.Field = str(v)
			}
			if v, ok := m["searchtype"]; ok {
				c.SearchType = str(v)
			}
			if v, ok := m["value"]; ok {
				c.Value = str(v)
			}
			if v, ok := m["link"]; ok {
				c.Link = str(v)
			}
		}
		if c.Field == "" {
			return nil, fmt.Errorf("criteria[%d]: field is required", i)
		}
		if c.SearchType == "" {
			c.SearchType = "contains"
		}
		if c.Link == "" {
			c.Link = "AND"
		}
		out = append(out, c)
	}
	return out, nil
}

var idInLink = regexp.MustCompile(`[?&]id=(\d+)`)

// hasIDColumn reports whether the type exposes its own id column (uid
// "<Type>.id"); related tables such as Type.Document_Item.id don't count.
func hasIDColumn(opts map[int]SearchOption, itemType string) bool {
	for _, o := range opts {
		if o.Field == "id" && o.UID == itemType+".id" {
			return true
		}
	}
	return false
}

func checkSearchType(o SearchOption, id int, st string) error {
	if len(o.AvailableSearchTypes) == 0 {
		return nil
	}
	st = strings.ToLower(st)
	has := map[string]bool{}
	for _, a := range o.AvailableSearchTypes {
		has[a] = true
	}
	if has[st] {
		return nil
	}
	msg := fmt.Sprintf("searchtype %q is not supported by %s (supported: %s)", st, o.Label(id), strings.Join(o.AvailableSearchTypes, ", "))
	if (st == "lessthan" || st == "morethan") && has["contains"] {
		msg += `; for numbers use searchtype "contains" with a comparison in the value, e.g. "<100", ">=40"`
	}
	return errors.New(msg)
}

// normalizeCriterion rewrites a criterion into the form GLPI actually honours.
//
//   - equals/notequals on a dropdown column compare against the numeric id, so a
//     name matches nothing (equals) or is silently ignored (notequals). A
//     non-numeric value is turned into GLPI's anchored text match "^name$" under
//     contains/notcontains, which is an exact match on the displayed name.
//   - empty only filters when the value is the literal "null".
func normalizeCriterion(o SearchOption, cr Criterion) Criterion {
	st := strings.ToLower(cr.SearchType)
	switch {
	case st == "empty":
		cr.SearchType, cr.Value = "empty", "null"
	case (st == "equals" || st == "notequals") && o.DataType == "dropdown":
		v := strings.TrimSpace(cr.Value)
		if _, err := strconv.Atoi(v); err != nil && v != "" && v != "null" {
			if st == "equals" {
				cr.SearchType = "contains"
			} else {
				cr.SearchType = "notcontains"
			}
			cr.Value = "^" + v + "$"
		}
	}
	return cr
}

// SearchParams describes a search.
type SearchParams struct {
	Criteria []Criterion
	Fields   []string // ids, uids or names; empty = GLPI's default columns
	Sort     string   // id, uid or name
	Order    string   // ASC or DESC
	Start    int      // 0-based row offset
	Limit    int
}

// SearchResult holds translated rows: keys are field labels, not numeric ids.
type SearchResult struct {
	TotalCount int              `json:"total"`
	Returned   int              `json:"returned"`
	Start      int              `json:"start"`
	Rows       []map[string]any `json:"rows"`
	// Omitted lists long-text columns dropped from a default-columns search
	// (request them via fields, or use glpi_get).
	Omitted  []string `json:"omitted_columns,omitempty"`
	Warnings []string `json:"warnings,omitempty"`
}

// Search runs /search/:itemtype, resolving field names to ids on the way in and
// translating numeric column keys back to labels on the way out.
func (c *Client) Search(ctx context.Context, itemType string, p SearchParams) (*SearchResult, error) {
	opts, err := c.SearchOptions(ctx, itemType)
	if err != nil {
		return nil, err
	}
	if p.Limit <= 0 {
		p.Limit = 50
	}
	if p.Limit > 200 {
		p.Limit = 200
	}
	if p.Start < 0 {
		p.Start = 0
	}

	var q Query
	for i, cr := range p.Criteria {
		id, err := ResolveField(opts, cr.Field)
		if err != nil {
			return nil, fmt.Errorf("criteria[%d]: %w", i, err)
		}
		if err := checkSearchType(opts[id], id, cr.SearchType); err != nil {
			return nil, fmt.Errorf("criteria[%d]: %w", i, err)
		}
		cr = normalizeCriterion(opts[id], cr)
		pre := fmt.Sprintf("criteria[%d]", i)
		q.Add(pre+"[field]", strconv.Itoa(id))
		q.Add(pre+"[searchtype]", cr.SearchType)
		q.Add(pre+"[value]", cr.Value)
		if i > 0 {
			q.Add(pre+"[link]", cr.Link)
		}
	}

	// Always show the id column (when the type has one) so rows are addressable.
	var show []int
	seen := map[int]bool{}
	if _, ok := opts[2]; ok && len(p.Fields) > 0 {
		show = append(show, 2)
		seen[2] = true
	}
	for _, f := range p.Fields {
		id, err := ResolveField(opts, f)
		if err != nil {
			return nil, fmt.Errorf("fields: %w", err)
		}
		if !seen[id] {
			show = append(show, id)
			seen[id] = true
		}
	}
	// Some types (ProjectTask) have no ID column. GLPI can return each name cell
	// as an HTML link containing the id (giveItems); parse it so rows stay
	// addressable.
	needIDs := !hasIDColumn(opts, itemType)
	if needIDs && len(show) > 0 && !seen[1] {
		show = append(show, 1)
	}
	for i, id := range show {
		q.Add(fmt.Sprintf("forcedisplay[%d]", i), strconv.Itoa(id))
	}
	if needIDs {
		q.Add("giveItems", "true")
	}

	if p.Sort != "" {
		id, err := ResolveField(opts, p.Sort)
		if err != nil {
			return nil, fmt.Errorf("sort: %w", err)
		}
		q.Add("sort", strconv.Itoa(id))
		order := strings.ToUpper(p.Order)
		if order != "DESC" {
			order = "ASC"
		}
		q.Add("order", order)
	}
	q.Add("range", rangeParam(p.Start, p.Limit))

	body, err := c.get(ctx, "/search/"+itemType, q)
	if err != nil {
		return nil, err
	}
	var resp struct {
		TotalCount int              `json:"totalcount"`
		Data       []map[string]any `json:"data"`
		DataHTML   []map[string]any `json:"data_html"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("search: unexpected response: %w", err)
	}

	res := &SearchResult{TotalCount: resp.TotalCount, Start: p.Start, Rows: make([]map[string]any, 0, len(resp.Data))}
	omitted := map[string]bool{}
	for ri, row := range resp.Data {
		out := make(map[string]any, len(row))
		if needIDs && ri < len(resp.DataHTML) {
			if h, ok := resp.DataHTML[ri]["1"].(string); ok {
				if m := idInLink.FindStringSubmatch(h); m != nil {
					n, _ := strconv.Atoi(m[1])
					out["id"] = n
				}
			}
		}
		for k, v := range row {
			if id, err := strconv.Atoi(k); err == nil {
				if o, ok := opts[id]; ok {
					// With no explicit fields GLPI returns its default view, which can
					// include whole HTML descriptions; keep those out unless asked for.
					if len(p.Fields) == 0 && o.DataType == "text" {
						omitted[o.Label(id)] = true
						continue
					}
					out[o.Label(id)] = v
					continue
				}
			}
			out[k] = v
		}
		res.Rows = append(res.Rows, out)
	}
	res.Returned = len(res.Rows)
	for l := range omitted {
		res.Omitted = append(res.Omitted, l)
	}
	sort.Strings(res.Omitted)
	if itemType == "ProjectTask" {
		res.Warnings = c.taskVisibilityWarnings(ctx)
	}
	return res, nil
}
