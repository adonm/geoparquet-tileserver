package filter

import "testing"

func TestMatch(t *testing.T) {
	props := map[string]any{
		"category":    "Best",
		"city":        "Birmingham",
		"state":       "AL",
		"residential": true,
		"grade":       "A",
		"score":       int64(42),
	}
	tests := []struct {
		expr string
		want bool
	}{
		{"category = 'Best'", true},
		{"category <> 'Best'", false},
		{"state = 'AL' AND residential = true", true},
		{"state = 'CA' OR grade = 'A'", true},
		{"NOT category = 'Best'", false},
		{"city LIKE 'Birm%'", true},
		{"city LIKE 'Mobile%'", false},
		{"score BETWEEN 40 AND 45", true},
		{"grade IN ('A','B')", true},
		{"grade IN ('C','D')", false},
		{"category IS NOT NULL", true},
		{"missing IS NULL", true},
		{"residential = false", false},
	}
	for _, tt := range tests {
		f, err := Compile(tt.expr)
		if err != nil {
			t.Fatalf("%q: compile: %v", tt.expr, err)
		}
		got, err := f.Match(props)
		if err != nil {
			t.Fatalf("%q: match: %v", tt.expr, err)
		}
		if got != tt.want {
			t.Errorf("%q: got %v, want %v", tt.expr, got, tt.want)
		}
	}
}

func TestMalformed(t *testing.T) {
	if _, err := Compile("category =="); err == nil {
		t.Fatal("expected malformed filter to fail")
	}
}
