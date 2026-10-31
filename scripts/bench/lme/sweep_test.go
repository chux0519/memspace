package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAdmissionGridRejectsTyposAndUnsafeProfileNames(t *testing.T) {
	for _, raw := range []string{
		`[{"name":"x","policy":{}},{"name":"x","policy":{}}]`,
		`[{"name":"x","policy":{}},{"name":"../escape","policy":{}}]`,
		`[{"name":"x","policy":{}},{"name":"y","policy":{"semantic_min":0.5}}]`,
	} {
		p := filepath.Join(t.TempDir(), "profiles.json")
		if err := os.WriteFile(p, []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := readPolicies(p); err == nil {
			t.Fatal("invalid grid accepted")
		}
	}
}
