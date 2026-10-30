package benchutil

import "testing"

func TestDistTailIsReported(t *testing.T) {
	d := Dist([]float64{1, 2, 100})
	if d["p95"].(float64) < 90 || d["max"].(float64) != 100 {
		t.Fatal(d)
	}
	if Dist(nil)["n"] != 0 {
		t.Fatal("empty")
	}
}
