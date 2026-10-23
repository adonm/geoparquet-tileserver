package main

import "testing"

func TestNormalizeFilter(t *testing.T) {
	tests := map[string]string{
		"  category = 'Best'  ":          "category = 'Best'",
		"category='Best' AND state='AL'": "category='Best' AND state='AL'",
		"name = 'A  B'":                  "name = 'A  B'",
		"name = 'O''Brien'":              "name = 'O''Brien'",
		"a\t=\n1":                        "a = 1",
	}
	for in, want := range tests {
		if got := normalizeFilter(in); got != want {
			t.Errorf("normalizeFilter(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTileCacheKeyVersioned(t *testing.T) {
	st := &catalogState{name: "default", version: "release-a"}
	a := tileCacheKey(st, "place", 8, 66, 102, 0, "", "category = 'Best'")
	b := tileCacheKey(st, "place", 8, 66, 102, 0, "", "category  =   'Best'")
	if a != b {
		t.Fatalf("equivalent whitespace should share a key:\n%s\n%s", a, b)
	}
	if a != tileCacheKey(st, "place", 8, 66, 102, 0, "cql2-text", "category = 'Best'") {
		t.Fatal("default filter-lang should match cql2-text")
	}
	c := tileCacheKey(&catalogState{name: "default", version: "release-b"}, "place", 8, 66, 102, 0, "", "category = 'Best'")
	if a == c {
		t.Fatal("catalog version must change the key")
	}
	d := tileCacheKey(st, "place", 8, 66, 102, 0, "", "category = 'Worst'")
	if a == d {
		t.Fatal("filter must change the key")
	}
}
