package prompt

import (
	"strings"
	"unicode"
)

// ShouldRecall allows compact Han queries such as "死锁" or "证书过期" while
// keeping acknowledgements and bare continuation prompts off the network.
// Latin-only input retains the existing six-letter/digit minimum.
func ShouldRecall(q string) bool {
	plain := strings.ToLower(strings.Join(strings.Fields(strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || unicode.IsSpace(r) {
			return r
		}
		return -1
	}, q)), " "))
	switch plain {
	case "", "ok", "okay", "thanks", "thank you", "hello", "好的", "好", "嗯", "谢谢", "谢谢你", "收到", "好吧", "行吧", "对的", "嗯嗯", "好嘞", "了解", "了解了", "明白", "知道了", "可以", "行", "你好", "继续", "接着", "没问题", "是的", "不用了", "为什么", "怎么办", "怎么做", "什么情况":
		return false
	}
	n, han := 0, 0
	for _, r := range q {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			n++
		}
		if unicode.Is(unicode.Han, r) {
			han++
		}
	}
	return n >= 6 || han >= 2
}
