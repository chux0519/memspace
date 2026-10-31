package admission

import (
	"context"
	"math"
	"testing"
)

func number(v float64) *float64 { return &v }
func TestDefaultMatchesLegacyAndExplicitZeroIsNotAutomatic(t *testing.T) {
	p, _ := (Policy{}).Resolve(.6)
	for _, c := range []struct {
		match          string
		sim            float64
		matched, total int
		want           bool
	}{
		{"all", 0, 1, 1, true}, {"semantic", .59, 0, 0, false}, {"semantic", .6, 0, 0, true},
		{"any", .59, 1, 4, false}, {"any", 0, 2, 4, true}, {"any", 0, 0, 0, false},
	} {
		if got := p.Accept(c.match, c.sim, c.matched, c.total); got != c.want {
			t.Fatalf("%+v: %v", c, got)
		}
	}
	zero, _ := (Policy{SemanticMin: number(0)}).Resolve(.6)
	if !zero.Accept("semantic", .2, 0, 0) || zero.Accept("any", 0, 1, 4) {
		t.Fatal("zero semantic threshold conflated with default or keyword-only hit")
	}
}
func TestPolicyModesAndValidation(t *testing.T) {
	for _, mode := range []string{"hybrid", "semantic", "none"} {
		p, err := (Policy{Mode: mode}).Resolve(.6)
		if err != nil {
			t.Fatal(err)
		}
		if p.Accept("all", 0, 4, 4) != (mode != "semantic") {
			t.Fatal("keyword shortcut not isolated")
		}
		if p.Accept("semantic", .3, 0, 0) != (mode == "none") {
			t.Fatal("mode did not control gate")
		}
	}
	for _, v := range []float64{-1, 1.01, math.NaN(), math.Inf(1)} {
		if (Policy{KeywordMin: number(v)}).Validate() == nil {
			t.Fatal("invalid coverage accepted")
		}
	}
	if (Policy{Mode: "typo"}).Validate() == nil {
		t.Fatal("unknown mode accepted")
	}
}
func TestPolicyContextDoesNotShareMutableSettings(t *testing.T) {
	v := .55
	p := Policy{SemanticMin: &v}
	ctx := WithPolicy(context.Background(), p)
	v = .9
	got, _ := FromContext(ctx).Resolve(.6)
	if got.SemanticMin != .55 {
		t.Fatal("settings shared across requests")
	}
}
