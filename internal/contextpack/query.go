package contextpack

import (
	"strings"

	"github.com/chux0519/memspace/internal/embedding"
)

// promptSemanticQuery keeps short questions verbatim. For a large paste retain
// both the opening subject and the final instruction instead of sending a full
// model window on the synchronous CPU path. This is not a generated summary.
func promptSemanticQuery(query string) string {
	query = strings.TrimSpace(query)
	runes := []rune(query)
	if len(runes) <= PromptSemanticChars && embedding.EstimateTokens(query) <= PromptSemanticTokens {
		return query
	}
	const gap = "\n…\n"
	keep := func(n int) string {
		head := (2*n + 2) / 3
		tail := n - head
		return strings.TrimSpace(string(runes[:head])) + gap + strings.TrimSpace(string(runes[len(runes)-tail:]))
	}
	low, high := 0, min(len(runes)-1, PromptSemanticChars-3)
	for low < high {
		mid := (low + high + 1) / 2
		if embedding.EstimateTokens(keep(mid)) <= PromptSemanticTokens {
			low = mid
		} else {
			high = mid - 1
		}
	}
	return keep(low)
}
