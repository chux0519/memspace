package embedding

import "testing"

func TestThresholdsFor(t *testing.T) {
	cases := []struct {
		model          string
		inject, search float64
		want           Thresholds
	}{
		{"BAAI/bge-m3", 0, 0, Thresholds{0.60, 0.45, "bge-m3"}},
		{"Snowflake/snowflake-arctic-embed-m-v2.0", 0, 0, Thresholds{0.35, 0.20, "arctic-embed"}},
		{"some-new-model", 0, 0, Thresholds{0.60, 0.30, "default"}},
		{"BAAI/bge-m3", 0.5, 0, Thresholds{0.50, 0.45, "config"}},
		{"BAAI/bge-m3", 0, 0.4, Thresholds{0.60, 0.40, "config"}},
	}
	for _, c := range cases {
		if got := ThresholdsFor(c.model, c.inject, c.search); got != c.want {
			t.Errorf("ThresholdsFor(%q, %v, %v) = %+v, want %+v", c.model, c.inject, c.search, got, c.want)
		}
	}
}
