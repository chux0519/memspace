// Package prompt separates what a user wrote from what they pasted.
//
// A prompt like "<300 lines of log> why does auth time out?" is mostly
// pasted material; the intent is the last line. Capturing it verbatim and
// truncating would keep the log and lose the question. Split keeps the
// user's own words intact and turns large pasted blocks (code fences,
// quotes, logs and stack traces) into attachment records: kind, size, hash,
// a head+tail excerpt and the error lines, which is what retrieval needs.
package prompt

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"

	"github.com/chux0519/memspace/internal/redact"
)

// Attachment describes pasted or attached material that is not stored in
// full. Media content is never stored, only its metadata.
type Attachment struct {
	Kind    string   `json:"kind"` // code | log | quote | image | pasted
	Lang    string   `json:"lang,omitempty"`
	Bytes   int      `json:"bytes,omitempty"`
	Lines   int      `json:"lines,omitempty"`
	SHA256  string   `json:"sha256,omitempty"`
	Excerpt string   `json:"excerpt,omitempty"`
	Errors  []string `json:"errors,omitempty"`
	Ref     string   `json:"ref,omitempty"` // placeholder or path the agent used
}

// Result is a split prompt.
type Result struct {
	// Text is the prompt with large pasted blocks replaced by placeholders.
	Text        string
	Attachments []Attachment
}

const (
	// MinBlockBytes is the smallest pasted block worth extracting; shorter
	// snippets stay inline because they are usually part of the question.
	MinBlockBytes = 400
	minLogLines   = 8
	excerptBytes  = 600
	maxErrorLines = 8
)

var (
	fenceRE = regexp.MustCompile("^\\s*(```|~~~)\\s*([\\w+#.-]*)")
	// Agents' own placeholders for collapsed pastes and images.
	imageRefRE  = regexp.MustCompile(`\[Image #\d+\]`)
	pastedRefRE = regexp.MustCompile(`\[Pasted text #\d+[^\]]*\]`)
	errorLineRE = regexp.MustCompile(`(?i)\b(error|exception|fatal|panic|fail(ed|ure)?|traceback|denied|refused|timeout|timed out)\b|错误|异常|失败|超时`)
	logLineRE   = regexp.MustCompile(`(?i)^\s*(\d{4}[-/]\d{2}[-/]\d{2}[ T]\d{2}:\d{2}|\d{2}:\d{2}:\d{2}|\[?(trace|debug|info|warn|warning|error|fatal|panic)\]?[:\s]|at\s+\S+\(|File ".*", line \d+|goroutine \d+|#\d+\s|\S+\.(go|py|js|ts|rs|java|c|cc|cpp|rb):\d+|Traceback|Caused by:|\s{2,}\S)`)
)

// Split extracts large pasted blocks from a prompt.
func Split(s string) Result {
	lines := strings.Split(s, "\n")
	var out []string
	var atts []Attachment
	add := func(kind, lang string, block []string) {
		atts = append(atts, describe(kind, lang, block))
		out = append(out, fmt.Sprintf("[pasted %s #%d: %d lines, %s]", kind, len(atts), len(block), size(atts[len(atts)-1].Bytes)))
	}

	for i := 0; i < len(lines); {
		// Fenced code block.
		if m := fenceRE.FindStringSubmatch(lines[i]); m != nil {
			end := i + 1
			for end < len(lines) && !strings.HasPrefix(strings.TrimSpace(lines[end]), m[1]) {
				end++
			}
			last := min(end+1, len(lines)) // include the closing fence when present
			block := lines[i:last]
			if bytesOf(block) >= MinBlockBytes {
				add("code", m[2], block)
			} else {
				out = append(out, block...)
			}
			i = last
			continue
		}
		// Quoted run.
		if strings.HasPrefix(strings.TrimSpace(lines[i]), ">") {
			end := i
			for end < len(lines) && strings.HasPrefix(strings.TrimSpace(lines[end]), ">") {
				end++
			}
			if block := lines[i:end]; bytesOf(block) >= MinBlockBytes {
				add("quote", "", block)
			} else {
				out = append(out, block...)
			}
			i = end
			continue
		}
		// Log / stack trace run: consecutive non-empty lines, most of which
		// look like log output.
		end := i
		hits := 0
		for end < len(lines) && strings.TrimSpace(lines[end]) != "" && !fenceRE.MatchString(lines[end]) {
			if logLineRE.MatchString(lines[end]) {
				hits++
			}
			end++
		}
		if n := end - i; n >= minLogLines && hits*10 >= n*6 && bytesOf(lines[i:end]) >= MinBlockBytes {
			// The block is the log lines only: a question typed right
			// before or after the paste (no blank line) stays text.
			first, last := i, end
			for first < last && !logLineRE.MatchString(lines[first]) {
				first++
			}
			for last > first && !logLineRE.MatchString(lines[last-1]) {
				last--
			}
			out = append(out, lines[i:first]...)
			add("log", "", lines[first:last])
			out = append(out, lines[last:end]...)
			i = end
			continue
		}
		if end == i {
			end = i + 1
		}
		out = append(out, lines[i:end]...)
		i = end
	}

	text := strings.Join(out, "\n")
	for _, ref := range imageRefRE.FindAllString(text, -1) {
		atts = append(atts, Attachment{Kind: "image", Ref: ref})
	}
	for _, ref := range pastedRefRE.FindAllString(text, -1) {
		atts = append(atts, Attachment{Kind: "pasted", Ref: ref})
	}
	return Result{Text: text, Attachments: atts}
}

func describe(kind, lang string, block []string) Attachment {
	body := strings.Join(block, "\n")
	sum := sha256.Sum256([]byte(body))
	a := Attachment{
		Kind:    kind,
		Lang:    lang,
		Bytes:   len(body),
		Lines:   len(block),
		SHA256:  hex.EncodeToString(sum[:]),
		Excerpt: redact.Truncate(body, excerptBytes),
	}
	for _, l := range block {
		if len(a.Errors) >= maxErrorLines {
			break
		}
		if errorLineRE.MatchString(l) {
			a.Errors = append(a.Errors, redact.Truncate(strings.TrimSpace(l), 200))
		}
	}
	return a
}

func bytesOf(lines []string) int {
	n := 0
	for _, l := range lines {
		n += len(l) + 1
	}
	return n
}

func size(n int) string {
	if n >= 1024 {
		return fmt.Sprintf("%.1f KB", float64(n)/1024)
	}
	return fmt.Sprintf("%d B", n)
}

var placeholderRE = regexp.MustCompile(`\[pasted (code|log|quote) #\d+: [^\]]*\]`)

// OwnWords is the user's own text: the split prompt without the
// placeholders that stand in for pasted blocks.
func OwnWords(raw string) string {
	return StripPlaceholders(Split(raw).Text)
}

// StripPlaceholders removes pasted-block placeholders from split text.
func StripPlaceholders(text string) string {
	return strings.TrimSpace(placeholderRE.ReplaceAllString(text, " "))
}

// Payload renders a split prompt into event payload fields.
func Payload(raw string) map[string]any {
	r := Split(raw)
	p := map[string]any{"prompt": r.Text, "prompt_bytes": len(raw)}
	if len(r.Attachments) > 0 {
		p["attachments"] = r.Attachments
	}
	return p
}
