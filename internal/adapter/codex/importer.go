package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/chux0519/memspace/internal/adapter"
	"github.com/chux0519/memspace/internal/project"
	"github.com/chux0519/memspace/internal/prompt"
	"github.com/chux0519/memspace/internal/spool"
)

// Importer incrementally converts Codex rollout files
// (<CODEX_HOME>/sessions/YYYY/MM/DD/rollout-*.jsonl) into MemSpace events.
//
// Rollouts are append-only JSONL with a per-line ordinal, so each file keeps
// a byte-offset cursor and event IDs are derived from (session, ordinal):
// re-reading a line after a crash produces the same event ID and the server
// deduplicates it.
type Importer struct {
	CodexHome string
	StateDir  string
	// Since skips rollout lines older than this (zero: import everything).
	Since time.Time
	// Reset ignores saved cursors (used by explicit backfills).
	Reset bool
}

func (im *Importer) Name() string { return "codex" }

type cursor struct {
	Offset    int64     `json:"offset"`
	Size      int64     `json:"size"`
	ModTime   time.Time `json:"mod_time"`
	SessionID string    `json:"session_id,omitempty"`
	Cwd       string    `json:"cwd,omitempty"`
}

type cursorFile struct {
	Files      map[string]*cursor `json:"files"`
	LastImport time.Time          `json:"last_import"`
}

func (im *Importer) cursorPath() string { return filepath.Join(im.StateDir, "codex-cursors.json") }

func (im *Importer) loadCursors() *cursorFile {
	cf := &cursorFile{}
	if b, err := os.ReadFile(im.cursorPath()); err == nil {
		json.Unmarshal(b, cf)
	}
	if cf.Files == nil || im.Reset {
		cf.Files = map[string]*cursor{}
	}
	return cf
}

func (im *Importer) saveCursors(cf *cursorFile) error {
	b, err := json.MarshalIndent(cf, "", " ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(im.StateDir, 0o700); err != nil {
		return err
	}
	tmp := im.cursorPath() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, im.cursorPath())
}

// Stats reports importer progress for doctor/status.
func (im *Importer) Stats() (files int, lastImport time.Time) {
	cf := im.loadCursors()
	return len(cf.Files), cf.LastImport
}

// Import processes new rollout lines. It stops early (without losing its
// place) when the spool is full.
func (im *Importer) Import(ctx context.Context, sp *spool.Spool, heartbeat func()) (int, error) {
	root := filepath.Join(im.CodexHome, "sessions")
	if _, err := os.Stat(root); err != nil {
		return 0, nil // Codex not used on this machine
	}
	cf := im.loadCursors()
	total := 0
	var firstErr error

	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasPrefix(d.Name(), "rollout-") || !strings.HasSuffix(d.Name(), ".jsonl") {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		fi, err := d.Info()
		if err != nil {
			return nil
		}
		c := cf.Files[path]
		if c == nil {
			// Files untouched since the import cutoff hold nothing to import.
			// The grace period absorbs coarse filesystem timestamps; the
			// per-line timestamp check applies the exact cutoff.
			if !im.Since.IsZero() && fi.ModTime().Before(im.Since.Add(-time.Minute)) {
				return nil
			}
			c = &cursor{}
			cf.Files[path] = c
		}
		if fi.Size() == c.Size && fi.ModTime().Equal(c.ModTime) {
			return nil
		}
		if fi.Size() < c.Offset { // rewritten (e.g. migrated); start over
			*c = cursor{}
		}
		n, err := im.importFile(path, c, sp, heartbeat)
		total += n
		if err != nil {
			if errors.Is(err, spool.ErrFull) {
				firstErr = err
				return fs.SkipAll
			}
			if firstErr == nil {
				firstErr = fmt.Errorf("%s: %w", filepath.Base(path), err)
			}
			return nil
		}
		c.Size, c.ModTime = fi.Size(), fi.ModTime()
		return nil
	})
	cf.LastImport = time.Now().UTC()
	if err := im.saveCursors(cf); err != nil && firstErr == nil {
		firstErr = err
	}
	if walkErr != nil && firstErr == nil {
		firstErr = walkErr
	}
	return total, firstErr
}

// rolloutLine is the envelope of every rollout line.
type rolloutLine struct {
	Timestamp time.Time       `json:"timestamp"`
	Ordinal   *int64          `json:"ordinal"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
}

func (im *Importer) importFile(path string, c *cursor, sp *spool.Spool, heartbeat func()) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	// The session header (line 0) carries the session id and cwd; read it
	// even when resuming mid-file, in case the cursor predates it.
	if c.SessionID == "" {
		if err := readMeta(f, c); err != nil {
			return 0, err
		}
	}
	if c.SessionID == "" {
		return 0, nil // header not written yet; retried when the file grows
	}
	if _, err := f.Seek(c.Offset, io.SeekStart); err != nil {
		return 0, err
	}
	hint := project.Detect(c.Cwd)
	r := bufio.NewReaderSize(f, 1<<20)
	n := 0
	offset := c.Offset
	for lineNo := 0; ; lineNo++ {
		line, err := r.ReadBytes('\n')
		if err == io.EOF {
			// A trailing line without '\n' is still being written.
			return n, nil
		}
		if err != nil {
			return n, err
		}
		lineStart := offset
		offset += int64(len(line))
		if lineNo%2000 == 0 && heartbeat != nil {
			heartbeat()
		}

		var rl rolloutLine
		if json.Unmarshal(line, &rl) != nil {
			c.Offset = offset // skip corrupt line
			continue
		}
		if !im.Since.IsZero() && !rl.Timestamp.IsZero() && rl.Timestamp.Before(im.Since) {
			c.Offset = offset
			continue
		}
		seq := strconv.FormatInt(lineStart, 10)
		if rl.Ordinal != nil {
			seq = "o" + strconv.FormatInt(*rl.Ordinal, 10)
		}
		for _, ev := range mapLine(&rl) {
			rec := spool.Record{
				EventID:         adapter.StableID("cx", c.SessionID, seq, ev.suffix),
				Agent:           AgentType,
				NativeSessionID: c.SessionID,
				EventType:       ev.typ,
				OccurredAt:      rl.Timestamp,
				Project:         hint,
				Payload:         adapter.Payload(ev.payload),
			}
			if err := sp.Write(rec); err != nil {
				return n, err // cursor stays before this line; it is retried
			}
			n++
		}
		c.Offset = offset
	}
}

func readMeta(f *os.File, c *cursor) error {
	r := bufio.NewReaderSize(f, 1<<20)
	line, err := r.ReadBytes('\n')
	if err != nil && err != io.EOF {
		return err
	}
	var rl rolloutLine
	if json.Unmarshal(line, &rl) != nil || rl.Type != "session_meta" {
		return nil
	}
	var meta struct {
		ID        string `json:"id"`
		SessionID string `json:"session_id"`
		Cwd       string `json:"cwd"`
	}
	json.Unmarshal(rl.Payload, &meta)
	c.SessionID = meta.SessionID
	if c.SessionID == "" {
		c.SessionID = meta.ID
	}
	c.Cwd = meta.Cwd
	return nil
}

type mapped struct {
	typ     string
	suffix  string // distinguishes several events from one line
	payload map[string]any
}

// mapLine converts one rollout line. It uses the semantic item stream
// (event_msg/item_completed) rather than raw model I/O: it is what the user
// saw, without injected instructions or encrypted reasoning.
func mapLine(rl *rolloutLine) []mapped {
	switch rl.Type {
	case "session_meta":
		var m map[string]any
		json.Unmarshal(rl.Payload, &m)
		return []mapped{{typ: "session.started", payload: pick(m,
			"cwd", "originator", "cli_version", "source", "model_provider", "thread_source")}}
	case "compacted":
		return []mapped{{typ: "session.compacted", payload: map[string]any{}}}
	case "event_msg":
	default:
		return nil
	}

	var ev map[string]any
	if json.Unmarshal(rl.Payload, &ev) != nil {
		return nil
	}
	turnID, _ := ev["turn_id"].(string)
	switch ev["type"] {
	case "task_complete":
		return []mapped{{typ: "agent.turn_completed", payload: map[string]any{
			"turn_id": turnID, "last_assistant_message": ev["last_agent_message"], "duration_ms": ev["duration_ms"],
		}}}
	case "turn_aborted":
		return []mapped{{typ: "agent.turn_aborted", payload: map[string]any{
			"turn_id": turnID, "reason": ev["reason"],
		}}}
	case "user_message": // older rollout format
		msg, _ := ev["message"].(string)
		p := prompt.Payload(msg)
		p["turn_id"] = turnID
		return []mapped{{typ: "user.prompt", payload: p}}
	case "item_completed":
		item, _ := ev["item"].(map[string]any)
		if item == nil {
			return nil
		}
		return mapItem(item, turnID)
	}
	return nil
}

func mapItem(item map[string]any, turnID string) []mapped {
	id, _ := item["id"].(string)
	status, _ := item["status"].(string)
	switch item["type"] {
	case "UserMessage":
		p := prompt.Payload(contentText(item["content"]))
		p["turn_id"] = turnID
		if media := mediaAttachments(item["content"]); len(media) > 0 {
			atts, _ := p["attachments"].([]prompt.Attachment)
			p["attachments"] = append(atts, media...)
		}
		return []mapped{{typ: "user.prompt", payload: p}}
	case "AgentMessage":
		if item["phase"] != "final_answer" {
			return nil // commentary is progress chatter
		}
		return []mapped{{typ: "agent.message", payload: map[string]any{
			"text": contentText(item["content"]), "turn_id": turnID,
		}}}
	case "CommandExecution":
		cmd := commandString(item["command"])
		exit, _ := item["exit_code"].(float64)
		p := map[string]any{
			"tool_name": "shell", "tool_use_id": id, "turn_id": turnID,
			"tool_input": map[string]any{"command": cmd, "cwd": strings.TrimPrefix(fmt.Sprint(item["cwd"]), "file://")},
			"exit_code":  item["exit_code"],
			"duration":   item["duration"],
			"status":     status,
		}
		output := item["aggregated_output"]
		if adapter.MentionsSensitivePath(cmd) {
			output = adapter.OmittedSensitive
		}
		typ := "tool.completed"
		if status == "failed" || status == "declined" || exit != 0 {
			typ = "tool.failed"
			p["error"] = output
		} else {
			p["tool_response"] = output
		}
		return []mapped{{typ: typ, payload: p}}
	case "FileChange":
		changes, _ := item["changes"].(map[string]any)
		var files []map[string]any
		for path, ch := range changes {
			kind := ""
			if m, ok := ch.(map[string]any); ok {
				kind, _ = m["type"].(string)
			}
			files = append(files, map[string]any{"path": path, "change": kind})
		}
		typ := "file.modified"
		if status == "failed" || status == "declined" {
			typ = "tool.failed"
		}
		return []mapped{{typ: typ, payload: map[string]any{
			"tool_name": "apply_patch", "tool_use_id": id, "turn_id": turnID, "files": files, "status": status,
		}}}
	case "McpToolCall":
		typ := "tool.completed"
		if status == "failed" {
			typ = "tool.failed"
		}
		return []mapped{{typ: typ, payload: map[string]any{
			"tool_name":   fmt.Sprintf("mcp:%v/%v", item["server"], item["tool"]),
			"tool_use_id": id, "turn_id": turnID, "status": status,
			"tool_input": item["arguments"], "tool_response": item["result"],
		}}}
	case "Extension":
		kind, _ := item["kind"].(string)
		return []mapped{{typ: "tool.completed", payload: map[string]any{
			"tool_name": kind, "tool_use_id": id, "turn_id": turnID,
			"tool_input": map[string]any{"query": item["query"], "action": item["action"]},
		}}}
	case "ContextCompaction":
		return []mapped{{typ: "session.compacted", payload: map[string]any{"turn_id": turnID}}}
	}
	return nil
}

func pick(m map[string]any, keys ...string) map[string]any {
	out := map[string]any{}
	for _, k := range keys {
		if v, ok := m[k]; ok {
			out[k] = v
		}
	}
	return out
}

// contentText joins the text parts of a Codex content array.
func contentText(v any) string {
	parts, _ := v.([]any)
	var b strings.Builder
	for _, p := range parts {
		m, _ := p.(map[string]any)
		if t, ok := m["text"].(string); ok {
			if b.Len() > 0 {
				b.WriteString("\n")
			}
			b.WriteString(t)
		}
	}
	if b.Len() == 0 {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return b.String()
}

// mediaAttachments records non-text content items (images, files) by
// metadata only; inline data is never kept.
func mediaAttachments(v any) []prompt.Attachment {
	parts, _ := v.([]any)
	var out []prompt.Attachment
	for _, p := range parts {
		m, _ := p.(map[string]any)
		typ, _ := m["type"].(string)
		if m == nil || typ == "" || strings.EqualFold(typ, "text") || typ == "input_text" {
			continue
		}
		a := prompt.Attachment{Kind: "image"}
		if !strings.Contains(strings.ToLower(typ), "image") {
			a.Kind = "file"
		}
		for _, k := range []string{"path", "file_path", "name", "filename"} {
			if s, ok := m[k].(string); ok && s != "" {
				a.Ref = s
				break
			}
		}
		if a.Ref == "" {
			a.Ref = typ
		}
		out = append(out, a)
	}
	return out
}

// commandString renders ["/bin/bash","-lc","echo hi"] as "echo hi".
func commandString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case []any:
		var parts []string
		for _, p := range t {
			parts = append(parts, fmt.Sprint(p))
		}
		if len(parts) == 3 && (parts[1] == "-lc" || parts[1] == "-c") {
			return parts[2]
		}
		return strings.Join(parts, " ")
	}
	return ""
}
