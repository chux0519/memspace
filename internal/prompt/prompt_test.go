package prompt

import (
	"fmt"
	"strings"
	"testing"
)

func logBlock(n int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "2026-10-26 10:%02d:01 INFO request id=%d handled in 12ms\n", i%60, i)
		if i == n/2 {
			b.WriteString("2026-10-26 10:30:00 ERROR auth: context deadline exceeded (timeout after 30s)\n")
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

func TestPastedLogThenQuestion(t *testing.T) {
	in := logBlock(200) + "\n\n为什么认证模块会超时？帮我看看。"
	r := Split(in)
	if !strings.HasSuffix(r.Text, "为什么认证模块会超时？帮我看看。") {
		t.Fatalf("question lost: %q", r.Text)
	}
	if len(r.Text) > 200 {
		t.Errorf("log not extracted, text is %d bytes", len(r.Text))
	}
	if len(r.Attachments) != 1 || r.Attachments[0].Kind != "log" || r.Attachments[0].Lines < 200 {
		t.Fatalf("attachments = %+v", r.Attachments)
	}
	a := r.Attachments[0]
	if len(a.Errors) != 1 || !strings.Contains(a.Errors[0], "deadline exceeded") {
		t.Errorf("error lines = %v", a.Errors)
	}
	if len(a.SHA256) != 64 || len(a.Excerpt) > 700 {
		t.Errorf("hash/excerpt: %d %d", len(a.SHA256), len(a.Excerpt))
	}
}

// Pasting a log and typing the question on the next line, with no blank
// line in between, is common.
func TestQuestionAdjacentToLog(t *testing.T) {
	r := Split("这个报错怎么回事：\n" + logBlock(30) + "\n为什么认证超时？")
	if len(r.Attachments) != 1 || !strings.HasPrefix(r.Text, "这个报错怎么回事：") || !strings.HasSuffix(r.Text, "为什么认证超时？") {
		t.Fatalf("text=%q atts=%d", r.Text, len(r.Attachments))
	}
}

func TestCodeFenceAndShortSnippets(t *testing.T) {
	big := "```go\n" + strings.Repeat("func f() { return }\n", 40) + "```"
	small := "```sh\ngo test ./...\n```"
	in := "Refactor this:\n" + big + "\nand keep running " + small + " afterwards."
	r := Split(in)
	if len(r.Attachments) != 1 || r.Attachments[0].Kind != "code" || r.Attachments[0].Lang != "go" {
		t.Fatalf("attachments = %+v", r.Attachments)
	}
	if !strings.Contains(r.Text, "go test ./...") || !strings.Contains(r.Text, "Refactor this:") {
		t.Errorf("short snippet or own words lost: %q", r.Text)
	}
	if strings.Contains(r.Text, "func f()") {
		t.Errorf("big block left inline")
	}
}

func TestUnclosedFenceAndQuotes(t *testing.T) {
	in := "看这段：\n```\n" + strings.Repeat("x := 1\n", 100)
	r := Split(in)
	if len(r.Attachments) != 1 || !strings.HasPrefix(r.Text, "看这段：") {
		t.Fatalf("unclosed fence: %+v %q", r.Attachments, r.Text)
	}
	q := strings.Repeat("> 引用的一段很长的设计文档内容，用来测试引用块提取。\n", 20) + "我同意吗？"
	r = Split(q)
	if len(r.Attachments) != 1 || r.Attachments[0].Kind != "quote" || !strings.HasSuffix(r.Text, "我同意吗？") {
		t.Fatalf("quote: %+v %q", r.Attachments, r.Text)
	}
}

func TestOrdinaryProseUntouched(t *testing.T) {
	in := strings.Repeat("这是一段普通的需求描述，没有粘贴的内容，只是比较长。\n", 30)
	r := Split(in)
	if r.Text != in || len(r.Attachments) != 0 {
		t.Errorf("prose was altered: %d attachments", len(r.Attachments))
	}
}

func TestPlaceholders(t *testing.T) {
	r := Split("看看这张图 [Image #1] 和 [Pasted text #2 +120 lines] 有什么问题")
	if len(r.Attachments) != 2 || r.Attachments[0].Kind != "image" || r.Attachments[1].Kind != "pasted" {
		t.Errorf("placeholders: %+v", r.Attachments)
	}
}
