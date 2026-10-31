// Package evidence maintains source-backed, versioned evidence relationships.
// Its projections never establish that a source statement is true.
package evidence

import (
	"context"
	"fmt"
)

type Policy struct {
	Mode       string `toml:"mode" json:"mode"`
	MaxRelated int    `toml:"max_related_nodes" json:"max_related_nodes"`
}

func (p Policy) Validate() error {
	if p.Mode != "" && p.Mode != "off" && p.Mode != "shadow" && p.Mode != "on" {
		return fmt.Errorf("evidence mode must be off, shadow or on")
	}
	if p.MaxRelated < 0 || p.MaxRelated > 512 {
		return fmt.Errorf("evidence max_related_nodes must be 0..512")
	}
	return nil
}
func (p Policy) Enabled() bool { return p.Mode == "shadow" || p.Mode == "on" }
func (p Policy) Limit() int {
	if p.MaxRelated == 0 {
		return 24
	}
	return p.MaxRelated
}

type policyKey struct{}

func WithPolicy(ctx context.Context, p Policy) context.Context {
	return context.WithValue(ctx, policyKey{}, p)
}
func FromContext(ctx context.Context) Policy { p, _ := ctx.Value(policyKey{}).(Policy); return p }
