package store

import (
	"reflect"
	"testing"

	"github.com/flipslidersand/dataguard-rail/internal/engine"
)

// TestViolationKeyRanges は CountViolationsFor / ListViolationsFor が走査するキー範囲を、
// 固定の期待値で確認する。期待値は実装のヘルパーを使わず、保存データのキー
// (`violation:<table>:<id>`) の辞書順から手で書き下したもの:
//
//	violation::e1 < violation:a:v1 < violation:a:v2 < violation:ab:v1 < violation:stock:v1
//
// table が空文字なら全件 (keyPrefix)、"a" の範囲に前方一致する "ab" は混入しない。
func TestViolationKeyRanges(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	in := []engine.Violation{
		{ID: "v1", Table: "a"},
		{ID: "v2", Table: "a"},
		{ID: "v1", Table: "ab"},
		{ID: "v1", Table: "stock"},
		{ID: "e1", Table: ""}, // key = `violation::e1`。For("") (全件) にのみ含まれる
	}
	if err := s.SaveViolations(in); err != nil {
		t.Fatalf("SaveViolations: %v", err)
	}

	type page struct {
		limit, offset int
		want          []string
	}
	cases := []struct {
		name      string
		table     string
		wantCount int
		pages     []page
	}{
		{"all", "", 5, []page{
			{0, 0, []string{"/e1", "a/v1", "a/v2", "ab/v1", "stock/v1"}},
			{2, 1, []string{"a/v1", "a/v2"}},
			{10, 3, []string{"ab/v1", "stock/v1"}},
		}},
		{"table a", "a", 2, []page{
			{0, 0, []string{"a/v1", "a/v2"}},
			{1, 1, []string{"a/v2"}},
			{5, 2, nil},
		}},
		{"table ab", "ab", 1, []page{
			{0, 0, []string{"ab/v1"}},
			{1, 1, nil},
		}},
		{"nonexistent", "nope", 0, []page{
			{0, 0, nil},
			{2, 1, nil},
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			count, err := s.CountViolationsFor(c.table)
			if err != nil || count != c.wantCount {
				t.Errorf("CountViolationsFor(%q) = %d (err=%v), want %d", c.table, count, err, c.wantCount)
			}
			for _, p := range c.pages {
				got, err := s.ListViolationsFor(c.table, p.limit, p.offset)
				if err != nil {
					t.Fatalf("ListViolationsFor(%q, %d, %d): %v", c.table, p.limit, p.offset, err)
				}
				var ids []string
				for _, v := range got {
					ids = append(ids, v.Table+"/"+v.ID)
				}
				if !reflect.DeepEqual(ids, p.want) {
					t.Errorf("ListViolationsFor(%q, %d, %d) = %v, want %v", c.table, p.limit, p.offset, ids, p.want)
				}
			}
		})
	}
}
