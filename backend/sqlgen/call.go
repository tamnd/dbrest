package sqlgen

import (
	"strings"

	"github.com/tamnd/dbrest/ir"
	"github.com/tamnd/dbrest/pgerr"
	"github.com/tamnd/dbrest/rpc"
)

// This file lowers an RPC call (spec 12) to SQL for a portable registry
// function. The function body is a parameterized statement whose :name
// placeholders bind to the call's arguments; the bound statement is the inner
// query. For a table return the inner query is wrapped in a SELECT that applies
// the post-filter projection, filters, ordering, and window, exactly as a table
// read would. A scalar or setof-scalar return runs the inner query directly and
// the renderer shapes its single column.

// CompileCall lowers a resolved RPC call to a parameterized statement. The
// function's SQL is rendered with its :name placeholders bound to the arguments
// (defaults filling omitted optional parameters); a placeholder that is not a
// declared parameter binds a reserved request-context value from ctxArgs (see
// ContextArgs); a table return additionally wraps the result so post-filters
// compile around it.
func CompileCall(d Dialect, c *ir.Call, fn *rpc.Function, ctxArgs map[string]any) (*Statement, *pgerr.APIError) {
	if fn.Query == nil || strings.TrimSpace(fn.Query.SQL) == "" {
		return nil, pgerr.ErrUnsupported("this function realization", "sql")
	}
	b := newBuilder(d)
	b.ctxArgs = ctxArgs

	inner, err := b.bindNamed(fn, c.Args)
	if err != nil {
		return nil, err
	}

	// Only a table return can be projected, filtered, ordered, and paginated; a
	// scalar or setof-scalar return is the function's value(s) verbatim.
	if fn.Returns.Kind != rpc.ReturnTable || !callHasPostFilter(c) {
		b.sb.WriteString(inner)
		return &Statement{SQL: b.sb.String(), Args: b.args}, nil
	}

	const alias = "_rpc"
	b.sb.WriteString("SELECT ")
	if err := b.writeSelect(c.Select); err != nil {
		return nil, err
	}
	b.sb.WriteString(" FROM (")
	b.sb.WriteString(inner)
	b.sb.WriteString(") ")
	b.sb.WriteString(alias)

	if c.Where != nil {
		b.sb.WriteString(" WHERE ")
		if err := b.writeCond(*c.Where); err != nil {
			return nil, err
		}
	}
	hasOrder := len(c.Order) > 0
	if hasOrder {
		if err := b.writeOrder(c.Order); err != nil {
			return nil, err
		}
	}
	if clause := b.d.LimitOffset(c.Limit, c.Offset, hasOrder); clause != "" {
		b.sb.WriteString(" ")
		b.sb.WriteString(clause)
	}
	return &Statement{SQL: b.sb.String(), Args: b.args}, nil
}

// CompileCallCount lowers the count of an RPC result: the bound function wrapped
// in a count over its rows, with a table return's WHERE post-filter applied (the
// select, order, and window do not change the count). It runs as a separate
// read-only statement for a count=exact request, exactly as a table read's count
// does. It is only valid for a read-only function; a volatile function must not
// run twice.
func CompileCallCount(d Dialect, c *ir.Call, fn *rpc.Function, ctxArgs map[string]any) (*Statement, *pgerr.APIError) {
	if fn.Query == nil || strings.TrimSpace(fn.Query.SQL) == "" {
		return nil, pgerr.ErrUnsupported("this function realization", "sql")
	}
	b := newBuilder(d)
	b.ctxArgs = ctxArgs
	inner, err := b.bindNamed(fn, c.Args)
	if err != nil {
		return nil, err
	}
	b.sb.WriteString("SELECT count(*) FROM (")
	b.sb.WriteString(inner)
	b.sb.WriteString(") _rpc")
	if fn.Returns.Kind == rpc.ReturnTable && c.Where != nil {
		b.sb.WriteString(" WHERE ")
		if err := b.writeCond(*c.Where); err != nil {
			return nil, err
		}
	}
	return &Statement{SQL: b.sb.String(), Args: b.args}, nil
}

// callHasPostFilter reports whether a call carries any clause that wraps the
// function result (a projection, a filter, an ordering, or a window).
func callHasPostFilter(c *ir.Call) bool {
	return len(c.Select) > 0 || c.Where != nil || len(c.Order) > 0 ||
		c.Limit != nil || c.Offset != nil
}

// bindNamed substitutes the :name placeholders in the function body with bound
// parameters, returning the rendered SQL. An omitted optional parameter binds
// its declared default; a single json/jsonb parameter that the body did not name
// receives the whole argument object (PostgREST's single-unnamed-argument form).
// A `::` sequence is left untouched so a cast is not mistaken for a placeholder.
func (b *builder) bindNamed(fn *rpc.Function, args map[string]ir.Value) (string, *pgerr.APIError) {
	args = singleObjectArgs(fn, args)
	sql := fn.Query.SQL
	var out strings.Builder
	for i := 0; i < len(sql); {
		if sql[i] == ':' && i+1 < len(sql) && sql[i+1] == ':' {
			out.WriteString("::") // a cast, not a placeholder
			i += 2
			continue
		}
		if sql[i] == ':' && i+1 < len(sql) && isIdentStart(sql[i+1]) {
			j := i + 1
			for j < len(sql) && isIdentChar(sql[j]) {
				j++
			}
			name := sql[i+1 : j]
			arg, perr := b.argValue(fn, name, args)
			if perr != nil {
				return "", perr
			}
			out.WriteString(arg)
			i = j
			continue
		}
		out.WriteByte(sql[i])
		i++
	}
	return out.String(), nil
}

// argValue binds the value for one named placeholder: the supplied argument when
// present, else the parameter's default. A GET argument arrives as text; a POST
// argument arrives as a decoded JSON value.
func (b *builder) argValue(fn *rpc.Function, name string, args map[string]ir.Value) (string, *pgerr.APIError) {
	p, ok := fn.Param(name)
	if !ok {
		// Not a declared parameter: a reserved request-context placeholder
		// binds the frontend-built value (spec 15: the emulated engines bind
		// context, they never read a session store). A declared parameter of
		// the same name takes this path only when undeclared, so it cannot be
		// shadowed away by a caller.
		if v, isCtx := b.ctxArgs[name]; isCtx {
			return b.bind(v), nil
		}
		return "", pgerr.ErrInternal("rpc body references undeclared parameter :" + name)
	}
	if v, ok := args[name]; ok {
		if p.Variadic {
			return b.bindVariadic(v), nil
		}
		return b.bind(callArg(b.d, v)), nil
	}
	if p.Variadic {
		// A variadic call with no trailing arguments expands to nothing, so a body
		// spelling the placeholder inside a call list (f(:nums)) becomes f().
		return "", nil
	}
	if p.Optional {
		return b.bind(p.Default), nil
	}
	// A required parameter with no argument cannot happen: Lookup only returns an
	// overload whose required parameters are all present. Guard anyway.
	return "", pgerr.ErrInternal("rpc call is missing required parameter :" + name)
}

// singleObjectArgs implements the single-unnamed-argument form: a function whose
// only parameter is a json/jsonb type receives the whole posted object when the
// body did not name that parameter. Otherwise the arguments are returned as-is.
func singleObjectArgs(fn *rpc.Function, args map[string]ir.Value) map[string]ir.Value {
	if len(fn.Params) != 1 || len(args) == 0 {
		return args
	}
	p := fn.Params[0]
	if !isJSONType(p.Type) {
		return args
	}
	if _, named := args[p.Name]; named {
		return args
	}
	obj := make(map[string]any, len(args))
	for k, v := range args {
		obj[k] = v.JSON
	}
	return map[string]ir.Value{p.Name: {JSON: obj}}
}

// callArg converts an argument value to a driver argument. A POST argument has a
// decoded JSON value (numbers preserved, objects/arrays re-encoded to text); a
// GET argument is the raw query-string text, bound verbatim. An empty query value
// is an empty string, not NULL: PostgREST passes "" to the parameter, and a NULL
// is expressed only by omitting the argument (which binds the parameter default).
func callArg(d Dialect, v ir.Value) any {
	if v.JSON != nil {
		return writeArg(d, v)
	}
	return v.Text
}

// bindVariadic expands a variadic argument into a comma-separated list of bound
// placeholders, one per collected element, so a body spelling the placeholder
// inside a call list or an IN (:name) clause receives every value. A GET call
// arrives as a text list; a POST call arrives as a decoded JSON array (a lone
// scalar is treated as a one-element list). An empty list binds nothing.
func (b *builder) bindVariadic(v ir.Value) string {
	elems := variadicElems(b.d, v)
	parts := make([]string, len(elems))
	for i, e := range elems {
		parts[i] = b.bind(e)
	}
	return strings.Join(parts, ", ")
}

// variadicElems flattens a variadic argument value into its driver elements. A
// GET text list maps each item verbatim; a POST JSON array maps each element
// through the write-value path (numbers preserved, nested documents re-encoded);
// any other single value is a one-element list.
func variadicElems(d Dialect, v ir.Value) []any {
	if v.List != nil {
		out := make([]any, len(v.List))
		for i, s := range v.List {
			out[i] = s
		}
		return out
	}
	if arr, ok := v.JSON.([]any); ok {
		out := make([]any, len(arr))
		for i, e := range arr {
			out[i] = writeArg(d, ir.Value{JSON: e})
		}
		return out
	}
	if v.JSON != nil {
		return []any{writeArg(d, v)}
	}
	if v.Text != "" {
		return []any{v.Text}
	}
	return nil
}

// isJSONType reports whether a canonical type name is a JSON family type.
func isJSONType(t string) bool {
	switch strings.ToLower(strings.TrimSpace(t)) {
	case "json", "jsonb":
		return true
	}
	return false
}

func isIdentStart(b byte) bool {
	return b == '_' || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

func isIdentChar(b byte) bool {
	return isIdentStart(b) || (b >= '0' && b <= '9')
}
