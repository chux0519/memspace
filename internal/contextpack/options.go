package contextpack

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/chux0519/memspace/internal/search"
)

// BuildOptions bounds the number of visible items without shrinking retrieval
// depth. Zero preserves existing behavior. This is also used by eval adapters
// to honor top_k without slicing already-rendered source text.
type BuildOptions struct{ MaxItems int }
type buildOptionsKey struct{}

func BuildWithOptions(ctx context.Context, db *sql.DB, owner, device string, req Request, opts BuildOptions) (*Pack, error) {
	if opts.MaxItems < 0 {
		return nil, fmt.Errorf("%w: negative item budget", search.ErrBadRequest)
	}
	return Build(context.WithValue(ctx, buildOptionsKey{}, opts), db, owner, device, req)
}
