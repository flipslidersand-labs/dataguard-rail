package store

import (
	"reflect"
	"testing"

	"github.com/flipslidersand/dataguard-rail/internal/engine"
)

// TestViolationsForMatchesExistingMethods は For 系 (table 空=全件) が、
// 従来の 4 メソッドと同じキー範囲・同じ返却値であることを固定する。
func TestViolationsForMatchesExistingMethods(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	in := []engine.Violation{
		{ID: "v1", Table: "a"}, {ID: "v2", Table: "a"}, {ID: "v1", Table: "ab"}, {ID: "e1", Table: ""},
	}
	if err := s.SaveViolations(in); err != nil {
		t.Fatalf("SaveViolations: %v", err)
	}

	// 全件: table "" → CountViolations / ListViolationsPaged と同一。
	wantN, _ := s.CountViolations()
	gotN, err := s.CountViolationsFor("")
	if err != nil || gotN != wantN || gotN != len(in) {
		t.Errorf("CountViolationsFor(\"\") = %d (err=%v), want %d", gotN, err, wantN)
	}
	for _, p := range [][2]int{{0, 0}, {2, 1}, {10, 3}} {
		want, _ := s.ListViolationsPaged(p[0], p[1])
		got, err := s.ListViolationsFor("", p[0], p[1])
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("ListViolationsFor(\"\", %v) = %v (err=%v), want %v", p, got, err, want)
		}
	}

	// table 指定: 従来の ByTable 系と同一 (前方一致の "ab" は "a" に混入しない)。
	for _, tbl := range []string{"a", "ab", "nope"} {
		wantN, _ := s.CountViolationsByTable(tbl)
		if gotN, err := s.CountViolationsFor(tbl); err != nil || gotN != wantN {
			t.Errorf("CountViolationsFor(%q) = %d (err=%v), want %d", tbl, gotN, err, wantN)
		}
		want, _ := s.ListViolationsByTablePaged(tbl, 1, 1)
		got, err := s.ListViolationsFor(tbl, 1, 1)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("ListViolationsFor(%q) = %v (err=%v), want %v", tbl, got, err, want)
		}
	}
}
