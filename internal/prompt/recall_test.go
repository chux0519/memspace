package prompt

import "testing"

func TestShouldRecall(t *testing.T) {
	for _, q := range []string{"死锁", "协程泄漏", "证书过期", "版本回滚", "缓存击穿", "SQL 报错", "auth timeout", "continue"} {
		if !ShouldRecall(q) {
			t.Errorf("technical query skipped: %q", q)
		}
	}
	for _, q := range []string{"", "好的！", "好的👍", "谢谢～", "行吧", "继续", "谢谢你", "收到", "为什么？", "ok", "thanks", "hello", "🙂", "啊"} {
		if ShouldRecall(q) {
			t.Errorf("low-information prompt triggered recall: %q", q)
		}
	}
}
