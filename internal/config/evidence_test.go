package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEvidencePolicyStrictConfiguration(t *testing.T) {
	for _, tc := range []struct {
		text  string
		valid bool
	}{{"", true}, {"[evidence]\nmode='shadow'\nmax_related_nodes=24", true}, {"[evidence]\nmode='typo'", false}, {"[evidence]\nmax_related_nodes=-1", false}, {"[evidence]\nmax_relatted_nodes=24", false}} {
		p := filepath.Join(t.TempDir(), "config.toml")
		os.WriteFile(p, []byte(tc.text), 0600)
		cfg, e := Load(p)
		if (e == nil) != tc.valid {
			t.Fatalf("%s: %v", tc.text, e)
		}
		if e == nil && tc.text == "" && cfg.Evidence.Enabled() {
			t.Fatal("evidence enabled by default")
		}
	}
}
