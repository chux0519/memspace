package embedding

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// Chunking targets (estimated tokens). Snowflake's tokenizer truncates at
// 512; staying well below keeps every chunk whole and attention cheap.
const (
	ChunkTokens     = 380
	ChunkOverlap    = 40
	maxChunksPerDoc = 32
)

// EstimateTokens approximates subword tokens without a tokenizer: about one
// per CJK character, ~1.3 per other word, one per punctuation mark.
func EstimateTokens(s string) int {
	n := 0.0
	inWord := false
	for _, r := range s {
		switch {
		case isCJK(r):
			n++
			inWord = false
		case unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_':
			if !inWord {
				n += 1.3
				inWord = true
			}
		case unicode.IsSpace(r):
			inWord = false
		default:
			n++
			inWord = false
		}
	}
	return int(n + 0.5)
}

func isCJK(r rune) bool {
	return unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hiragana, r) ||
		unicode.Is(unicode.Katakana, r) || unicode.Is(unicode.Hangul, r)
}

// Chunk splits text into pieces of about ChunkTokens, preferring to cut at
// line or sentence ends, with ChunkOverlap tokens repeated between
// neighbours so a match spanning a boundary is not lost.
func Chunk(text string) []string {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	if EstimateTokens(text) <= ChunkTokens {
		return []string{text}
	}
	units := splitUnits(text)
	var chunks []string
	var cur []string
	curTokens := 0
	flush := func() {
		if len(cur) == 0 {
			return
		}
		chunks = append(chunks, strings.TrimSpace(strings.Join(cur, "")))
		// Carry the tail over as overlap.
		var keep []string
		kept := 0
		for i := len(cur) - 1; i >= 0 && kept < ChunkOverlap; i-- {
			keep = append([]string{cur[i]}, keep...)
			kept += EstimateTokens(cur[i])
		}
		cur, curTokens = keep, kept
		if curTokens >= ChunkTokens/2 { // a huge last unit: don't duplicate it
			cur, curTokens = nil, 0
		}
	}
	for _, u := range units {
		t := EstimateTokens(u)
		if curTokens+t > ChunkTokens && curTokens > 0 {
			flush()
			if len(chunks) >= maxChunksPerDoc {
				break
			}
		}
		cur = append(cur, u)
		curTokens += t
	}
	if len(chunks) < maxChunksPerDoc {
		if s := strings.TrimSpace(strings.Join(cur, "")); s != "" && (len(chunks) == 0 || !strings.HasSuffix(chunks[len(chunks)-1], s)) {
			chunks = append(chunks, s)
		}
	}
	return chunks
}

// splitUnits cuts text after sentence ends and newlines, and hard-splits any
// piece still longer than a chunk.
func splitUnits(text string) []string {
	var units []string
	start := 0
	for i, r := range text {
		end := i + utf8.RuneLen(r)
		if r == '\n' || r == '。' || r == '！' || r == '？' || r == '；' ||
			((r == '.' || r == '!' || r == '?' || r == ';') && end < len(text) && text[end] == ' ') {
			units = append(units, text[start:end])
			start = end
		}
	}
	if start < len(text) {
		units = append(units, text[start:])
	}
	var out []string
	for _, u := range units {
		for EstimateTokens(u) > ChunkTokens {
			cut := cutAt(u, ChunkTokens)
			out = append(out, u[:cut])
			u = u[cut:]
		}
		if u != "" {
			out = append(out, u)
		}
	}
	return out
}

// cutAt returns a byte offset in s after about n estimated tokens, at a
// rune (preferably space) boundary. Counting mirrors EstimateTokens.
func cutAt(s string, n int) int {
	lastSpace := -1
	count := 0.0
	inWord := false
	for i, r := range s {
		if int(count+0.5) >= n {
			if lastSpace > i/2 {
				return lastSpace
			}
			return i
		}
		switch {
		case isCJK(r):
			count++
			inWord = false
		case unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_':
			if !inWord {
				count += 1.3
				inWord = true
			}
		case unicode.IsSpace(r):
			inWord = false
			lastSpace = i
		default:
			count++
			inWord = false
		}
	}
	return len(s)
}
