package schema

// This file projects base-table foreign keys onto views. PostgREST makes a view
// embeddable by inferring its relationships from the base tables behind it: when
// a base table's foreign-key columns survive unchanged into the view's select
// list, the view inherits that foreign key under the view's own column names. The
// same projection makes a view embeddable from both directions, because the
// reverse view of an inherited key is resolved by the ordinary backward scan.
// View-over-view chains resolve because projection runs to a fixpoint, so an
// inner view's inherited keys are available when the outer view projects. A view
// the introspector cannot resolve to plain base columns (a UNION, an expression
// column) carries no ViewColumns and so inherits nothing, matching PostgREST.

// projectViews carries base-table foreign keys onto every view in the model,
// using each view's column-to-base mapping. It repeats until no new key is added
// so that chains of views (a view selecting from another view) resolve, bounded
// by the relation count since each pass can only add keys.
func (m *Model) projectViews() {
	for pass := 0; pass < len(m.order); pass++ {
		added := false
		for _, key := range m.order {
			v := m.relations[key]
			if v.Kind != KindView || len(v.ViewColumns) == 0 {
				continue
			}
			if m.projectOneView(v) {
				added = true
			}
		}
		if !added {
			return
		}
	}
}

// projectOneView adds to view v every base-table foreign key whose columns all
// survive into v, naming the projected key's columns by the view columns that
// expose them. It reports whether it added a key this pass (a projected key is
// added once; a second pass over the same view is a no-op).
func (m *Model) projectOneView(v *Relation) bool {
	// Index the view's exposure of each base relation: base (schema,rel,col) to
	// the view column that projects it. A base column may surface under several
	// view columns; the first is used, the way a join projection names one.
	exposes := map[string]map[string]string{} // baseRelKey -> baseCol -> viewCol
	for _, vc := range v.ViewColumns {
		bk := Key(vc.BaseSchema, vc.BaseRelation)
		cols := exposes[bk]
		if cols == nil {
			cols = map[string]string{}
			exposes[bk] = cols
		}
		if _, seen := cols[vc.BaseColumn]; !seen {
			cols[vc.BaseColumn] = vc.Name
		}
	}

	added := false
	for bk, cols := range exposes {
		base, ok := m.relations[bk]
		if !ok {
			continue
		}
		for _, fk := range base.ForeignKeys {
			viewCols, ok := mapColumns(fk.Columns, cols)
			if !ok {
				continue // not every FK column survives into the view
			}
			if v.hasProjectedFK(fk, base) {
				continue
			}
			v.ForeignKeys = append(v.ForeignKeys, &ForeignKey{
				Name:           fk.Name,
				Columns:        viewCols,
				RefSchema:      fk.RefSchema,
				RefRelation:    fk.RefRelation,
				RefColumns:     fk.RefColumns,
				SourceRelation: base.Name,
			})
			added = true
		}
	}
	return added
}

// mapColumns translates base column names to the view columns that expose them,
// reporting ok=false if any base column is not exposed by the view.
func mapColumns(baseCols []string, exposed map[string]string) ([]string, bool) {
	out := make([]string, len(baseCols))
	for i, c := range baseCols {
		vc, ok := exposed[c]
		if !ok {
			return nil, false
		}
		out[i] = vc
	}
	return out, true
}

// hasProjectedFK reports whether the view already carries this base foreign key,
// so a second projection pass does not duplicate it.
func (r *Relation) hasProjectedFK(fk *ForeignKey, base *Relation) bool {
	for _, existing := range r.ForeignKeys {
		if existing.Name == fk.Name && existing.SourceRelation == base.Name {
			return true
		}
	}
	return false
}
