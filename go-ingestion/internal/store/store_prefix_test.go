package store

import (
	"sort"
	"testing"

	"github.com/flipslidersand/dataguard-rail/internal/engine"
)

// TestViolationKeyRanges は Count/List (全件・table 指定・空 table) が走査するキー範囲を固定する。
// 期待値は保存データとキー順 (violation:<table>:<id> の辞書順) から独立に組み立てる。
// 空 table の tableKeyPrefix("") は `violation::` で、全件用 keyPrefix とは別物である。
func TestViolationKeyRanges(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	in := []engine.Violation{
		{ID: "v1", Table: "a"},
		{ID: "v2", Table: "a"},
		{ID: "v1", Table: "ab"}, // "a" の prefix `violation:a:` に混入しないこと
		{ID: "v1", Table: "stock"},
		{ID: "e1", Table: ""}, // key = `violation::e1`
	}
	if err := s.SaveViolations(in); err != nil {
		t.Fatalf("SaveViolations: %v", err)
	}

	ids := func(vs []engine.Violation) []string {
		var out []string
		for _, v := range vs {
			out = append(out, v.Table+"/"+v.ID)
		}
		return out
	}
	// キー順 (`violation:` + table + `:` + id) の辞書順に並べた期待値。
	keyOf := func(v engine.Violation) string { return keyPrefix + v.Table + ":" + v.ID }
	want := func(table *string) []string {
		var sel []engine.Violation
		for _, v := range in {
			if table == nil || v.Table == *table {
				sel = append(sel, v)
			}
		}
		sort.Slice(sel, func(i, j int) bool { return keyOf(sel[i]) < keyOf(sel[j]) })
		return ids(sel)
	}
	tbl := func(s string) *string { return &s }

	equal := func(a, b []string) bool {
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

	cases := []struct {
		name  string
		table *string // nil = 全件
	}{
		{"all", nil},
		{"table a", tbl("a")},
		{"table ab", tbl("ab")},
		{"empty table", tbl("")},
		{"nonexistent", tbl("nope")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			exp := want(c.table)

			var count int
			var all []engine.Violation
			if c.table == nil {
				count, err = s.CountViolations()
				if err == nil {
					all, err = s.ListViolationsPaged(0, 0)
				}
			} else {
				count, err = s.CountViolationsByTable(*c.table)
				if err == nil {
					all, err = s.ListViolationsByTablePaged(*c.table, 0, 0)
				}
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if count != len(exp) {
				t.Errorf("count = %d, want %d", count, len(exp))
			}
			if got := ids(all); !equal(got, exp) {
				t.Errorf("list = %v, want %v", got, exp)
			}

			// ページング: offset=1, limit=2 は exp[1:3] (範囲外は切り詰め)。
			var page []engine.Violation
			if c.table == nil {
				page, err = s.ListViolationsPaged(2, 1)
			} else {
				page, err = s.ListViolationsByTablePaged(*c.table, 2, 1)
			}
			if err != nil {
				t.Fatalf("paged: %v", err)
			}
			lo, hi := 1, 3
			if lo > len(exp) {
				lo = len(exp)
			}
			if hi > len(exp) {
				hi = len(exp)
			}
			if got := ids(page); !equal(got, exp[lo:hi]) {
				t.Errorf("page = %v, want %v", got, exp[lo:hi])
			}
		})
	}
}
