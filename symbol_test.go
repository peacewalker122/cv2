package main

import (
	"path/filepath"
	"reflect"
	"sort"
	"testing"
)

func TestFixtureReferences(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("testdata", "go"))
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		file string
		line int
		col  int
		want []string
	}{
		{"pkg/a.go", 12, 10, []string{"pkg/a.go:11", "pkg/a.go:12"}},
		{"pkg/b.go", 4, 9, []string{"pkg/a.go:3", "pkg/b.go:4"}},
		{"other/c.go", 3, 6, []string{"other/c.go:3"}},
	}
	for _, tc := range cases {
		gotAns, err := references(root, tc.file, tc.line, tc.col)
		if err != nil {
			t.Fatal(err)
		}
		seen := map[string]bool{}
		var got []string
		for _, hit := range gotAns.Results {
			if hit.Source != "go/ast" && hit.Source != "treesitter" {
				t.Fatalf("%s:%d source=%s", tc.file, tc.line, hit.Source)
			}
			if hit.Confidence != "resolved" {
				t.Fatalf("%s:%d source=%s confidence=%s", tc.file, tc.line, hit.Source, hit.Confidence)
			}
			key := hit.File + ":" + itoa(hit.Range[0])
			if seen[key] {
				continue
			}
			seen[key] = true
			got = append(got, key)
		}
		sort.Strings(got)
		sort.Strings(tc.want)
		if !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("%s:%d:%d got %v want %v unresolved %v", tc.file, tc.line, tc.col, got, tc.want, gotAns.Unresolved)
		}
	}
}
