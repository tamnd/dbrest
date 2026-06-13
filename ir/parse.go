package ir

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"errors"
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
	// An omitted select defaults to all columns; an explicitly empty select= is a
	// parse error, matching PostgREST (item 01.5).
	if vals.Has("select") {
		sel := vals.Get("select")
		if sel == "" {
			return pgerr.ErrParse("\"failed to parse select parameter ()\" (line 1, column 1)")
		}
		items, embeds, perr := parseSelect(sel, false)
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
		if head, rest, ok := cutIdentAware(key, '.'); ok {
			if idx := findEmbed(q.Embeds, head); idx >= 0 {
				ev := scoped[head]
				if ev == nil {
					ev = url.Values{}
					scoped[head] = ev
				}
				ev[rest] = vs
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
	// PUT is the only method the router maps to Upsert; capture it before the
	// promotion below can also turn a POST into an upsert.
	q := &Query{Kind: kind, Relation: Ref{Name: relation}, IsPut: kind == Upsert}
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

	// PostgREST performs an upsert only for PUT or for a POST carrying a
	// Prefer: resolution= preference. on_conflict alone leaves a POST a plain
	// insert (a duplicate key then fails with 409), and both on_conflict and
	// resolution are ignored entirely for PATCH and DELETE. The conflict target
	// defaults to the primary key, which the planner fills in.
	onConflict := vals.Get("on_conflict")
	if q.IsPut || (kind == Insert && q.Prefer.Resolution != nil) {
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
		// Without an explicit columns= override, PostgREST requires every object
		// in a bulk JSON array to carry exactly the first object's keys; columns=
		// switches to RawJSON semantics and skips the check (item 01.15).
		if vals.Get("columns") == "" && bodyFormat(contentType) == fmtJSON {
			if perr := checkUniformKeys(objs); perr != nil {
				return nil, perr
			}
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
		raw := url.Values{}
		for k, vs := range vals {
			if callReserved[k] {
				post[k] = vs
				continue
			}
			// A candidate argument: the last value wins for the argument binding
			// (matching url.Values.Get), but every value is retained on RawGet so the
			// planner can re-read a key that turns out to be a filter, or collect a
			// variadic parameter's repeats, once the signature is known.
			raw[k] = vs
			args[k] = Value{Text: vs[len(vs)-1]}
		}
		c.RawGet = raw
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

// PartitionGetArgs splits a GET /rpc call's query parameters into function
// arguments and post-filters once the resolved function's parameter names are
// known. A key naming a declared parameter stays an argument; every other key is
// re-read through the filter grammar and merged into the call's WHERE, matching
// how PostgREST treats a query key that does not name a parameter as a filter on
// a table-valued result. It is a no-op on a POST call, where the body carries the
// arguments and the query string already post-filtered.
func (c *Call) PartitionGetArgs(isParam func(string) bool, isVariadic func(string) bool) *pgerr.APIError {
	if c.RawGet == nil {
		return nil
	}
	filters := url.Values{}
	for k, vs := range c.RawGet {
		if isParam(k) {
			// A variadic parameter collects every repeat of its key as a list; a
			// scalar parameter already took the last value in ParseCall.
			if isVariadic(k) {
				c.Args[k] = Value{List: append([]string(nil), vs...)}
			}
			continue
		}
		delete(c.Args, k)
		filters[k] = vs
	}
	if len(filters) == 0 {
		return nil
	}
	cond, perr := parseFilters(filters)
	if perr != nil {
		return perr
	}
	if cond == nil {
		return nil
	}
	if c.Where == nil {
		c.Where = cond
		return nil
	}
	merged := Cond(And{Kids: []Cond{*c.Where, *cond}})
	c.Where = &merged
	return nil
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
// assignments. PostgREST accepts CSV for PATCH as well as POST, so a single-row
// CSV body is decoded with the same NULL rule the insert path uses.
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
	case fmtCSV:
		objs, _, perr := decodeCSVObjects(body)
		if perr != nil {
			return nil, perr
		}
		if len(objs) != 1 {
			return nil, pgerr.ErrInvalidBody("CSV update body must have exactly one data row")
		}
		return objs[0], nil
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
// row, with PostgREST's CSV semantics: the unquoted literal string NULL becomes
// SQL null and every other field, including an empty cell, becomes a string (an
// empty cell inserts an empty string). Go's csv reader enforces a uniform field
// count against the header, so a ragged row surfaces as PGRST102 "All lines must
// have same number of fields".
func decodeCSVObjects(body []byte) ([]map[string]any, []string, *pgerr.APIError) {
	r := csv.NewReader(bytes.NewReader(body))
	recs, err := r.ReadAll()
	if err != nil {
		var pe *csv.ParseError
		if errors.As(err, &pe) && pe.Err == csv.ErrFieldCount {
			return nil, nil, pgerr.ErrInvalidBody("All lines must have same number of fields")
		}
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
			switch {
			case i >= len(rec):
				obj[h] = nil
			case rec[i] == "NULL":
				obj[h] = nil
			default:
				obj[h] = rec[i]
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

// checkUniformKeys enforces PostgREST's rule that every object in a bulk insert
// shares the first object's exact key set; a mismatch is PGRST102 "All object
// keys must match". A single object (or none) is trivially uniform. The columns=
// parameter overrides the rule, so the caller skips this when it is present.
func checkUniformKeys(objs []map[string]any) *pgerr.APIError {
	if len(objs) < 2 {
		return nil
	}
	first := objs[0]
	for _, obj := range objs[1:] {
		if len(obj) != len(first) {
			return pgerr.ErrInvalidBody("All object keys must match")
		}
		for k := range first {
			if _, ok := obj[k]; !ok {
				return pgerr.ErrInvalidBody("All object keys must match")
			}
		}
	}
	return nil
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
		raw := splitComma(columnsParam)
		cols = make([]string, len(raw))
		for i, c := range raw {
			if len(c) >= 2 && c[0] == '"' && c[len(c)-1] == '"' {
				c = c[1 : len(c)-1]
			}
			cols[i] = c
		}
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
func parseSelect(s string, nested bool) ([]SelectItem, []Embed, *pgerr.APIError) {
	// PostgREST treats a bare "*" as "all columns" — equivalent to omitting
	// the select parameter entirely. We normalise it to an empty list here so
	// the planner and compiler see no explicit projection.
	if strings.TrimSpace(s) == "*" {
		return nil, nil, nil
	}
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
			// An item with empty parens is an aggregate (count(), amount.sum());
			// anything else is an embedded resource. The aggregate functions are a
			// closed set, so a name(...) that is not one falls through to the embed.
			if agg, ok, perr := parseAggregate(raw); perr != nil {
				return nil, nil, perr
			} else if ok {
				items = append(items, agg)
				continue
			}
			emb, perr := parseEmbed(raw, i)
			if perr != nil {
				return nil, nil, perr
			}
			items = append(items, EmbedRef{Index: len(embeds)})
			embeds = append(embeds, emb)
			continue
		}
		// Inside an embed select, a bare "count" is the legacy virtual aggregate that
		// maps to count(*) in the JSON output; it predates the count() form and is
		// exempt from the db-aggregates-enabled gate. At the top level "count" is an
		// ordinary column reference (PostgREST v12+).
		if nested && raw == "count" {
			items = append(items, Aggregate{Func: AggCount, Legacy: true})
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

// aggFuncByName maps the PostgREST aggregate spellings to their IR function.
var aggFuncByName = map[string]AggFunc{
	"count": AggCount, "sum": AggSum, "avg": AggAvg, "min": AggMin, "max": AggMax,
}

// parseAggregate recognizes the aggregate forms count() and path.func(), each
// with an optional response-key alias, an optional input cast on the aggregated
// column, and an optional output cast on the result. It reports ok=false (no
// error) when raw is not an aggregate so the caller can treat it as an embed.
func parseAggregate(raw string) (Aggregate, bool, *pgerr.APIError) {
	// The function call is always empty parens. Their absence rules out an
	// aggregate immediately; a non-empty pair means an embedded resource.
	head, tail, found := strings.Cut(raw, "()")
	if !found {
		return Aggregate{}, false, nil
	}

	var agg Aggregate
	// Output cast trails the parens as ::type.
	if tail != "" {
		if !strings.HasPrefix(tail, "::") {
			return Aggregate{}, false, nil
		}
		agg.Cast = tail[2:]
		if agg.Cast == "" {
			return Aggregate{}, false, pgerr.ErrParse("empty cast target")
		}
	}
	// Strip a response-key alias: the leading name before a single ':' that is not
	// part of a '::' cast and not inside quotes.
	if alias, rest, ok := cutAliasAware(head); ok {
		agg.Alias = unquoteIdent(alias)
		head = rest
	}
	// The function name is the token after the last dot; no dot means the whole
	// head is the function, which is only valid for the no-argument count().
	fn := head
	argSpec := ""
	if dot := strings.LastIndexByte(head, '.'); dot >= 0 {
		fn = head[dot+1:]
		argSpec = head[:dot]
	}
	f, ok := aggFuncByName[fn]
	if !ok {
		// An unknown function name with empty parens is not an aggregate; let the
		// caller try it as an embed.
		return Aggregate{}, false, nil
	}
	agg.Func = f
	if argSpec == "" {
		if f != AggCount {
			return Aggregate{}, false, pgerr.ErrParse(fn + "() requires a column argument")
		}
		return agg, true, nil
	}
	arg, perr := parseColumnItem(argSpec)
	if perr != nil {
		return Aggregate{}, false, perr
	}
	agg.Arg = &arg
	return agg, true, nil
}

// cutAliasAware splits a select-item head on the alias colon: the first ':' that
// is a single colon (not part of a '::' cast) and lies outside double quotes. It
// returns ok=false when there is no such colon.
func cutAliasAware(s string) (alias, rest string, ok bool) {
	inQuote := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inQuote {
			if c == '\\' && i+1 < len(s) {
				i++
				continue
			}
			if c == '"' {
				inQuote = false
			}
			continue
		}
		switch c {
		case '"':
			inQuote = true
		case ':':
			if i+1 < len(s) && s[i+1] == ':' {
				i++ // skip the cast '::'
				continue
			}
			return s[:i], s[i+1:], true
		}
	}
	return "", s, false
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
	// A head may carry both a disambiguation hint and a join modifier, in either
	// order (rel!hint!inner or rel!inner!hint). Split on every `!`: the first
	// segment is the relation, each later one is "inner"/"left" (the join) or a
	// hint. Two hints are a grammar error.
	if strings.IndexByte(head, '!') >= 0 {
		segs := strings.Split(head, "!")
		head = segs[0]
		sawHint := false
		for _, seg := range segs[1:] {
			switch seg {
			case "inner":
				emb.Join = JoinInner
			case "left":
				emb.Join = JoinLeft
			default:
				if sawHint {
					return Embed{}, pgerr.ErrParse("embed carries more than one disambiguation hint")
				}
				emb.Hint = seg
				sawHint = true
			}
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
		items, nested, perr := parseSelect(inner, true)
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
	// alias: leading name before a single ':' (not '::', already stripped). The
	// split is quote-aware so an aliased or target name may itself contain a colon
	// when double-quoted (item 01.2).
	if alias, rest, ok := cutIdentAware(raw, ':'); ok {
		col.Alias = unquoteIdent(alias)
		raw = rest
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
	// Sweep ->> and -> into hops, but treat an arrow inside a double-quoted segment
	// as part of the identifier rather than a delimiter (item 01.2).
	var hops []string
	start := 0
	inQuote := false
	for i := 0; i < len(raw); {
		c := raw[i]
		if inQuote {
			if c == '\\' && i+1 < len(raw) {
				i += 2
				continue
			}
			if c == '"' {
				inQuote = false
			}
			i++
			continue
		}
		if c == '"' {
			inQuote = true
			i++
			continue
		}
		if c == '-' && i+1 < len(raw) && raw[i+1] == '>' {
			hops = append(hops, raw[start:i])
			if i+2 < len(raw) && raw[i+2] == '>' {
				last = JSONArrow2
				i += 3
			} else {
				last = JSONArrow
				i += 2
			}
			start = i
			continue
		}
		i++
	}
	hops = append(hops, raw[start:])
	for j := range hops {
		hops[j] = unquoteIdent(hops[j])
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
		// Peel the column quote-aware so a double-quoted name may contain a dot
		// before the modifier list is split (item 01.2).
		colPart, modPart, hasMods := cutIdentAware(p, '.')
		var t OrderTerm
		path, _, perr := parsePath(colPart)
		if perr != nil {
			return nil, perr
		}
		t.Path = path
		var mods []string
		if hasMods {
			mods = strings.Split(modPart, ".")
		}
		// PostgREST's grammar is column[.asc|.desc][.nullsfirst|.nullslast] in that
		// fixed order: at most one direction, then at most one nulls modifier, no
		// repeats and no direction after a nulls modifier (item 01.7).
		var sawDir, sawNulls bool
		for _, mod := range mods {
			switch mod {
			case "asc", "desc":
				if sawDir || sawNulls {
					return nil, pgerr.ErrParse("unexpected order modifier: " + mod)
				}
				sawDir = true
				t.Desc = mod == "desc"
			case "nullsfirst", "nullslast":
				if sawNulls {
					return nil, pgerr.ErrParse("unexpected order modifier: " + mod)
				}
				sawNulls = true
				v := mod == "nullsfirst"
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
		case "not.and", "not.or":
			subOp := strings.TrimPrefix(key, "not.")
			for _, v := range vals[key] {
				node, perr := parseLogical(subOp, v)
				if perr != nil {
					return nil, perr
				}
				kids = append(kids, Not{Kid: node})
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
		// column.op.value, the column split quote-aware so a double-quoted name may
		// contain a dot (item 01.2).
		col, rest, ok := cutIdentAware(p, '.')
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
	// An operator may carry a parenthesized argument. For the full-text family it
	// is a language config (fts(english)); for the comparison operators it is a
	// quantifier (op(any)/op(all)). The base token before the paren selects which.
	base := opTok
	var paren string
	hasParen := false
	if i := strings.IndexByte(opTok, '('); i >= 0 {
		if !strings.HasSuffix(opTok, ")") {
			return Compare{}, pgerr.ErrParse("malformed argument in operator: " + opTok)
		}
		base = opTok[:i]
		paren = opTok[i+1 : len(opTok)-1]
		hasParen = true
	}
	// The full-text operators carry their variant and an optional language config;
	// they share one IR op and never take a quantifier.
	if variant, isFTS := ftsVariant(base); isFTS {
		c.Op = OpFTS
		c.FTS = variant
		c.Config = paren
		c.Value = Value{Text: operand}
		return c, nil
	}
	if hasParen {
		switch paren {
		case "any":
			c.Quant = QAny
		case "all":
			c.Quant = QAll
		default:
			return Compare{}, pgerr.ErrParse("unknown quantifier: " + paren)
		}
	}
	op, ok := opFromToken(base)
	if !ok {
		return Compare{}, pgerr.ErrParse("unknown operator: " + base)
	}
	c.Op = op
	// A quantifier applies to a braces list and is valid only for the operators
	// PostgREST allows it on; every element is parsed from the {…} literal, with
	// LIKE/ILIKE wildcards translated per element (item 01.1).
	if c.Quant != QNone {
		if !isQuantifiable(op) {
			return Compare{}, pgerr.ErrParse("quantifier any/all is not valid for operator: " + base)
		}
		list, perr := parseBraceList(operand)
		if perr != nil {
			return Compare{}, perr
		}
		if op == OpLike || op == OpILike {
			for i, p := range list {
				list[i] = strings.ReplaceAll(p, "*", "%")
			}
		}
		c.Value = Value{List: list}
		return c, nil
	}
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
	case OpLike, OpILike:
		// PostgREST maps * to % in LIKE/ILIKE patterns so URL-friendly wildcards work.
		c.Value = Value{Text: strings.ReplaceAll(operand, "*", "%")}
	default:
		c.Value = Value{Text: operand}
	}
	return c, nil
}

// isQuantifiable reports whether an operator accepts an any/all quantifier, the
// set PostgREST allows: eq, gt, gte, lt, lte, like, ilike, match, imatch.
func isQuantifiable(op Op) bool {
	switch op {
	case OpEq, OpGt, OpGte, OpLt, OpLte, OpLike, OpILike, OpMatch, OpIMatch:
		return true
	}
	return false
}

// parseInList parses (a,b,"c,d") into a slice, honoring double-quoted elements.
func parseInList(raw string) ([]string, *pgerr.APIError) {
	raw = strings.TrimSpace(raw)
	if len(raw) < 2 || raw[0] != '(' || raw[len(raw)-1] != ')' {
		return nil, pgerr.ErrParse("in. expects a parenthesized list")
	}
	inner := raw[1 : len(raw)-1]
	// PostgREST's grammar requires at least one element; ?id=in.() is a parse
	// error, not an empty match (item 01.3).
	if inner == "" {
		return nil, pgerr.ErrParse("in. expects at least one value")
	}
	parts, err := splitTopLevel(inner, ',')
	if err != nil {
		return nil, pgerr.ErrParse("malformed in. list")
	}
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if len(p) >= 2 && p[0] == '"' && p[len(p)-1] == '"' {
			// A quoted element may escape an interior quote as \" and a backslash as
			// \\ (item 01.2); strip the quotes and unescape.
			p = unescapeQuoted(p[1 : len(p)-1])
		}
		out = append(out, p)
	}
	return out, nil
}

// unescapeQuoted reverses the in-list quoting escapes: \" -> " and \\ -> \. Any
// other backslash sequence keeps the following character literally.
func unescapeQuoted(s string) string {
	if !strings.ContainsRune(s, '\\') {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			i++
			b.WriteByte(s[i])
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// parseBraceList parses a {a,b,"c,d"} array literal (PostgREST's quantified
// operand) into its elements, honoring double-quoted elements so a comma or
// reserved character can appear inside one. No wildcard translation is done here;
// a LIKE/ILIKE caller applies * → % afterward (items 01.1, 01.2).
func parseBraceList(raw string) ([]string, *pgerr.APIError) {
	raw = strings.TrimSpace(raw)
	if len(raw) < 2 || raw[0] != '{' || raw[len(raw)-1] != '}' {
		return nil, pgerr.ErrParse("any/all expects a {…} list")
	}
	inner := raw[1 : len(raw)-1]
	if inner == "" {
		return nil, pgerr.ErrParse("any/all list must have at least one value")
	}
	parts, err := splitTopLevel(inner, ',')
	if err != nil {
		return nil, pgerr.ErrParse("malformed any/all list")
	}
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if len(p) >= 2 && p[0] == '"' && p[len(p)-1] == '"' {
			p = unescapeQuoted(p[1 : len(p)-1])
		}
		out = append(out, p)
	}
	return out, nil
}

// ftsVariant maps a full-text operator token to its IR variant. The four tokens
// share the single OpFTS op and differ only in the query grammar a backend lowers
// them to (spec 21).
func ftsVariant(tok string) (FTSVariant, bool) {
	switch tok {
	case "fts":
		return FTSPlain, true
	case "plfts":
		return FTSPlainText, true
	case "phfts":
		return FTSPhrase, true
	case "wfts":
		return FTSWeb, true
	}
	return 0, false
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

// splitTopLevel splits s on sep, ignoring sep inside (), {}, and "". Inside a
// quoted span a backslash escapes the next byte, so an escaped quote does not end
// the span and an escaped separator is not a split point (items 01.1, 01.2).
func splitTopLevel(s string, sep byte) ([]string, error) {
	var out []string
	depth := 0
	inQuote := false
	start := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inQuote {
			switch {
			case c == '\\' && i+1 < len(s):
				i++ // skip the escaped byte
			case c == '"':
				inQuote = false
			}
			continue
		}
		switch c {
		case '"':
			inQuote = true
		case '(', '{':
			depth++
		case ')', '}':
			depth--
		case sep:
			if depth == 0 {
				out = append(out, s[start:i])
				start = i + 1
			}
		}
	}
	out = append(out, s[start:])
	return out, nil
}

// cutIdentAware splits s at the first sep byte that is not inside a double-quoted
// identifier segment, returning the text before and after it and whether one was
// found. A backslash inside quotes escapes the next byte. This lets a reserved
// character (dot, colon) sit inside a %22-quoted column or relation name without
// being treated as a delimiter (item 01.2).
func cutIdentAware(s string, sep byte) (before, after string, found bool) {
	inQuote := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inQuote {
			if c == '\\' && i+1 < len(s) {
				i++
				continue
			}
			if c == '"' {
				inQuote = false
			}
			continue
		}
		switch c {
		case '"':
			inQuote = true
		case sep:
			return s[:i], s[i+1:], true
		}
	}
	return s, "", false
}

// unquoteIdent strips one layer of surrounding double quotes from an identifier
// segment so a reserved character can appear in a column or relation name; an
// interior doubled quote ("") unescapes to a single quote, as in SQL. A segment
// that is not fully quoted is returned unchanged (item 01.2).
func unquoteIdent(s string) string {
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return strings.ReplaceAll(s[1:len(s)-1], `""`, `"`)
	}
	return s
}

// sortStrings sorts in place (small slices; avoids importing sort everywhere).
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}
