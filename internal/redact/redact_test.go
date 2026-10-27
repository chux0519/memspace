package redact

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestStringRedactsSecrets(t *testing.T) {
	cases := []struct{ in, leaked string }{
		{"export OPENAI_API_KEY=sk-proj-abcdefghijklmnopqrstuvwxyz123456", "abcdefghijklmnop"},
		{"key sk-ant-api03-AAAAAAAAAAAAAAAAAAAAAAAAAAAA", "api03-AAAA"},
		{"aws AKIAIOSFODNN7EXAMPLE here", "AKIAIOSFODNN7EXAMPLE"},
		{"token ghp_0123456789abcdefghijklmnopqrstuvwxyzAB", "ghp_0123456789"},
		{`curl -H "Authorization: Bearer abc.def.ghijklmnopqrstuvwxyz"`, "abc.def.ghijk"},
		{"postgres://admin:hunter2pass@db.local:5432/app", "hunter2pass"},
		{`{"password": "correct-horse-battery"}`, "correct-horse"},
		{"DB_PASSWORD=s3cr3t-value", "s3cr3t-value"},
		{"-----BEGIN RSA PRIVATE KEY-----\nMIIEow\nabc\n-----END RSA PRIVATE KEY-----", "MIIEow"},
		{"jwt eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PlFUP0THsR8U", "eyJzdWIi"},
	}
	for _, c := range cases {
		out := String(c.in)
		if strings.Contains(out, c.leaked) {
			t.Errorf("String(%q) = %q, still contains %q", c.in, out, c.leaked)
		}
		if !strings.Contains(out, Placeholder) {
			t.Errorf("String(%q) = %q, no placeholder", c.in, out)
		}
	}
}

func TestStringKeepsOrdinaryText(t *testing.T) {
	for _, in := range []string{
		"max_tokens=1500",
		"tokens used: 4,327",
		"git push origin main",
		"func authenticate(user string) error",
		"password: $DB_PASSWORD",
		"see https://github.com/chux0519/memspace",
	} {
		if out := String(in); out != in {
			t.Errorf("String(%q) = %q, want unchanged", in, out)
		}
	}
}

func TestValueRedactsSensitiveKeys(t *testing.T) {
	var v any
	json.Unmarshal([]byte(`{"headers":{"Authorization":"xyz","Accept":"json"},"api_key":"abc123","nested":[{"client_secret":"q"}]}`), &v)
	b, _ := json.Marshal(Value(v))
	s := string(b)
	for _, leaked := range []string{`"xyz"`, `"abc123"`, `"q"`} {
		if strings.Contains(s, leaked) {
			t.Errorf("Value leaked %s: %s", leaked, s)
		}
	}
	if !strings.Contains(s, `"Accept":"json"`) {
		t.Errorf("Value dropped a harmless key: %s", s)
	}
}

func TestJSONAlwaysValid(t *testing.T) {
	for _, in := range []string{`{"a":"DB_PASSWORD=hunter22"}`, `not json at all password=hunter22`, `[]`} {
		out := JSON([]byte(in))
		if !json.Valid(out) {
			t.Errorf("JSON(%q) produced invalid JSON %q", in, out)
		}
		if strings.Contains(string(out), "hunter22") {
			t.Errorf("JSON(%q) leaked: %s", in, out)
		}
	}
}

func TestSensitivePath(t *testing.T) {
	yes := []string{".env", "/app/.env.production", "/home/u/.ssh/config", "certs/server.key", "id_ed25519", "/x/credentials.json", `C:\Users\me\.aws\credentials`}
	no := []string{"main.go", ".env.example", "id_ed25519.pub", "/app/README.md", "keyboard.go", ""}
	for _, p := range yes {
		if !SensitivePath(p) {
			t.Errorf("SensitivePath(%q) = false", p)
		}
	}
	for _, p := range no {
		if SensitivePath(p) {
			t.Errorf("SensitivePath(%q) = true", p)
		}
	}
}

func TestBound(t *testing.T) {
	big := strings.Repeat("x", 100_000)
	v := map[string]any{"out": big, "list": []any{big, "small"}, "img": "data:image/png;base64," + strings.Repeat("A", 500)}
	b, _ := json.Marshal(Bound(v, 8<<10, 16<<10))
	if len(b) > 16<<10 {
		t.Fatalf("bounded payload is %d bytes", len(b))
	}
	if strings.Contains(string(b), "AAAAAAAA") {
		t.Errorf("data URI not stripped")
	}
	if !strings.Contains(string(b), "small") {
		t.Errorf("small string lost")
	}
	if len(v["out"].(string)) != 100_000 {
		t.Errorf("Bound mutated its input")
	}
}

func TestTruncateKeepsHeadAndTail(t *testing.T) {
	s := "问题背景：" + strings.Repeat("日志行\n", 2000) + "请问为什么认证超时？"
	out := Truncate(s, 200)
	if !strings.HasPrefix(out, "问题背景") || !strings.HasSuffix(out, "请问为什么认证超时？") {
		t.Fatalf("head or tail lost: %q", out)
	}
	if !strings.Contains(out, "bytes omitted") || len(out) > 260 {
		t.Errorf("bad marker or length %d: %q", len(out), out)
	}
	if strings.ContainsRune(out, utf8.RuneError) {
		t.Errorf("split a rune: %q", out)
	}
	if Truncate("short", 10) != "short" {
		t.Error("short string changed")
	}
}
