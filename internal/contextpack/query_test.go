package contextpack

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/chux0519/memspace/internal/embedding"
)

func TestPromptSemanticQueryKeepsShortQuestionsAndBoundsLargePastes(t *testing.T) {
	for _, query := range []string{"连接池为什么耗尽？", "What did we decide about the authentication cache?", "mixed 中文 retries 排查"} {
		if got := promptSemanticQuery(query); got != query {
			t.Fatalf("short query changed: %q", got)
		}
	}
	for _, body := range []string{strings.Repeat("这是日志与说明。", 1000), strings.Repeat("traceback recovery failure details ", 1000), strings.Repeat("a91bc0d2", 2000)} {
		query := "OAuth修复目标\n" + body + "\n最后问题：连接池如何恢复？"
		got := promptSemanticQuery(query)
		if !utf8.ValidString(got) || utf8.RuneCountInString(got) > PromptSemanticChars || embedding.EstimateTokens(got) > PromptSemanticTokens {
			t.Fatalf("semantic query exceeded its input budget: chars=%d tokens=%d", utf8.RuneCountInString(got), embedding.EstimateTokens(got))
		}
		if !strings.HasPrefix(got, "OAuth修复目标") || !strings.HasSuffix(got, "连接池如何恢复？") {
			t.Fatalf("lost subject or trailing instruction: %q", got)
		}
	}
}
