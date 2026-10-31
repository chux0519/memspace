package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestContextPolicyTOMLPreservesZeroAndRejectsInvalidValues(t *testing.T) {
	for _, tc := range []struct {
		text  string
		valid bool
	}{
		{"[context]\nadmission_mode='hybrid'\nsemantic_min_similarity=0.0\nkeyword_min_coverage=0.75\nallow_exact_keywords=false\n", true},
		{"[context]\nkeyword_min_coverage=1.1\n", false},
		{"[context]\nadmission_mode='typo'\n", false},
		{"[context]\nsemantic_min=0.5\n", false},
	} {
		p := filepath.Join(t.TempDir(), "config.toml")
		if err := os.WriteFile(p, []byte(tc.text), 0600); err != nil {
			t.Fatal(err)
		}
		c, err := Load(p)
		if (err == nil) != tc.valid {
			t.Fatalf("unexpected validation: %v", err)
		}
		if tc.valid && (c.Context.SemanticMin == nil || *c.Context.SemanticMin != 0 || c.Context.ExactKeywords == nil || *c.Context.ExactKeywords) {
			t.Fatal("explicit zero/false lost")
		}
	}
}
