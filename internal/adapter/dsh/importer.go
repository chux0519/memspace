package dsh

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/chux0519/memspace/internal/adapter"
	"github.com/chux0519/memspace/internal/project"
	"github.com/chux0519/memspace/internal/prompt"
	"github.com/chux0519/memspace/internal/spool"
)

// Importer incrementally converts DSH session logs
// (<DSH_HOME>/sessions/<workspace>/<session>/session.v4.jsonl.zstd) into
// MemSpace events.
//
// A log is a sequence of zstd frames, one per append, each holding whole
// JSONL records. Each file keeps the byte offset after the last frame it
// processed, so only new frames are decompressed. Event IDs are derived from
// (session, seq): re-reading a record produces the same ID and the server
// deduplicates it.
type Importer struct {
	DSHHome  string
	StateDir string
	// Since skips records older than this (zero: import everything).
	Since time.Time
	// Reset ignores saved cursors (used by explicit backfills).
	Reset bool

	hints map[string]project.Hint
}

func (im *Importer) Name() string { return "dsh" }

// hint detects the project of a session's start directory, once per import.
func (im *Importer) hint(cwd string) project.Hint {
	if h, ok := im.hints[cwd]; ok {
		return h
	}
	if im.hints == nil {
		im.hints = map[string]project.Hint{}
	}
	h := project.Detect(cwd)
	im.hints[cwd] = h
	return h
}

// LogName is the session log file name DSH 0.2 writes.
const LogName = "session.v4.jsonl.zstd"

type cursor struct {
	Offset  int64     `json:"offset"`
	Size    int64     `json:"size"`
	ModTime time.Time `json:"mod_time"`
	// From the session header.
	SessionID string `json:"session_id,omitempty"`
	Cwd       string `json:"cwd,omitempty"`
	// Parent is set for subagent sessions: their work is recorded in the
	// parent session.
	Parent string `json:"parent,omitempty"`
	// Turn state carried across imports.
	Turn     int64            `json:"turn,omitempty"`
	LastText string           `json:"last_text,omitempty"`
	Calls    map[string]*call `json:"calls,omitempty"`
}

// call is a tool call waiting for its result.
type call struct {
	Name string `json:"name"`
	Args string `json:"args"`
	Turn int64  `json:"turn"`
}

type cursorFile struct {
	Files      map[string]*cursor `json:"files"`
	LastImport time.Time          `json:"last_import"`
}

func (im *Importer) cursorPath() string { return filepath.Join(im.StateDir, "dsh-cursors.json") }

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

// LatestChange returns the newest modification time of any session log.
func (im *Importer) LatestChange() time.Time {
	var latest time.Time
	filepath.WalkDir(filepath.Join(im.DSHHome, "sessions"), func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || d.Name() != LogName {
			return nil
		}
		if fi, err := d.Info(); err == nil && fi.ModTime().After(latest) {
			latest = fi.ModTime()
		}
		return nil
	})
	return latest
}

// Import processes new log records. It stops early (without losing its
// place) when the spool is full.
func (im *Importer) Import(ctx context.Context, sp *spool.Spool, heartbeat func()) (int, error) {
	root := filepath.Join(im.DSHHome, "sessions")
	if _, err := os.Stat(root); err != nil {
		return 0, nil // DSH not used on this machine
	}
	dec, err := zstd.NewReader(nil, zstd.WithDecoderConcurrency(1))
	if err != nil {
		return 0, err
	}
	defer dec.Close()
	cf := im.loadCursors()
	total := 0
	var firstErr error

	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || d.Name() != LogName {
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
			if !im.Since.IsZero() && fi.ModTime().Before(im.Since.Add(-time.Minute)) {
				return nil
			}
			c = &cursor{}
			cf.Files[path] = c
		}
		if fi.Size() == c.Size && fi.ModTime().Equal(c.ModTime) {
			return nil
		}
		if fi.Size() < c.Offset { // rewritten; start over
			*c = cursor{}
		}
		if heartbeat != nil {
			heartbeat()
		}
		n, err := im.importFile(path, c, dec, sp)
		total += n
		if err != nil {
			if errors.Is(err, spool.ErrFull) {
				firstErr = err
				return fs.SkipAll
			}
			if firstErr == nil {
				firstErr = fmt.Errorf("%s: %w", filepath.Base(filepath.Dir(path)), err)
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

var frameMagic = []byte{0x28, 0xb5, 0x2f, 0xfd}

// importFile decodes the frames after c.Offset. Frames are found by their
// magic number; a split that lands inside a frame (the magic bytes can
// occur in compressed data) fails to decode and is merged with the next
// piece. A trailing frame that does not decode is still being written and
// is retried on the next import.
func (im *Importer) importFile(path string, c *cursor, dec *zstd.Decoder, sp *spool.Spool) (int, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	if c.Offset > int64(len(b)) {
		*c = cursor{}
	}
	rest := b[c.Offset:]
	var starts []int
	for i := 0; i < len(rest); {
		j := bytes.Index(rest[i:], frameMagic)
		if j < 0 {
			break
		}
		starts = append(starts, i+j)
		i += j + len(frameMagic)
	}
	if len(starts) == 0 || starts[0] != 0 {
		if len(rest) > 0 {
			return 0, errors.New("not a zstd stream at the saved offset")
		}
		return 0, nil
	}
	starts = append(starts, len(rest))

	n := 0
	from := 0
	for k := 1; k < len(starts); k++ {
		out, err := dec.DecodeAll(rest[starts[from]:starts[k]], nil)
		if err != nil {
			continue // not a frame boundary, or an incomplete last frame
		}
		m, err := im.records(out, c, sp)
		n += m
		if err != nil {
			return n, err
		}
		c.Offset += int64(starts[k] - starts[from])
		from = k
	}
	return n, nil
}

// records maps the JSONL records of one frame.
func (im *Importer) records(data []byte, c *cursor, sp *spool.Spool) (int, error) {
	n := 0
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
	for sc.Scan() {
		var r record
		if json.Unmarshal(sc.Bytes(), &r) != nil {
			continue
		}
		for _, ev := range im.mapRecord(&r, c) {
			if !im.Since.IsZero() && ev.at.Before(im.Since) {
				continue
			}
			sid := c.SessionID
			if c.Parent != "" {
				sid = c.Parent
			}
			rec := spool.Record{
				EventID:         adapter.StableID("dsh", c.SessionID, ev.seq, ev.suffix),
				Agent:           AgentType,
				NativeSessionID: sid,
				EventType:       ev.typ,
				OccurredAt:      ev.at,
				Project:         im.hint(c.Cwd),
				Payload:         adapter.Payload(ev.payload),
			}
			if err := sp.Write(rec); err != nil {
				return n, err
			}
			n++
		}
	}
	return n, sc.Err()
}

// record is one log line. The session header has no data: its fields are
// at the top level.
type record struct {
	Type string          `json:"type"`
	Seq  int64           `json:"seq"`
	Time int64           `json:"time"`
	Data json.RawMessage `json:"data"`
	// Header fields.
	ID              string `json:"id"`
	CreatedAt       int64  `json:"createdAt"`
	Cwd             string `json:"cwd"`
	DelegationDepth int    `json:"delegationDepth"`
	ParentSession   string `json:"parentSession"`
	AgentPreset     string `json:"agentPreset"`
}

type mapped struct {
	typ     string
	seq     string
	suffix  string
	at      time.Time
	payload map[string]any
}

type content []struct {
	Type      string `json:"type"`
	Text      string `json:"text"`
	Name      string `json:"name"`
	MediaType string `json:"mediaType"`
	Path      string `json:"path"`
}

func (cs content) text(kind string) string {
	var parts []string
	for _, p := range cs {
		if p.Type == kind && strings.TrimSpace(p.Text) != "" {
			parts = append(parts, p.Text)
		}
	}
	return strings.Join(parts, "\n")
}

type message struct {
	Role    string  `json:"role"`
	Content content `json:"content"`
	Source  struct {
		Kind string `json:"kind"`
	} `json:"source"`
	ToolCallID string `json:"toolCallId"`
	IsError    bool   `json:"isError"`
}

// fileTools write the file named in their file_path argument.
var fileTools = map[string]bool{"edit": true, "write": true}

// quietTools return file contents or images: their results are not kept.
var quietTools = map[string]bool{"read": true, "read_image": true}

// mapRecord converts one record, updating the cursor's session and turn
// state. Only what the user and agent did is kept: runtime context, skill
// catalogs, request logs and inbox bookkeeping are skipped.
func (im *Importer) mapRecord(r *record, c *cursor) []mapped {
	at := time.UnixMilli(r.Time).UTC()
	seq := strconv.FormatInt(r.Seq, 10)
	turnID := func() string { return "t" + strconv.FormatInt(c.Turn, 10) }
	sub := c.Parent != ""

	switch r.Type {
	case "session":
		c.SessionID, c.Cwd = r.ID, r.Cwd
		if r.DelegationDepth > 0 && r.ParentSession != "" {
			c.Parent = r.ParentSession
			return nil
		}
		return []mapped{{typ: "session.started", seq: "header", at: time.UnixMilli(r.CreatedAt).UTC(),
			payload: map[string]any{"cwd": r.Cwd, "agent_preset": r.AgentPreset}}}
	}
	if c.SessionID == "" {
		return nil // header not seen
	}

	switch r.Type {
	case "turn/start":
		var d struct {
			Turn int64 `json:"turn"`
		}
		json.Unmarshal(r.Data, &d)
		c.Turn, c.LastText = d.Turn, ""
		return nil

	case "user/message":
		var m message
		if json.Unmarshal(r.Data, &m) != nil || m.Source.Kind != "user" || sub {
			return nil
		}
		p := prompt.Payload(m.Content.text("text"))
		p["turn_id"] = turnID()
		if media := attachments(m.Content); len(media) > 0 {
			atts, _ := p["attachments"].([]prompt.Attachment)
			p["attachments"] = append(atts, media...)
		}
		return []mapped{{typ: "user.prompt", seq: seq, at: at, payload: p}}

	case "assistant/message":
		var d struct {
			Message message `json:"message"`
		}
		json.Unmarshal(r.Data, &d)
		if t := d.Message.Content.text("text"); t != "" {
			c.LastText = t
		}
		return nil

	case "turn/end":
		if sub {
			return nil
		}
		var d struct {
			Reason struct {
				Kind string `json:"kind"`
			} `json:"reason"`
		}
		json.Unmarshal(r.Data, &d)
		text := c.LastText
		c.LastText = ""
		if d.Reason.Kind != "" && d.Reason.Kind != "completed" {
			return []mapped{{typ: "agent.turn_aborted", seq: seq, at: at, payload: map[string]any{
				"turn_id": turnID(), "reason": d.Reason.Kind, "last_assistant_message": text,
			}}}
		}
		return []mapped{{typ: "agent.turn_completed", seq: seq, at: at, payload: map[string]any{
			"turn_id": turnID(), "last_assistant_message": text,
		}}}

	case "tool/call":
		var d struct {
			CallID    string `json:"callId"`
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		}
		if json.Unmarshal(r.Data, &d) != nil || d.CallID == "" {
			return nil
		}
		if c.Calls == nil {
			c.Calls = map[string]*call{}
		}
		if len(c.Calls) > 200 { // results that never came
			c.Calls = map[string]*call{}
		}
		c.Calls[d.CallID] = &call{Name: d.Name, Args: d.Arguments, Turn: c.Turn}
		return nil

	case "tool/result":
		var d struct {
			Message message `json:"message"`
		}
		if json.Unmarshal(r.Data, &d) != nil {
			return nil
		}
		cl := c.Calls[d.Message.ToolCallID]
		if cl == nil {
			return nil
		}
		delete(c.Calls, d.Message.ToolCallID)
		return mapTool(cl, d.Message, seq, at)
	}
	return nil
}

var exitCodeRE = regexp.MustCompile(`\[exit code: (-?\d+)\]\s*$`)

func mapTool(cl *call, res message, seq string, at time.Time) []mapped {
	var args map[string]any
	json.Unmarshal([]byte(cl.Args), &args)
	if args == nil {
		args = map[string]any{}
	}
	turnID := "t" + strconv.FormatInt(cl.Turn, 10)
	output := res.Content.text("text")
	str := func(k string) string { s, _ := args[k].(string); return s }

	input := args
	failed := res.IsError
	p := map[string]any{"tool_name": cl.Name, "tool_use_id": res.ToolCallID, "turn_id": turnID}
	switch {
	case cl.Name == "bash":
		cmd := str("command")
		input = map[string]any{"command": cmd, "description": str("description")}
		if m := exitCodeRE.FindStringSubmatch(output); m != nil {
			code, _ := strconv.Atoi(m[1])
			p["exit_code"] = code
			failed = failed || code != 0
		}
		if adapter.MentionsSensitivePath(cmd) {
			output = adapter.OmittedSensitive
		}
	case fileTools[cl.Name]:
		// The content is in the file; keep the path only.
		input = map[string]any{"file_path": str("file_path")}
	case cl.Name == "web_search":
		var qs []string
		if l, ok := args["queries"].([]any); ok {
			for _, q := range l {
				qs = append(qs, fmt.Sprint(q))
			}
		}
		input = map[string]any{"query": strings.Join(qs, "\n")}
	}
	p["tool_input"] = input

	if failed {
		p["error"] = output
		return []mapped{{typ: "tool.failed", seq: seq, at: at, payload: p}}
	}
	if !quietTools[cl.Name] {
		p["tool_response"] = output
	}
	out := []mapped{{typ: "tool.completed", seq: seq, at: at, payload: p}}
	if path := str("file_path"); fileTools[cl.Name] && path != "" {
		out = append(out, mapped{typ: "file.modified", seq: seq, suffix: "file", at: at, payload: map[string]any{
			"path": path, "tool_name": cl.Name, "tool_use_id": res.ToolCallID, "turn_id": turnID,
		}})
	}
	return out
}

// attachments records non-text content (images, files) by metadata only.
func attachments(cs content) []prompt.Attachment {
	var out []prompt.Attachment
	for _, p := range cs {
		if p.Type == "" || p.Type == "text" {
			continue
		}
		a := prompt.Attachment{Kind: "file", Ref: p.Type}
		if strings.Contains(p.Type, "image") || strings.HasPrefix(p.MediaType, "image/") {
			a.Kind = "image"
		}
		for _, s := range []string{p.Path, p.Name} {
			if s != "" {
				a.Ref = s
				break
			}
		}
		out = append(out, a)
	}
	return out
}
