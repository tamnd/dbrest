package ir

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/tamnd/dbrest/pgerr"
)

// reserved query-string keys that are not column filters.
var reservedKeys = map[string]bool{
	"select": true, "order": true, "limit": true, "offset": true,
	"and": true, "or": true, "on_conflict": true, "columns": true,
}

// ParseRead parses a GET/HEAD request for relation into a read Query. rawQuery
// is the URL-encoded query string; preferHeaders are the raw Prefer header
// values. All errors are PGRST1xx (*pgerr.APIError).
func ParseRead(relation, rawQuery string, preferHeaders []string) (*Query, *pgerr.APIError) {
	vals, err := url.ParseQuery(rawQuery)
	if err != nil {
		return nil, pgerr.ErrParse("could not parse query string")
	}
	q := &Query{Kind: Read, Relation: Ref{Name: relation}}
	q.Prefer = ParsePrefer(preferHeaders)
	if q.Prefer.Count != nil {
		q.Count = *q.Prefer.Count
	}
	if perr := parseQueryString(q, vals); perr != nil {
		return nil, perr
	}
	return q, nil
}

// parseQueryString fills the parts of a Query shared by reads and writes from
// the decoded query string: the select list and embeds, ordering, the
// limit/offset window, and the horizontal-filter tree. A write uses the filter
// tree as its WHERE and the select list as its returning projection.
func parseQueryString(q *Query, vals url.Values) *pgerr.APIError {
	if sel := vals.Get("select"); sel != "" {
		items, embeds, perr := parseSelect(sel)
		if perr != nil {
			return perr
		}
		q.Select, q.Embeds = items, embeds
	}
	return applyParams(q, vals)
}

// applyParams fills a query's order, window, and filter tree from the query
// string, after partitioning the params by embed prefix. A key of the form
// rel.<rest> whose first segment names one of this level's embeds is routed to
// that embed's nested query (so actors.order=name.asc orders the embedded
// actors, not the parent); everything else applies at this level. The split
// recurses, so a deeper rel.sub.<rest> reaches a nested embed. See spec 09.
func applyParams(q *Query, vals url.Values) *pgerr.APIError {
	self := url.Values{}
	scoped := map[string]url.Values{}
	for key, vs := range vals {
		if key == "select" {
			continue // consumed by the caller / by the embed parens
		}
		if i := strings.IndexByte(key, '.'); i >= 0 {
			if idx := findEmbed(q.Embeds, key[:i]); idx >= 0 {
				prefix := key[:i]
				ev := scoped[prefix]
				if ev == nil {
					ev = url.Values{}
					scoped[prefix] = ev
				}
				ev[key[i+1:]] = vs
				continue
			}
		}
		self[key] = vs
	}

	if ord := self.Get("order"); ord != "" {
		terms, perr := parseOrder(ord)
		if perr != nil {
			return perr
		}
		q.Order = terms
	}
	if lim := self.Get("limit"); lim != "" {
		n, e := strconv.Atoi(lim)
		if e != nil || n < 0 {
			return pgerr.ErrParse("limit must be a non-negative integer")
		}
		q.Limit = &n
	}
	if off := self.Get("offset"); off != "" {
		n, e := strconv.Atoi(off)
		if e != nil || n < 0 {
			return pgerr.ErrParse("offset must be a non-negative integer")
		}
		q.Offset = &n
	}
	cond, perr := parseFilters(self)
	if perr != nil {
		return perr
	}
	q.Where = cond

	for prefix, ev := range scoped {
		idx := findEmbed(q.Embeds, prefix)
		if perr := applyParams(&q.Embeds[idx].Query, ev); perr != nil {
			return perr
		}
	}
	return nil
}

// findEmbed returns the index of the embed whose response key is name, or -1.
func findEmbed(embeds []Embed, name string) int {
	for i := range embeds {
		if embeds[i].OutKey == name {
			return i
		}
	}
	return -1
}

// ParseWrite parses a POST/PATCH/PUT/DELETE request into a write Query. kind is
// the mutation the router chose from the method; contentType selects the body
// parser (JSON, CSV, or form-urlencoded; see spec 17) and body is the raw
// request body. The filter tree from the query string becomes the WHERE for
// update and delete; the select list is the returning projection. A resolution
// preference or an on_conflict target promotes an insert to an upsert. All
// errors are PGRST1xx (*pgerr.APIError). See spec 11-writes and 17-content-negotiation.
func ParseWrite(kind QueryKind, relation, rawQuery string, preferHeaders []string, contentType string, body []byte) (*Query, *pgerr.APIError) {
	vals, err := url.ParseQuery(rawQuery)
	if err != nil {
		return nil, pgerr.ErrParse("could not parse query string")
	}
	q := &Query{Kind: kind, Relation: Ref{Name: relation}}
	q.Prefer = ParsePrefer(preferHeaders)
	if q.Prefer.Count != nil {
		q.Count = *q.Prefer.Count
	}
	if perr := parseQueryString(q, vals); perr != nil {
		return nil, perr
	}

	w := &WriteSpec{}
	if q.Prefer.Return != nil {
		w.Return = *q.Prefer.Return
	}
	if q.Prefer.Missing != nil {
		w.Missing = *q.Prefer.Missing
	}
	if q.Prefer.Tx != nil {
		w.Tx = *q.Prefer.Tx
	}

	// An on_conflict target or a resolution preference makes this an upsert; PUT
	// is always an upsert. The conflict target defaults to the primary key,
	// which the planner fills in.
	onConflict := vals.Get("on_conflict")
	if kind == Upsert || onConflict != "" || q.Prefer.Resolution != nil {
		q.Kind = Upsert
		c := &Conflict{}
		if onConflict != "" {
			c.Target = splitComma(onConflict)
		}
		if q.Prefer.Resolution != nil {
			c.Resolution = *q.Prefer.Resolution
		}
		w.Conflict = c
	}

	switch q.Kind {
	case Insert, Upsert:
		objs, header, perr := decodeBodyObjects(contentType, body)
		if perr != nil {
			return nil, perr
		}
		w.Rows, w.Columns = buildInsert(objs, vals.Get("columns"), header)
	case Update:
		obj, perr := decodeBodyObject(contentType, body)
		if perr != nil {
			return nil, perr
		}
		set := make(map[string]Value, len(obj))
		for k, v := range obj {
			set[k] = Value{JSON: v}
		}
		w.Set = set
	case Delete:
		// A delete carries no body; its scope is the WHERE tree.
	}

	q.Write = w
	return q, nil
}

// callReserved are the query-string keys that post-filter an RPC result rather
// than name a function argument. On a GET call every other key is an argument.
var callReserved = map[string]bool{
	"select": true, "order": true, "limit": true, "offset": true,
}

// ParseCall parses a /rpc/<fn> request into a Call. On GET the arguments come
// from the query string (each non-reserved key is one argument, as text) and the
// reserved keys post-filter the result; on POST the JSON body carries the
// arguments (with their JSON types) and the whole query string post-filters. The
// planner resolves the function and checks volatility against the method. All
// errors are PGRST1xx. See spec 12-rpc.
func ParseCall(fn, rawQuery string, preferHeaders []string, isGet bool, contentType string, body []byte) (*Call, *pgerr.APIError) {
	vals, err := url.ParseQuery(rawQuery)
	if err != nil {
		return nil, pgerr.ErrParse("could not parse query string")
	}
	c := &Call{Function: Ref{Name: fn}}
	c.Prefer = ParsePrefer(preferHeaders)
	if c.Prefer.Count != nil {
		c.Count = *c.Prefer.Count
	}

	// Post-filters are parsed into a throwaway Query so the read-path parsers
	// (select, order, window, filter tree) are reused verbatim.
	pq := &Query{Kind: Read}
	args := map[string]Value{}

	if isGet {
		post := url.Values{}
		for k, vs := range vals {
			if callReserved[k] {
				post[k] = vs
				continue
			}
			// A function argument; the last value wins, matching url.Values.Get.
			args[k] = Value{Text: vs[len(vs)-1]}
		}
		if perr := parseQueryString(pq, post); perr != nil {
			return nil, perr
		}
	} else {
		if perr := parseQueryString(pq, vals); perr != nil {
			return nil, perr
		}
		if len(body) > 0 {
			obj, perr := decodeBodyObject(contentType, body)
			if perr != nil {
				return nil, perr
			}
			for k, v := range obj {
				args[k] = Value{JSON: v}
			}
		}
	}

	c.Select, c.Where, c.Order, c.Limit, c.Offset = pq.Select, pq.Where, pq.Order, pq.Limit, pq.Offset
	c.Args = args
	return c, nil
}

// writeFormat is the request body encoding selected by Content-Type (spec 17).
type writeFormat int

const (
	fmtJSON writeFormat = iota
	fmtCSV
	fmtForm
	fmtUnknown
)

// bodyFormat classifies a Content-Type into a write body parser. An empty
// Content-Type defaults to JSON, matching PostgREST.
func bodyFormat(contentType string) writeFormat {
	ct := strings.ToLower(strings.TrimSpace(contentType))
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = strings.TrimSpace(ct[:i])
	}
	switch ct {
	case "", "application/json":
		return fmtJSON
	case "text/csv":
		return fmtCSV
	case "application/x-www-form-urlencoded":
		return fmtForm
	default:
		return fmtUnknown
	}
}

// decodeBodyObjects decodes an insert/upsert body into a list of row objects.
// For CSV it also returns the header columns in their declared order, which
// fixes the write column order; JSON and form bodies return a nil header and the
// column order is derived in buildInsert.
func decodeBodyObjects(contentType string, body []byte) ([]map[string]any, []string, *pgerr.APIError) {
	switch bodyFormat(contentType) {
	case fmtJSON:
		objs, perr := decodeJSONObjects(body)
		return objs, nil, perr
	case fmtCSV:
		return decodeCSVObjects(body)
	case fmtForm:
		obj, perr := decodeFormObject(body)
		if perr != nil {
			return nil, nil, perr
		}
		return []map[string]any{obj}, nil, nil
	default:
		return nil, nil, pgerr.ErrUnsupportedMediaType(contentType)
	}
}

// decodeBodyObject decodes an update body into a single object of column
// assignments. CSV is not a meaningful patch format and is rejected.
func decodeBodyObject(contentType string, body []byte) (map[string]any, *pgerr.APIError) {
	switch bodyFormat(contentType) {
	case fmtJSON:
		dec := json.NewDecoder(bytes.NewReader(body))
		dec.UseNumber()
		var obj map[string]any
		if err := dec.Decode(&obj); err != nil {
			return nil, pgerr.ErrParse("update body must be a JSON object")
		}
		return obj, nil
	case fmtForm:
		return decodeFormObject(body)
	default:
		return nil, pgerr.ErrUnsupportedMediaType(contentType)
	}
}

// decodeJSONObjects decodes a single object or an array of objects, with numbers
// kept as json.Number so integer keys round-trip without float widening.
func decodeJSONObjects(body []byte) ([]map[string]any, *pgerr.APIError) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var raw any
	if err := dec.Decode(&raw); err != nil {
		return nil, pgerr.ErrParse("request body is not valid JSON")
	}
	switch v := raw.(type) {
	case map[string]any:
		return []map[string]any{v}, nil
	case []any:
		objs := make([]map[string]any, 0, len(v))
		for _, e := range v {
			obj, ok := e.(map[string]any)
			if !ok {
				return nil, pgerr.ErrParse("insert array must contain objects")
			}
			objs = append(objs, obj)
		}
		return objs, nil
	default:
		return nil, pgerr.ErrParse("insert body must be an object or an array of objects")
	}
}

// decodeCSVObjects parses an RFC 4180 body into row objects keyed by the header
// row. An empty field decodes to SQL NULL, matching PostgREST's default CSV null
// handling (Go's csv reader does not distinguish a quoted empty string from an
// unquoted empty field, so both map to null).
func decodeCSVObjects(body []byte) ([]map[string]any, []string, *pgerr.APIError) {
	r := csv.NewReader(bytes.NewReader(body))
	recs, err := r.ReadAll()
	if err != nil {
		return nil, nil, pgerr.ErrParse("malformed CSV body")
	}
	if len(recs) == 0 {
		return nil, nil, pgerr.ErrParse("CSV body has no header row")
	}
	header := recs[0]
	objs := make([]map[string]any, 0, len(recs)-1)
	for _, rec := range recs[1:] {
		obj := make(map[string]any, len(header))
		for i, h := range header {
			if i < len(rec) && rec[i] != "" {
				obj[h] = rec[i]
			} else {
				obj[h] = nil
			}
		}
		objs = append(objs, obj)
	}
	return objs, header, nil
}

// decodeFormObject parses an application/x-www-form-urlencoded body into one row
// object; each field's first value becomes a string column.
func decodeFormObject(body []byte) (map[string]any, *pgerr.APIError) {
	vals, err := url.ParseQuery(string(body))
	if err != nil {
		return nil, pgerr.ErrParse("malformed form body")
	}
	obj := make(map[string]any, len(vals))
	for k, v := range vals {
		if len(v) > 0 {
			obj[k] = v[0]
		}
	}
	return obj, nil
}

// buildInsert turns decoded objects into write rows and resolves the column set.
// The column order is the explicit columns= parameter when present, else the CSV
// header order, else the sorted keys of the first row (matching PostgREST: later
// rows' extra keys are ignored and missing keys take the missing= behavior).
func buildInsert(objs []map[string]any, columnsParam string, header []string) ([]map[string]Value, []string) {
	rows := make([]map[string]Value, len(objs))
	for i, obj := range objs {
		row := make(map[string]Value, len(obj))
		for k, val := range obj {
			row[k] = Value{JSON: val}
		}
		rows[i] = row
	}

	var cols []string
	switch {
	case columnsParam != "":
		cols = splitComma(columnsParam)
	case header != nil:
		cols = header
	case len(objs) > 0:
		for k := range objs[0] {
			cols = append(cols, k)
		}
		sort.Strings(cols)
	}
	return rows, cols
}

// splitComma splits a comma-separated parameter, trimming each element and
// dropping empties.
func splitComma(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// parseSelect parses the comma-separated select list at the top level. An item
// containing a parenthesis is an embed (rel(...)); plain items are columns,
// optionally alias:col::cast. "*" selects all columns.
func parseSelect(s string) ([]SelectItem, []Embed, *pgerr.APIError) {
	parts, err := splitTopLevel(s, ',')
	if err != nil {
		return nil, nil, pgerr.ErrParse("malformed select list")
	}
	var items []SelectItem
	var embeds []Embed
	for _, raw := range parts {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			return nil, nil, pgerr.ErrParse("empty item in select list")
		}
		if i := strings.IndexByte(raw, '('); i >= 0 {
			emb, perr := parseEmbed(raw, i)
			if perr != nil {
				return nil, nil, perr
			}
			items = append(items, EmbedRef{Index: len(embeds)})
			embeds = append(embeds, emb)
			continue
		}
		col, perr := parseColumnItem(raw)
		if perr != nil {
			return nil, nil, perr
		}
		items = append(items, col)
	}
	return items, embeds, nil
}

// parseEmbed parses rel(...) including an optional alias and hint. The inner
// select is parsed recursively so the IR is complete; the planner resolves the
// relationship.
func parseEmbed(raw string, lparen int) (Embed, *pgerr.APIError) {
	if raw[len(raw)-1] != ')' {
		return Embed{}, pgerr.ErrParse("unterminated embed in select list")
	}
	head := raw[:lparen]
	inner := raw[lparen+1 : len(raw)-1]

	emb := Embed{Join: JoinLeft}
	// alias:rel!hint, the alias naming the response key.
	if c := strings.IndexByte(head, ':'); c >= 0 {
		emb.Alias = head[:c]
		head = head[c+1:]
	}
	if b := strings.IndexByte(head, '!'); b >= 0 {
		hint := head[b+1:]
		head = head[:b]
		switch hint {
		case "inner":
			emb.Join = JoinInner
		case "left":
			emb.Join = JoinLeft
		default:
			emb.Hint = hint
		}
	}
	if strings.HasPrefix(head, "...") {
		emb.Spread = true
		head = strings.TrimPrefix(head, "...")
	}
	emb.Target = Ref{Name: head}
	emb.Query.Relation = Ref{Name: head}
	emb.Query.Kind = Read
	// The response key is the alias when given, else the relation name; it is also
	// the prefix that routes embed-scoped query params (films?actors.order=...).
	emb.OutKey = head
	if emb.Alias != "" {
		emb.OutKey = emb.Alias
	}
	if inner != "" {
		items, nested, perr := parseSelect(inner)
		if perr != nil {
			return Embed{}, perr
		}
		emb.Query.Select = items
		emb.Query.Embeds = nested
	}
	return emb, nil
}

// parseColumnItem parses alias:path::cast into a Column. path may carry JSON
// arrow hops (-> / ->>).
func parseColumnItem(raw string) (Column, *pgerr.APIError) {
	var col Column
	// cast: trailing ::type
	if i := strings.LastIndex(raw, "::"); i >= 0 {
		col.Cast = raw[i+2:]
		raw = raw[:i]
		if col.Cast == "" {
			return Column{}, pgerr.ErrParse("empty cast target")
		}
	}
	// alias: leading name before a single ':' (not '::', already stripped)
	if i := strings.IndexByte(raw, ':'); i >= 0 {
		col.Alias = raw[:i]
		raw = raw[i+1:]
	}
	path, last, perr := parsePath(raw)
	if perr != nil {
		return Column{}, perr
	}
	col.Path = path
	col.Last = last
	return col, nil
}

// parsePath splits a column reference with optional JSON arrows into hops.
// e.g. data->a->>b => {"data","a","b"} with Last=JSONArrow2.
func parsePath(raw string) ([]string, JSONStep, *pgerr.APIError) {
	if raw == "" {
		return nil, JSONNone, pgerr.ErrParse("empty column reference")
	}
	last := JSONNone
	// normalize ->> and -> into a delimiter sweep
	var hops []string
	rest := raw
	for {
		i2 := strings.Index(rest, "->>")
		i1 := strings.Index(rest, "->")
		switch {
		case i2 >= 0 && (i1 == -1 || i2 <= i1):
			hops = append(hops, rest[:i2])
			rest = rest[i2+3:]
			last = JSONArrow2
		case i1 >= 0:
			hops = append(hops, rest[:i1])
			rest = rest[i1+2:]
			last = JSONArrow
		default:
			hops = append(hops, rest)
			rest = ""
		}
		if rest == "" {
			break
		}
	}
	if slices.Contains(hops, "") {
		return nil, JSONNone, pgerr.ErrParse("empty hop in column path")
	}
	if len(hops) == 1 {
		last = JSONNone
	}
	return hops, last, nil
}

// parseOrder parses the order list: comma-separated path[.asc|.desc][.nullsfirst|.nullslast].
func parseOrder(s string) ([]OrderTerm, *pgerr.APIError) {
	parts := strings.Split(s, ",")
	terms := make([]OrderTerm, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			return nil, pgerr.ErrParse("empty order term")
		}
		segs := strings.Split(p, ".")
		var t OrderTerm
		path, _, perr := parsePath(segs[0])
		if perr != nil {
			return nil, perr
		}
		t.Path = path
		for _, mod := range segs[1:] {
			switch mod {
			case "asc":
				t.Desc = false
			case "desc":
				t.Desc = true
			case "nullsfirst":
				v := true
				t.NullsFirst = &v
			case "nullslast":
				v := false
				t.NullsFirst = &v
			default:
				return nil, pgerr.ErrParse("unknown order modifier: " + mod)
			}
		}
		terms = append(terms, t)
	}
	return terms, nil
}

// parseFilters builds the top-level filter tree from column filters plus and=/or=.
func parseFilters(vals url.Values) (*Cond, *pgerr.APIError) {
	var kids []Cond
	// deterministic key order for stable output
	keys := make([]string, 0, len(vals))
	for k := range vals {
		keys = append(keys, k)
	}
	sortStrings(keys)
	for _, key := range keys {
		switch key {
		case "and", "or":
			for _, v := range vals[key] {
				node, perr := parseLogical(key, v)
				if perr != nil {
					return nil, perr
				}
				kids = append(kids, node)
			}
			continue
		}
		if reservedKeys[key] {
			continue
		}
		path, _, perr := parsePath(key)
		if perr != nil {
			return nil, perr
		}
		for _, v := range vals[key] {
			cmp, perr := parseCompare(path, v)
			if perr != nil {
				return nil, perr
			}
			kids = append(kids, cmp)
		}
	}
	if len(kids) == 0 {
		return nil, nil
	}
	if len(kids) == 1 {
		return &kids[0], nil
	}
	var c Cond = And{Kids: kids}
	return &c, nil
}

// parseLogical parses an and=(...) / or=(...) value into a tree node.
func parseLogical(op, raw string) (Cond, *pgerr.APIError) {
	negate := false
	if strings.HasPrefix(raw, "not.") {
		negate = true
		raw = strings.TrimPrefix(raw, "not.")
	}
	raw = strings.TrimSpace(raw)
	if len(raw) < 2 || raw[0] != '(' || raw[len(raw)-1] != ')' {
		return nil, pgerr.ErrParse("malformed logical operator: " + op)
	}
	inner := raw[1 : len(raw)-1]
	parts, err := splitTopLevel(inner, ',')
	if err != nil {
		return nil, pgerr.ErrParse("malformed logical operator: " + op)
	}
	var kids []Cond
	for _, p := range parts {
		p = strings.TrimSpace(p)
		// nested logical: and(...) / or(...)
		if strings.HasPrefix(p, "and(") || strings.HasPrefix(p, "or(") ||
			strings.HasPrefix(p, "not.and(") || strings.HasPrefix(p, "not.or(") {
			subOp := "and"
			if strings.HasPrefix(p, "or(") || strings.HasPrefix(p, "not.or(") {
				subOp = "or"
			}
			i := strings.IndexByte(p, '(')
			node, perr := parseLogical(subOp, p[i:])
			if perr != nil {
				return nil, perr
			}
			if strings.HasPrefix(p, "not.") {
				node = Not{Kid: node}
			}
			kids = append(kids, node)
			continue
		}
		// column.op.value
		col, rest, ok := strings.Cut(p, ".")
		if !ok {
			return nil, pgerr.ErrParse("malformed predicate in logical: " + p)
		}
		path, _, perr := parsePath(col)
		if perr != nil {
			return nil, perr
		}
		cmp, perr := parseCompare(path, rest)
		if perr != nil {
			return nil, perr
		}
		kids = append(kids, cmp)
	}
	var node Cond
	if op == "or" {
		node = Or{Kids: kids}
	} else {
		node = And{Kids: kids}
	}
	if negate {
		node = Not{Kid: node}
	}
	return node, nil
}

// parseCompare parses a "operator.operand" filter value against a column path.
func parseCompare(path []string, raw string) (Compare, *pgerr.APIError) {
	c := Compare{Path: path}
	if strings.HasPrefix(raw, "not.") {
		c.Negate = true
		raw = strings.TrimPrefix(raw, "not.")
	}
	opTok, operand, ok := strings.Cut(raw, ".")
	if !ok {
		return Compare{}, pgerr.ErrParse("filter must be operator.value: " + raw)
	}
	// quantifier: op(any) / op(all)
	if i := strings.IndexByte(opTok, '('); i >= 0 {
		if !strings.HasSuffix(opTok, ")") {
			return Compare{}, pgerr.ErrParse("malformed quantifier in operator: " + opTok)
		}
		q := opTok[i+1 : len(opTok)-1]
		switch q {
		case "any":
			c.Quant = QAny
		case "all":
			c.Quant = QAll
		default:
			return Compare{}, pgerr.ErrParse("unknown quantifier: " + q)
		}
		opTok = opTok[:i]
	}
	op, ok := opFromToken(opTok)
	if !ok {
		return Compare{}, pgerr.ErrParse("unknown operator: " + opTok)
	}
	c.Op = op
	switch op {
	case OpIn:
		list, perr := parseInList(operand)
		if perr != nil {
			return Compare{}, perr
		}
		c.Value = Value{List: list}
	case OpIs:
		switch operand {
		case "null", "true", "false", "unknown", "not_null":
			c.Value = Value{Text: operand}
		default:
			return Compare{}, pgerr.ErrParse("is. expects null|true|false|unknown|not_null")
		}
	default:
		c.Value = Value{Text: operand}
	}
	return c, nil
}

// parseInList parses (a,b,"c,d") into a slice, honoring double-quoted elements.
func parseInList(raw string) ([]string, *pgerr.APIError) {
	raw = strings.TrimSpace(raw)
	if len(raw) < 2 || raw[0] != '(' || raw[len(raw)-1] != ')' {
		return nil, pgerr.ErrParse("in. expects a parenthesized list")
	}
	inner := raw[1 : len(raw)-1]
	if inner == "" {
		return []string{}, nil
	}
	parts, err := splitTopLevel(inner, ',')
	if err != nil {
		return nil, pgerr.ErrParse("malformed in. list")
	}
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if len(p) >= 2 && p[0] == '"' && p[len(p)-1] == '"' {
			p = p[1 : len(p)-1]
		}
		out = append(out, p)
	}
	return out, nil
}

// opFromToken maps a query-string operator token to an Op.
func opFromToken(tok string) (Op, bool) {
	switch tok {
	case "eq":
		return OpEq, true
	case "neq":
		return OpNeq, true
	case "gt":
		return OpGt, true
	case "gte":
		return OpGte, true
	case "lt":
		return OpLt, true
	case "lte":
		return OpLte, true
	case "like":
		return OpLike, true
	case "ilike":
		return OpILike, true
	case "match":
		return OpMatch, true
	case "imatch":
		return OpIMatch, true
	case "in":
		return OpIn, true
	case "is":
		return OpIs, true
	case "isdistinct":
		return OpIsDistinct, true
	case "fts", "plfts", "phfts", "wfts":
		return OpFTS, true
	case "cs":
		return OpContains, true
	case "cd":
		return OpContained, true
	case "ov":
		return OpOverlap, true
	case "sl":
		return OpRangeSL, true
	case "sr":
		return OpRangeSR, true
	case "nxr":
		return OpRangeNXR, true
	case "nxl":
		return OpRangeNXL, true
	case "adj":
		return OpRangeAdj, true
	}
	return 0, false
}

// splitTopLevel splits s on sep, ignoring sep inside () and "".
func splitTopLevel(s string, sep byte) ([]string, error) {
	var out []string
	depth := 0
	inQuote := false
	start := 0
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c == '"':
			inQuote = !inQuote
		case inQuote:
			// skip
		case c == '(':
			depth++
		case c == ')':
			depth--
		case c == sep && depth == 0:
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	out = append(out, s[start:])
	return out, nil
}

// sortStrings sorts in place (small slices; avoids importing sort everywhere).
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}
