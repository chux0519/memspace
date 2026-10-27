// Package redact removes secrets from event payloads and bounds their size.
//
// It runs twice: in the client hook before an event touches the local spool,
// and again on the server at ingest (defence in depth). Rules are
// deliberately conservative pattern matches; they reduce exposure, they do not
// guarantee it.
package redact

import (
	"encoding/json"
	"path"
	"regexp"
	"strings"
	"unicode/utf8"
)

// Placeholder replaces every redacted value.
const Placeholder = "[REDACTED]"

type rule struct {
	re   *regexp.Regexp
	repl func(m []string) string
}

func drop([]string) string { return Placeholder }

var rules = []rule{
	// PEM private keys (multi-line).
	{regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----[\s\S]*?(-----END [A-Z0-9 ]*PRIVATE KEY-----|$)`), drop},
	// Provider-specific token shapes.
	{regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`), drop},
	{regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{30,}\b`), drop},
	{regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{20,}\b`), drop},
	{regexp.MustCompile(`\bsk-(?:ant-|proj-)?[A-Za-z0-9_\-]{20,}`), drop},
	{regexp.MustCompile(`\bxox[abposr]-[A-Za-z0-9\-]{10,}`), drop},
	{regexp.MustCompile(`\bAIza[0-9A-Za-z_\-]{35}\b`), drop},
	{regexp.MustCompile(`\b[sr]k_(?:live|test)_[0-9A-Za-z]{16,}\b`), drop},
	{regexp.MustCompile(`\beyJ[A-Za-z0-9_\-]{8,}\.[A-Za-z0-9_\-]{8,}\.[A-Za-z0-9_\-]{8,}`), drop},
	// Authorization headers: keep the scheme, drop the credential.
	{regexp.MustCompile(`(?i)\b(authorization["']?\s*[:=]\s*["']?(?:bearer|basic|token)\s+)[A-Za-z0-9._~+/=\-]{8,}`),
		func(m []string) string { return m[1] + Placeholder }},
	{regexp.MustCompile(`(?i)\b(bearer\s+)[A-Za-z0-9._~+/=\-]{20,}`),
		func(m []string) string { return m[1] + Placeholder }},
	// Credentials embedded in URLs: scheme://user:password@host.
	{regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.\-]*://[^\s:/@"']+:)[^\s@/"']+@`),
		func(m []string) string { return m[1] + Placeholder + "@" }},
	// Generic NAME=value / "name": "value" assignments with a sensitive name.
	{regexp.MustCompile(`(?i)\b([A-Za-z0-9_.\-]*(?:password|passwd|secret|token|api[_\-]?key|access[_\-]?key|private[_\-]?key|credential|auth)[A-Za-z0-9_.\-]*)(["']?\s*[:=]\s*)(["']?)([^\s"',;}]+)`),
		func(m []string) string {
			v := m[4]
			// Avoid mangling counters like max_tokens=1500 or placeholders.
			if len(v) < 6 || isNumeric(v) || v == Placeholder || strings.HasPrefix(v, "$") {
				return m[0]
			}
			return m[1] + m[2] + m[3] + Placeholder
		}},
}

func isNumeric(s string) bool {
	for _, r := range s {
		if (r < '0' || r > '9') && r != '.' && r != ',' && r != '_' {
			return false
		}
	}
	return true
}

// String redacts secrets inside free text.
func String(s string) string {
	for _, r := range rules {
		s = r.re.ReplaceAllStringFunc(s, func(match string) string {
			return r.repl(r.re.FindStringSubmatch(match))
		})
	}
	return s
}

var sensitiveKey = regexp.MustCompile(`(?i)^(?:password|passwd|secret|token|api[_\-]?key|apikey|access[_\-]?token|refresh[_\-]?token|id[_\-]?token|client[_\-]?secret|private[_\-]?key|authorization|cookie|set-cookie)$|[_\-](?:secret|token|password|api[_\-]?key)$`)

// SensitiveKey reports whether a JSON object key names a credential.
func SensitiveKey(k string) bool { return sensitiveKey.MatchString(k) }

// Value walks a decoded JSON value, redacting credential-named keys and
// secrets inside strings.
func Value(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			if s, ok := val.(string); ok && SensitiveKey(k) && s != "" {
				t[k] = Placeholder
				continue
			}
			t[k] = Value(val)
		}
		return t
	case []any:
		for i := range t {
			t[i] = Value(t[i])
		}
		return t
	case string:
		return String(t)
	default:
		return v
	}
}

// JSON redacts a JSON document. Invalid JSON is redacted as plain text and
// wrapped so the result is always valid JSON.
func JSON(raw []byte) []byte {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		out, _ := json.Marshal(map[string]string{"raw": String(string(raw))})
		return out
	}
	out, err := json.Marshal(Value(v))
	if err != nil {
		return raw
	}
	return out
}

// SensitivePath reports whether a file path looks like a secret store whose
// contents must never be captured (.env, private keys, cloud credentials…).
func SensitivePath(p string) bool {
	if p == "" {
		return false
	}
	// Normalize separators ourselves: agent payloads may carry Windows paths
	// regardless of the OS this runs on.
	norm := strings.ToLower(strings.ReplaceAll(p, `\`, "/"))
	for _, dir := range []string{"/.ssh/", "/.aws/", "/.gnupg/", "/.docker/config.json", "/.kube/config"} {
		if strings.Contains(norm, dir) {
			return true
		}
	}
	base := path.Base(norm)
	switch {
	case base == ".env" || base == ".envrc":
		return true
	case strings.HasPrefix(base, ".env."):
		for _, ok := range []string{".example", ".sample", ".template", ".dist"} {
			if strings.HasSuffix(base, ok) {
				return false
			}
		}
		return true
	case strings.HasPrefix(base, "id_rsa"), strings.HasPrefix(base, "id_ed25519"),
		strings.HasPrefix(base, "id_ecdsa"), strings.HasPrefix(base, "id_dsa"):
		return !strings.HasSuffix(base, ".pub")
	}
	for _, ext := range []string{".pem", ".key", ".p12", ".pfx", ".keystore", ".jks"} {
		if strings.HasSuffix(base, ext) {
			return true
		}
	}
	switch base {
	case ".netrc", ".npmrc", ".pypirc", ".git-credentials", "credentials", "credentials.json",
		"secrets.json", "secrets.yaml", "secrets.yml", "auth.json", "service-account.json":
		return true
	}
	return false
}

// Truncate shortens s to at most about max bytes by cutting the middle: the
// beginning and the end of a text are where intent usually is (the question
// after a pasted log, the conclusion after a long explanation). Cuts fall on
// rune boundaries and the marker records how much was dropped.
func Truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	headLen := max / 2
	tailStart := len(s) - (max - headLen)
	for headLen > 0 && !utf8.RuneStart(s[headLen]) {
		headLen--
	}
	for tailStart < len(s) && !utf8.RuneStart(s[tailStart]) {
		tailStart++
	}
	return s[:headLen] + "\n…[" + itoa(tailStart-headLen) + " bytes omitted]…\n" + s[tailStart:]
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// Bound truncates every string in v to maxString bytes, then shrinks the
// limit until the encoded value fits in maxTotal bytes. It also strips inline
// data: URIs (screenshots), which are large and useless for text retrieval.
func Bound(v any, maxString, maxTotal int) any {
	for limit := maxString; ; limit /= 4 {
		b := boundStrings(deepCopy(v), limit)
		out, err := json.Marshal(b)
		if err != nil || len(out) <= maxTotal || limit <= 128 {
			if err == nil && len(out) > maxTotal {
				return map[string]any{"truncated": true, "original_bytes": len(out)}
			}
			return b
		}
	}
}

var dataURI = regexp.MustCompile(`data:[a-zA-Z0-9.+\-/]+;base64,[A-Za-z0-9+/=]{64,}`)

func boundStrings(v any, limit int) any {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			t[k] = boundStrings(val, limit)
		}
		return t
	case []any:
		for i := range t {
			t[i] = boundStrings(t[i], limit)
		}
		return t
	case string:
		if len(t) > 64 && strings.Contains(t, ";base64,") {
			t = dataURI.ReplaceAllString(t, "[inline data omitted]")
		}
		return Truncate(t, limit)
	default:
		return v
	}
}

func deepCopy(v any) any {
	switch t := v.(type) {
	case map[string]any:
		m := make(map[string]any, len(t))
		for k, val := range t {
			m[k] = deepCopy(val)
		}
		return m
	case []any:
		s := make([]any, len(t))
		for i := range t {
			s[i] = deepCopy(t[i])
		}
		return s
	default:
		return v
	}
}
