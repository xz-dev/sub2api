//go:build unit

package handler

import (
	"bytes"
	"math/rand"
	"strconv"
	"strings"
)

// Corpus for the charge-covers-allocation test: Go versions of the
// acceptance/ generators, rendered in each route's protocol.
//
//	tiny     1-2 words per message (gen_tiny.py, worst for structure)
//	small    8-12 words per message (gen_small.py)
//	text     ASCII parts of 1-16 KiB (gen_m0.py SHAPE text)
//	utext    parts of 8-60 KiB with \" \t \n escapes and non-ASCII (gen_utext.py)
//	invalid  parts of 8-60 KiB, an 0xFF or truncated 0xC3 byte every ~8 bytes (gen_invalid.py)
//
// Words are stored already JSON-escaped and never cut, so every body is
// valid JSON (apart from the invalid UTF-8 bytes on purpose).
type coverBase struct {
	name          string
	words         []string
	user, asst    [2]int // min, max bytes of a user / assistant text
	parts         bool   // content as a typed text part, not a string
	invalidEveryN int
}

var coverBases = []coverBase{
	{name: "tiny", words: []string{"ok", "yes", "no", "go", "run", "fix", "log", "a", "b", "c"}, user: [2]int{1, 6}, asst: [2]int{1, 6}},
	{name: "small", words: []string{"ok", "yes", "no", "run", "test", "build", "log", "fix", "diff", "file", "path", "retry", "step", "next", "done"}, user: [2]int{40, 70}, asst: [2]int{10, 30}},
	{name: "text", words: []string{"the", "keeper", "spool", "frame", "bytes", "request", "response", "tool", "output", "build", "test", "patch"}, user: [2]int{1 << 10, 16 << 10}, asst: [2]int{512, 4096}, parts: true},
	{name: "utext", words: []string{"the", "keeper", "spool", `\"quoted\"`, `tab\t`, `newline\n`, "größe", "数据", "ünïcödé", "retry"}, user: [2]int{8 << 10, 60 << 10}, asst: [2]int{512, 4096}, parts: true},
	{name: "invalid", words: []string{"the", "keeper", "spool", "frame", "bytes", "build", "test", "patch"}, user: [2]int{8 << 10, 60 << 10}, asst: [2]int{512, 4096}, parts: true, invalidEveryN: 8},
	// rr6: tiny messages with invalid UTF-8 (structure-heavy and invalid).
	{name: "rr6_tinyinvalid", words: []string{"ok", "yes", "no", "go", "run", "fix", "log", "a", "b", "c"}, user: [2]int{1, 6}, asst: [2]int{1, 6}, invalidEveryN: 2},
}

func (s coverBase) text(b *bytes.Buffer, rnd *rand.Rand, user bool) {
	lim := s.asst
	if user {
		lim = s.user
	}
	n := lim[0] + rnd.Intn(lim[1]-lim[0]+1)
	start := b.Len()
	for b.Len()-start < n {
		if b.Len() > start {
			b.WriteByte(' ')
		}
		b.WriteString(s.words[rnd.Intn(len(s.words))])
		if s.invalidEveryN > 0 && (b.Len()-start)%s.invalidEveryN < 4 {
			b.WriteString([]string{"\xff", "\xc3"}[rnd.Intn(2)])
		}
	}
}

// coverProto renders messages in one route protocol.
type coverProto struct {
	head      func(model string) string
	user      func(parts bool) (pre, post string)
	asst      func(parts bool) (pre, post string)
	tail      string
	oneString bool // web_search: all text in one query string
}

var coverProtos = map[string]coverProto{
	"responses": {
		head: func(m string) string { return `"model":"` + m + `","stream":false,"store":false,"input":[` },
		user: func(p bool) (string, string) {
			if p {
				return `{"type":"message","role":"user","content":[{"type":"input_text","text":"`, `"}]}`
			}
			return `{"role":"user","content":"`, `"}`
		},
		asst: func(p bool) (string, string) {
			if p {
				return `{"type":"message","role":"assistant","content":[{"type":"output_text","text":"`, `"}]}`
			}
			return `{"role":"assistant","content":"`, `"}`
		},
		tail: `{"role":"user","content":"last"}]}`,
	},
	"chat": {
		head: func(m string) string { return `"model":"` + m + `","stream":false,"messages":[` },
		user: func(p bool) (string, string) {
			if p {
				return `{"role":"user","content":[{"type":"text","text":"`, `"}]}`
			}
			return `{"role":"user","content":"`, `"}`
		},
		asst: func(bool) (string, string) { return `{"role":"assistant","content":"`, `"}` },
		tail: `{"role":"user","content":"last"}]}`,
	},
	"claude": {
		head: func(m string) string { return `"model":"` + m + `","max_tokens":16,"stream":false,"messages":[` },
		user: func(p bool) (string, string) {
			if p {
				return `{"role":"user","content":[{"type":"text","text":"`, `"}]}`
			}
			return `{"role":"user","content":"`, `"}`
		},
		asst: func(bool) (string, string) { return `{"role":"assistant","content":"`, `"}` },
		tail: `{"role":"user","content":"last"}]}`,
	},
	"gemini": {
		head: func(string) string { return `"contents":[` },
		user: func(bool) (string, string) { return `{"role":"user","parts":[{"text":"`, `"}]}` },
		asst: func(bool) (string, string) { return `{"role":"model","parts":[{"text":"`, `"}]}` },
		tail: `{"role":"user","parts":[{"text":"last"}]}]}`,
	},
	"websearch": {
		head: func(string) string { return `"query":"` }, tail: `"}`, oneString: true,
		user: func(bool) (string, string) { return "", " " }, asst: func(bool) (string, string) { return "", " " },
	},
}

// coverBody renders base s in protocol p, about size bytes; root is spliced
// in first in the root object, item (a JSON value) goes before the last
// message.
func coverBody(p coverProto, s coverBase, size int, model, root, item string) []byte {
	var b bytes.Buffer
	b.Grow(size + 64<<10)
	b.WriteString(`{` + root + p.head(model))
	rnd := rand.New(rand.NewSource(int64(len(s.name)) * 31))
	stop := size - len(p.tail) - len(item) - 1 - max(s.user[1], s.asst[1]) - 64
	for i := 0; b.Len() < stop; i++ {
		if i > 0 && !p.oneString {
			b.WriteByte(',')
		}
		pre, post := p.asst(s.parts)
		if i%2 == 0 {
			pre, post = p.user(s.parts)
		}
		b.WriteString(pre)
		s.text(&b, rnd, i%2 == 0)
		b.WriteString(post)
	}
	if !p.oneString {
		b.WriteByte(',')
		if item != "" {
			b.WriteString(item + ",")
		}
	}
	b.WriteString(p.tail)
	return b.Bytes()
}

// coverMods are the shapes that make a normalizer or rewrite act, on the
// tiny base; proto names the protocol each applies to. codex sends a Codex
// client with the image-generation bridge on and images allowed. header is
// sent with the request (and with the priming request). prime names the
// per-session state set up first with a small request of the same shape
// and session (see coverPrimes).
type coverMod struct {
	proto, name, root, item, model string
	codex                          bool
	header                         map[string]string
	prime                          string
}

var coverMods = []coverMod{
	{proto: "responses", name: "compaction_trigger", item: `{"type":"compaction_trigger"}`},
	{proto: "responses", name: "dupkey", root: `"store":false,`},
	{proto: "responses", name: "parallel_tool_calls", root: `"parallel_tool_calls":true,`},
	{proto: "responses", name: "json_schema", root: `"text":{"format":{"type":"json_schema","name":"r","schema":{"type":"object","properties":{}}}},`},
	{proto: "responses", name: "image_generation", root: `"tools":[{"type":"image_generation","output_format":"png"}],`, codex: true},
	{proto: "responses", name: "gpt-image", model: "gpt-image-1"},
	// Handler Codex bootstrap normalizers (an automation candidate is
	// rewritten; any codex_tui item is decoded).
	{proto: "responses", name: "codex_app", item: `{"type":"function_call_output","namespace":"codex_app","name":"automation_update","output":"<heartbeat><automation_id>wiki</automation_id></heartbeat>"}`},
	{proto: "responses", name: "codex_tui", item: `{"type":"message","role":"user","namespace":"codex_tui","content":"x"}`},
	// Responses-Lite: any client may send the header.
	{proto: "responses", name: "responses_lite", header: map[string]string{"X-OpenAI-Internal-Codex-Responses-Lite": "true"}},
	// A session an earlier invalid_encrypted_content retry marked, whose
	// next request still carries the rejected item.
	{proto: "responses", name: "encrypted_lineage", item: `{"type":"reasoning","summary":[],"encrypted_content":"enc-cover-rejected"}`,
		header: map[string]string{"session_id": "cover-lineage-session"}, prime: "invalid_encrypted_content"},
	// A namespace tool declaration (OAuth flattens it).
	{proto: "responses", name: "namespace_tools", root: `"tools":[{"type":"namespace","name":"ns","description":"d","tools":[{"type":"function","name":"f","description":"d","parameters":{"type":"object","properties":{}}}]}],`},
	// rr6 reviewer additions.
	// A replayed reasoning item with content (Forward: reasoning content replay).
	{proto: "responses", name: "rr6_reasoning_content", item: `{"type":"reasoning","summary":[],"encrypted_content":"enc-cover-ok","content":[{"type":"reasoning_text","text":"t"}]}`},
	// Codex freeform (custom) tool and its call/output pair.
	{proto: "responses", name: "rr6_custom_tools", root: `"tools":[{"type":"custom","name":"apply_patch","description":"d","format":{"type":"grammar","syntax":"lark","definition":"start: /.+/"}}],`,
		item: `{"type":"custom_tool_call","call_id":"c1","name":"apply_patch","input":"x"},{"type":"custom_tool_call_output","call_id":"c1","output":"ok"}`},
	// A tool schema with a null root type and a lookahead pattern.
	{proto: "responses", name: "rr6_tool_schema", root: `"tools":[{"type":"function","name":"f","description":"d","parameters":{"type":null,"properties":{"a":{"type":"string","pattern":"^(?=a).*$"}}}}],`},
	// A gzip-encoded body (fallback path, charged on decoded bytes).
	{proto: "responses", name: "rr6_gzip", header: map[string]string{"Content-Encoding": "gzip"}},
	{proto: "claude", name: "rr6_gzip", header: map[string]string{"Content-Encoding": "gzip"}},
	{proto: "chat", name: "rr6_gzip", header: map[string]string{"Content-Encoding": "gzip"}},
	// A base64 input image (tier-A media).
	{proto: "responses", name: "rr6_input_image", item: `{"type":"message","role":"user","content":[{"type":"input_image","image_url":"data:image/png;base64,` + strings.Repeat("QUJD", 256<<10) + `"}]}`},
	// An empty base64 input image (sanitized before the send).
	{proto: "responses", name: "rr6_empty_image", item: `{"type":"message","role":"user","content":[{"type":"input_image","image_url":"data:image/png;base64,"}]}`},
	{proto: "chat", name: "dupkey", root: `"stream":false,`},
	{proto: "chat", name: "parallel_tool_calls", root: `"parallel_tool_calls":true,`},
	{proto: "chat", name: "json_schema", root: `"response_format":{"type":"json_schema","json_schema":{"name":"r","schema":{"type":"object","properties":{}}}},`},
	{proto: "chat", name: "gpt-image", model: "gpt-image-1"},
}

func coverSize(n int) string {
	if n%(1<<20) == 0 {
		return strconv.Itoa(n>>20) + "MiB"
	}
	return strconv.Itoa(n>>10) + "KiB"
}
