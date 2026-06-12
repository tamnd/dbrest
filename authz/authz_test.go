package authz_test

import (
	"net/http"
	"testing"

	"github.com/tamnd/dbrest/authz"
	"github.com/tamnd/dbrest/ir"
	"github.com/tamnd/dbrest/reqctx"
	"github.com/tamnd/dbrest/schema"
)

// filmsRel is the relation the tests gate against.
func filmsRel() *schema.Relation {
	r := &schema.Relation{
		Schema: "public",
		Name:   "films",
		Columns: []*schema.Column{
			{Name: "id", Type: "integer", Position: 1},
			{Name: "title", Type: "text", Position: 2},
			{Name: "owner", Type: "text", Position: 3},
			{Name: "secret", Type: "text", Position: 4},
		},
	}
	schema.NewModel([]*schema.Relation{r}) // indexes byName
	return r
}

// readPlan builds a Read plan over films with the given select items.
func readPlan(sel ...ir.SelectItem) *ir.Plan {
	rel := filmsRel()
	return &ir.Plan{
		Rel: rel,
		Query: &ir.Query{
			Kind:     ir.Read,
			Relation: ir.Ref{Schema: "public", Name: "films"},
			Select:   sel,
		},
	}
}

func col(name string) ir.Column { return ir.Column{Path: []string{name}} }

func star() ir.Column { return ir.Column{Path: []string{"*"}} }

func TestActionGrantedAllows(t *testing.T) {
	reg := authz.NewRegistry(
		[]authz.Grant{{Role: "web_user", Relation: "films", Action: authz.Select}},
		nil,
	)
	p := readPlan()
	if err := reg.Authorize(&reqctx.Context{Role: "web_user"}, p); err != nil {
		t.Fatalf("Authorize denied a granted read: %v", err)
	}
}

func TestActionDeniedForbidsAuthenticated(t *testing.T) {
	reg := authz.NewRegistry(nil, nil)
	p := readPlan()
	err := reg.Authorize(&reqctx.Context{Role: "web_user"}, p)
	if err == nil {
		t.Fatal("ungranted read was allowed")
	}
	if err.HTTPStatus != http.StatusForbidden {
		t.Errorf("status = %d, want 403", err.HTTPStatus)
	}
	if err.Code != "42501" {
		t.Errorf("code = %q, want 42501", err.Code)
	}
}

func TestActionDeniedForAnonIs401(t *testing.T) {
	reg := authz.NewRegistry(nil, nil)
	p := readPlan()
	err := reg.Authorize(&reqctx.Context{Role: "anon", Anonymous: true}, p)
	if err == nil {
		t.Fatal("ungranted anon read was allowed")
	}
	if err.HTTPStatus != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", err.HTTPStatus)
	}
}

func TestExplicitForbiddenColumnDenied(t *testing.T) {
	reg := authz.NewRegistry(
		[]authz.Grant{{Role: "web_user", Relation: "films", Action: authz.Select, Columns: []string{"id", "title"}}},
		nil,
	)
	p := readPlan(col("id"), col("secret"))
	err := reg.Authorize(&reqctx.Context{Role: "web_user"}, p)
	if err == nil {
		t.Fatal("projecting an ungranted column was allowed")
	}
	if err.HTTPStatus != http.StatusForbidden {
		t.Errorf("status = %d, want 403", err.HTTPStatus)
	}
}

// A star projection under a column-limited grant means SELECT every column,
// which the grant does not cover. PostgreSQL raises 42501 for that and the
// PostgREST maintainers rejected narrowing * to the granted set (issue #1732),
// so the request is denied; the client must name the granted columns.
func TestStarRejectedUnderColumnLimitedGrant(t *testing.T) {
	reg := authz.NewRegistry(
		[]authz.Grant{{Role: "web_user", Relation: "films", Action: authz.Select, Columns: []string{"id", "title"}}},
		nil,
	)
	p := readPlan(star())
	err := reg.Authorize(&reqctx.Context{Role: "web_user"}, p)
	if err == nil {
		t.Fatal("star projection under a column-limited grant was allowed")
	}
	if err.HTTPStatus != http.StatusForbidden {
		t.Errorf("status = %d, want 403", err.HTTPStatus)
	}
	if err.Code != "42501" {
		t.Errorf("code = %q, want 42501", err.Code)
	}
}

func TestEmptyProjectionRejectedUnderColumnLimitedGrant(t *testing.T) {
	reg := authz.NewRegistry(
		[]authz.Grant{{Role: "web_user", Relation: "films", Action: authz.Select, Columns: []string{"title"}}},
		nil,
	)
	p := readPlan() // no select items: whole-row projection, same as *
	err := reg.Authorize(&reqctx.Context{Role: "web_user"}, p)
	if err == nil {
		t.Fatal("whole-row projection under a column-limited grant was allowed")
	}
	if err.HTTPStatus != http.StatusForbidden {
		t.Errorf("status = %d, want 403", err.HTTPStatus)
	}
}

func TestStarRejectedForAnonIs401(t *testing.T) {
	reg := authz.NewRegistry(
		[]authz.Grant{{Role: "anon", Relation: "films", Action: authz.Select, Columns: []string{"id"}}},
		nil,
	)
	p := readPlan(star())
	err := reg.Authorize(&reqctx.Context{Role: "anon", Anonymous: true}, p)
	if err == nil {
		t.Fatal("anon star projection under a column-limited grant was allowed")
	}
	if err.HTTPStatus != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", err.HTTPStatus)
	}
}

func TestGrantedColumnsProjectFine(t *testing.T) {
	reg := authz.NewRegistry(
		[]authz.Grant{{Role: "web_user", Relation: "films", Action: authz.Select, Columns: []string{"id", "title"}}},
		nil,
	)
	p := readPlan(col("id"), col("title"))
	if err := reg.Authorize(&reqctx.Context{Role: "web_user"}, p); err != nil {
		t.Fatalf("Authorize denied a fully granted projection: %v", err)
	}
	if got := projectedNames(p.Query.Select); !equal(got, []string{"id", "title"}) {
		t.Errorf("projection = %v, want untouched [id title]", got)
	}
}

func TestFullGrantLeavesProjectionUntouched(t *testing.T) {
	reg := authz.NewRegistry(
		[]authz.Grant{{Role: "web_user", Relation: "films", Action: authz.Select}},
		nil,
	)
	p := readPlan(star())
	if err := reg.Authorize(&reqctx.Context{Role: "web_user"}, p); err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	if len(p.Query.Select) != 1 {
		t.Errorf("full grant should not narrow the star, got %d items", len(p.Query.Select))
	}
}

func TestUsingInjectedAndedAtTop(t *testing.T) {
	reg := authz.NewRegistry(
		[]authz.Grant{{Role: "web_user", Relation: "films", Action: authz.Select}},
		[]authz.Policy{{
			Relation: "films", Role: "web_user",
			Using: authz.Predicate{Terms: []authz.Term{{Column: "owner", Op: authz.OpEq, Claim: "sub"}}},
		}},
	)
	// A client filter the policy must sit above.
	clientFilter := ir.Cond(ir.Compare{Path: []string{"id"}, Op: ir.OpEq, Value: ir.Value{Text: "1"}})
	p := readPlan()
	p.Query.Where = &clientFilter

	rc := &reqctx.Context{Role: "web_user", Claims: map[string]any{"sub": "alice"}}
	if err := reg.Authorize(rc, p); err != nil {
		t.Fatalf("Authorize: %v", err)
	}

	top, ok := (*p.Query.Where).(ir.And)
	if !ok {
		t.Fatalf("top of filter tree is %T, want And", *p.Query.Where)
	}
	if len(top.Kids) != 2 {
		t.Fatalf("top And has %d kids, want 2 (client filter + policy)", len(top.Kids))
	}
	// The first kid is the client's original filter, preserved whole.
	if _, ok := top.Kids[0].(ir.Compare); !ok {
		t.Errorf("first kid = %T, want the client Compare", top.Kids[0])
	}
	pol, ok := top.Kids[1].(ir.Compare)
	if !ok {
		t.Fatalf("second kid = %T, want the policy Compare", top.Kids[1])
	}
	if pol.Op != ir.OpEq || pol.Value.Text != "alice" {
		t.Errorf("policy predicate = %+v, want owner = alice", pol)
	}
}

func TestMissingClaimDeniesEveryRow(t *testing.T) {
	reg := authz.NewRegistry(
		[]authz.Grant{{Role: "web_user", Relation: "films", Action: authz.Select}},
		[]authz.Policy{{
			Relation: "films", Role: "web_user",
			Using: authz.Predicate{Terms: []authz.Term{{Column: "owner", Op: authz.OpEq, Claim: "sub"}}},
		}},
	)
	p := readPlan()
	// No claims: the policy references a claim that is absent.
	if err := reg.Authorize(&reqctx.Context{Role: "web_user"}, p); err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	top := (*p.Query.Where).(ir.And)
	c := top.Kids[0].(ir.Compare)
	if c.Op != ir.OpIn || c.Value.List != nil {
		t.Errorf("missing-claim predicate = %+v, want always-false empty IN", c)
	}
}

func TestNoPolicyLeavesFilterUntouched(t *testing.T) {
	reg := authz.NewRegistry(
		[]authz.Grant{{Role: "web_user", Relation: "films", Action: authz.Select}},
		nil,
	)
	p := readPlan()
	if err := reg.Authorize(&reqctx.Context{Role: "web_user"}, p); err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	if p.Query.Where != nil {
		t.Errorf("no policy should leave Where nil, got %+v", *p.Query.Where)
	}
}

func TestWithCheckRejectsBadInsert(t *testing.T) {
	reg := authz.NewRegistry(
		[]authz.Grant{{Role: "web_user", Relation: "films", Action: authz.Insert}},
		[]authz.Policy{{
			Relation: "films", Role: "web_user",
			WithCheck: authz.Predicate{Terms: []authz.Term{{Column: "owner", Op: authz.OpEq, Claim: "sub"}}},
		}},
	)
	p := &ir.Plan{
		Rel: filmsRel(),
		Query: &ir.Query{
			Kind:     ir.Insert,
			Relation: ir.Ref{Schema: "public", Name: "films"},
			Write: &ir.WriteSpec{
				Columns: []string{"id", "owner"},
				Rows: []map[string]ir.Value{
					{"id": {Text: "1"}, "owner": {Text: "mallory"}},
				},
			},
		},
	}
	rc := &reqctx.Context{Role: "web_user", Claims: map[string]any{"sub": "alice"}}
	err := reg.Authorize(rc, p)
	if err == nil {
		t.Fatal("insert violating with_check was allowed")
	}
	if err.HTTPStatus != http.StatusForbidden {
		t.Errorf("status = %d, want 403", err.HTTPStatus)
	}
}

func TestWithCheckAllowsGoodInsert(t *testing.T) {
	reg := authz.NewRegistry(
		[]authz.Grant{{Role: "web_user", Relation: "films", Action: authz.Insert}},
		[]authz.Policy{{
			Relation: "films", Role: "web_user",
			WithCheck: authz.Predicate{Terms: []authz.Term{{Column: "owner", Op: authz.OpEq, Claim: "sub"}}},
		}},
	)
	p := &ir.Plan{
		Rel: filmsRel(),
		Query: &ir.Query{
			Kind:     ir.Insert,
			Relation: ir.Ref{Schema: "public", Name: "films"},
			Write: &ir.WriteSpec{
				Columns: []string{"id", "owner"},
				Rows: []map[string]ir.Value{
					{"id": {Text: "1"}, "owner": {Text: "alice"}},
				},
			},
		},
	}
	rc := &reqctx.Context{Role: "web_user", Claims: map[string]any{"sub": "alice"}}
	if err := reg.Authorize(rc, p); err != nil {
		t.Fatalf("Authorize denied a conforming insert: %v", err)
	}
}

func TestUpdateWithCheckSkipsUnsetColumns(t *testing.T) {
	reg := authz.NewRegistry(
		[]authz.Grant{
			{Role: "web_user", Relation: "films", Action: authz.Update},
			{Role: "web_user", Relation: "films", Action: authz.Select},
		},
		[]authz.Policy{{
			Relation: "films", Role: "web_user",
			WithCheck: authz.Predicate{Terms: []authz.Term{{Column: "owner", Op: authz.OpEq, Claim: "sub"}}},
		}},
	)
	// The update sets only title; owner is absent from Set, so with_check on
	// owner is not evaluated and the update is allowed.
	p := &ir.Plan{
		Rel: filmsRel(),
		Query: &ir.Query{
			Kind:     ir.Update,
			Relation: ir.Ref{Schema: "public", Name: "films"},
			Write:    &ir.WriteSpec{Set: map[string]ir.Value{"title": {Text: "x"}}},
		},
	}
	rc := &reqctx.Context{Role: "web_user", Claims: map[string]any{"sub": "alice"}}
	if err := reg.Authorize(rc, p); err != nil {
		t.Fatalf("Authorize denied an update that touches no policy column: %v", err)
	}
}

func TestUpdateWithCheckRejectsBadSet(t *testing.T) {
	reg := authz.NewRegistry(
		[]authz.Grant{
			{Role: "web_user", Relation: "films", Action: authz.Update},
			{Role: "web_user", Relation: "films", Action: authz.Select},
		},
		[]authz.Policy{{
			Relation: "films", Role: "web_user",
			WithCheck: authz.Predicate{Terms: []authz.Term{{Column: "owner", Op: authz.OpEq, Claim: "sub"}}},
		}},
	)
	p := &ir.Plan{
		Rel: filmsRel(),
		Query: &ir.Query{
			Kind:     ir.Update,
			Relation: ir.Ref{Schema: "public", Name: "films"},
			Write:    &ir.WriteSpec{Set: map[string]ir.Value{"owner": {Text: "mallory"}}},
		},
	}
	rc := &reqctx.Context{Role: "web_user", Claims: map[string]any{"sub": "alice"}}
	if err := reg.Authorize(rc, p); err == nil {
		t.Fatal("update that reassigns owner to a foreign value was allowed")
	}
}

func TestWriteColumnGateRejectsUngrantedColumn(t *testing.T) {
	reg := authz.NewRegistry(
		[]authz.Grant{{Role: "web_user", Relation: "films", Action: authz.Insert, Columns: []string{"id", "title"}}},
		nil,
	)
	p := &ir.Plan{
		Rel: filmsRel(),
		Query: &ir.Query{
			Kind:     ir.Insert,
			Relation: ir.Ref{Schema: "public", Name: "films"},
			Write: &ir.WriteSpec{
				Columns: []string{"id", "secret"},
				Rows:    []map[string]ir.Value{{"id": {Text: "1"}, "secret": {Text: "x"}}},
			},
		},
	}
	if err := reg.Authorize(&reqctx.Context{Role: "web_user"}, p); err == nil {
		t.Fatal("inserting an ungranted column was allowed")
	}
}

// An UPDATE under a column-restricted grant gates the Set map too, not just an
// insert's column list. Assigning a column outside the grant is denied.
func TestUpdateColumnGateRejectsUngrantedSet(t *testing.T) {
	reg := authz.NewRegistry(
		[]authz.Grant{{Role: "web_user", Relation: "films", Action: authz.Update, Columns: []string{"title"}}},
		nil,
	)
	p := &ir.Plan{
		Rel: filmsRel(),
		Query: &ir.Query{
			Kind:     ir.Update,
			Relation: ir.Ref{Schema: "public", Name: "films"},
			Write:    &ir.WriteSpec{Set: map[string]ir.Value{"secret": {Text: "x"}}},
		},
	}
	if err := reg.Authorize(&reqctx.Context{Role: "web_user"}, p); err == nil {
		t.Fatal("updating an ungranted column through Set was allowed")
	}
}

// The same restricted UPDATE is allowed when every Set column is within the
// grant, so the gate is not simply refusing all column-restricted writes.
func TestUpdateColumnGateAllowsGrantedSet(t *testing.T) {
	reg := authz.NewRegistry(
		[]authz.Grant{{Role: "web_user", Relation: "films", Action: authz.Update, Columns: []string{"title"}}},
		nil,
	)
	p := &ir.Plan{
		Rel: filmsRel(),
		Query: &ir.Query{
			Kind:     ir.Update,
			Relation: ir.Ref{Schema: "public", Name: "films"},
			Write:    &ir.WriteSpec{Set: map[string]ir.Value{"title": {Text: "Dune"}}},
		},
	}
	if err := reg.Authorize(&reqctx.Context{Role: "web_user"}, p); err != nil {
		t.Fatalf("updating a granted column should be allowed: %v", err)
	}
}

func TestNumericClaimComparesAsText(t *testing.T) {
	reg := authz.NewRegistry(
		[]authz.Grant{{Role: "web_user", Relation: "films", Action: authz.Insert}},
		[]authz.Policy{{
			Relation: "films", Role: "web_user",
			WithCheck: authz.Predicate{Terms: []authz.Term{{Column: "id", Op: authz.OpEq, Claim: "uid"}}},
		}},
	)
	p := &ir.Plan{
		Rel: filmsRel(),
		Query: &ir.Query{
			Kind:     ir.Insert,
			Relation: ir.Ref{Schema: "public", Name: "films"},
			Write: &ir.WriteSpec{
				Columns: []string{"id"},
				Rows:    []map[string]ir.Value{{"id": {Text: "42"}}},
			},
		},
	}
	// A JSON number claim must compare as "42", not "42.000000".
	rc := &reqctx.Context{Role: "web_user", Claims: map[string]any{"uid": float64(42)}}
	if err := reg.Authorize(rc, p); err != nil {
		t.Fatalf("numeric claim 42 should match id 42: %v", err)
	}
}

func TestNestedClaimPath(t *testing.T) {
	reg := authz.NewRegistry(
		[]authz.Grant{{Role: "web_user", Relation: "films", Action: authz.Select}},
		[]authz.Policy{{
			Relation: "films", Role: "web_user",
			Using: authz.Predicate{Terms: []authz.Term{{Column: "owner", Op: authz.OpEq, Claim: "app.tenant"}}},
		}},
	)
	p := readPlan()
	rc := &reqctx.Context{Role: "web_user", Claims: map[string]any{
		"app": map[string]any{"tenant": "acme"},
	}}
	if err := reg.Authorize(rc, p); err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	c := (*p.Query.Where).(ir.And).Kids[0].(ir.Compare)
	if c.Value.Text != "acme" {
		t.Errorf("resolved nested claim = %q, want acme", c.Value.Text)
	}
}

func TestNilPlanAndRPCPassThrough(t *testing.T) {
	reg := authz.NewRegistry(nil, nil)
	if err := reg.Authorize(&reqctx.Context{Role: "web_user"}, nil); err != nil {
		t.Errorf("nil plan should pass through, got %v", err)
	}
	// An RPC plan carries no relation query.
	if err := reg.Authorize(&reqctx.Context{Role: "web_user"}, &ir.Plan{}); err != nil {
		t.Errorf("query-less plan should pass through, got %v", err)
	}
}

func BenchmarkAuthorizeReadWithPolicy(b *testing.B) {
	reg := authz.NewRegistry(
		[]authz.Grant{{Role: "web_user", Relation: "films", Action: authz.Select, Columns: []string{"id", "title"}}},
		[]authz.Policy{{
			Relation: "films", Role: "web_user",
			Using: authz.Predicate{Terms: []authz.Term{{Column: "owner", Op: authz.OpEq, Claim: "sub"}}},
		}},
	)
	rel := filmsRel()
	rc := &reqctx.Context{Role: "web_user", Claims: map[string]any{"sub": "alice"}}
	b.ReportAllocs()
	for b.Loop() {
		// A fresh plan each iteration: Authorize mutates it in place.
		p := &ir.Plan{
			Rel: rel,
			Query: &ir.Query{
				Kind:     ir.Read,
				Relation: ir.Ref{Schema: "public", Name: "films"},
				Select:   []ir.SelectItem{col("id"), col("title")},
			},
		}
		if err := reg.Authorize(rc, p); err != nil {
			b.Fatalf("Authorize: %v", err)
		}
	}
}

// projectedNames lists the base column names of a projection in order.
func projectedNames(items []ir.SelectItem) []string {
	var out []string
	for _, it := range items {
		if c, ok := it.(ir.Column); ok {
			out = append(out, c.Path[0])
		}
	}
	return out
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
