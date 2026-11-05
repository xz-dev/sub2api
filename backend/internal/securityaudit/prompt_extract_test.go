package securityaudit

import (
	"encoding/json"
	"errors"
	"math/rand"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

var parityProtocols = []string{
	"openai_responses", "responses_websocket", "openai_chat_completions", "anthropic_messages",
	"gemini", "media", "typesafe_systemone", "", "other",
}

// requireSnapshotParity: the in-place extraction gives the legacy snapshot
// (json into any) for req, in both modes, or the same kind of error.
func requireSnapshotParity(t *testing.T, req Request, label string) {
	t.Helper()
	for _, latest := range []bool{false, true} {
		got, gotErr := ExtractBlockingPromptSnapshot(req, latest)
		want, wantErr := legacyExtractPromptSnapshot(req, latest)
		if wantErr != nil || gotErr != nil {
			require.Error(t, gotErr, "%s latest=%v", label, latest)
			require.Error(t, wantErr, "%s latest=%v: %v", label, latest, gotErr)
			require.Equal(t, errors.Is(wantErr, ErrNoPromptText), errors.Is(gotErr, ErrNoPromptText), "%s latest=%v", label, latest)
			continue
		}
		require.Equal(t, want.ScanText, got.ScanText, "%s latest=%v", label, latest)
		require.Equal(t, want.PromptHash, got.PromptHash, "%s latest=%v", label, latest)
		require.Equal(t, want.PromptLength, got.PromptLength, "%s latest=%v", label, latest)
		require.Equal(t, want.MessageCount, got.MessageCount, "%s latest=%v", label, latest)
		require.Equal(t, want.FullPrompt, got.FullPrompt, "%s latest=%v", label, latest)
		require.Equal(t, want.RedactedPreview, got.RedactedPreview, "%s latest=%v", label, latest)
	}
}

// TestPromptExtractionMatchesLegacy: hand-written edge cases, the cost
// shapes and random documents over the extractors' keys, for every protocol.
func TestPromptExtractionMatchesLegacy(t *testing.T) {
	bodies := []string{
		`{"input":"hi"}`, `{"input":null,"instructions":" sys "}`, `{"input":[null,1,"a",[],{"text":"t"}]}`,
		`{"input":[{"role":"user","content":"a"},{"role":"user","content":"b","role":"bot"}]}`,
		`{"input":[{"role":"USER","content":[{"type":"Input_Text","text":" x "},{"type":"image","text":"no"},"s",{"text":"y"}]}]}`,
		`{"input":[{"role":"assİstant","content":"İ"},{"role":"\u212aey","content":"k"},{"role":" tool ","content":null,"text":"z"}]}`,
		`{"input":[{"role":"user","content":"a","content":"b"},{"text":" ","content":" "}]}`,
		`{"type":"response.create","response":{"instructions":"i","input":[{"content":"c"}]}}`,
		`{"type":"response.create","input":null,"instructions":[{"text":"i"}]}`, `{"type":"x","input":"a"}`,
		`{"\u0069nput":[{"r\u006fle":"\u0075ser","c\u006fntent":"\ud800\u0041 \ud83d\ude00"}]}`,
		`{"messages":[{"role":"user","content":"m"},{"role":"model","content":"no"},{"role":"tool","content":[{"text":"t"}]}],"system":"s"}`,
		`{"messages":[{"role":"user","content":"   "}],"input":"later"}`, `{"messages":[],"input":"later"}`,
		`{"contents":[{"role":"model","parts":[{"text":"m"}]},{"parts":[{"text":"u"},{"text":" "}]}],"systemInstruction":{"parts":[{"text":"si"}]}}`,
		`{"system_instruction":[{"parts":[{"text":"sys"}]}],"content":{"role":"user","parts":[{"text":"c"}]},"instances":[{"prompt":"p"},1]}`,
		`{"requests":[{"systemInstruction":"s","contents":[{"parts":[{"text":"r"}]}]},{"instances":[{"prompt":"q"}]}]}`,
		`{"systemInstruction":{"text":"t"},"contents":{"parts":[{"text":"one"}]}}`,
		`{"prompt":"draw","negative_prompt":"Draw","b":{"Prompt":["x","draw"," "]},"input":"https://img","image_prompt":"data:image/png"}`,
		`{"prompt":"` + strings.Repeat("QUJD", 70) + `","Final-ZH-Prompt":"f","\u0070rompt":"last"}`,
		`{"questions":{"q2":{"type":"t","criteria":"c","x":"y"},"q1":"plain"},"state":{"b":1,"a":"s"},"model":"m","ext":["e",{"k":"v"}]}`,
		`{"questions":["a",{"b":"c"}],"state":"st","z":"zz","z":"last"}`,
		`[1,2]`, `"text"`, `null`, `{}`, `{"input":1e400}`, `{"input":"a"} x`, `{"input":"\xff"}`, `{"a":1,}`, ``, `  {"input":"ws"}  `,
		// invalid UTF-8 (review rr3 blocker 1): each invalid byte decodes to U+FFFD
		"{\"input\":\"hello \xff world\"}", "{\"input\":\"\xff\xfe\"}",
		"{\"input\":[{\"role\":\"user\",\"content\":\"a\xff\xfe\"},{\"role\":\"assistant\",\"content\":\"\xc3\"}]}",
		"{\"messages\":[{\"role\":\"us\xffer\",\"content\":\"m\"},{\"role\":\"user\",\"content\":\"\\u00e9\xed\xa0\x80\\n\xe2\x82\"}]}",
		"{\"questions\":{\"\xff\":\"x\",\"\xfe\":\"y\"},\"state\":\"s\xff\"}", "{\"prompt\":\"p\xff\",\"\xffprompt\":\"q\"}",
	}
	for _, body := range bodies {
		for _, protocol := range parityProtocols {
			requireSnapshotParity(t, Request{Protocol: protocol, Body: []byte(body)}, protocol+" "+body)
		}
	}
	for name, body := range guardShapeBodies(16 << 10) {
		for _, protocol := range parityProtocols {
			requireSnapshotParity(t, Request{Protocol: protocol, Body: body}, protocol+" "+name)
		}
	}
	rng := rand.New(rand.NewSource(6))
	for i := 0; i < 6000; i++ {
		body := randomGuardDocument(rng, 0)
		if i%2 == 1 {
			body = randomProtocolDocument(rng)
		}
		for _, protocol := range parityProtocols {
			requireSnapshotParity(t, Request{Protocol: protocol, Body: []byte(body)}, protocol+" "+body)
		}
	}
}

var (
	guardRootKeys = []string{"input", "messages", "instructions", "system", "contents", "content", "systemInstruction",
		"system_instruction", "requests", "instances", "prompt", "questions", "state", "type", "response", "\\u0069nput"}
	guardDocKeys = []string{"role", "content", "text", "type", "input", "messages", "instructions", "system", "response",
		"parts", "contents", "systemInstruction", "system_instruction", "instances", "requests", "prompt", "Prompt",
		"input-prompt", "questions", "state", "model", "stream", "criteria", "q", "\\u0072ole", "c\\u006fntent", "x",
		"\xffk", "\xfek"}
	guardDocStrings = []string{"user", "assistant", "model", "tool", "system", "developer", "USER", " user ", "bot",
		"input_text", "output_text", "text", "image_url", "response.create", "", " ", "hello", "a\\nb", "\\u00e9t\\u00e9",
		"\\ud800x", "https://x", "data:image/png", "İnput_text", "\\u212a", "same", "same", "\\u0000z",
		// invalid UTF-8: json.Unmarshal makes each invalid byte U+FFFD
		"\xff", "a\xc3\xa9\xc3", "\xed\xa0\x80 ", "us\xffer", "\\u00e9\xfe\\n"}
)

// randomGuardDocument is a random JSON document over the extractors' keys
// and values, with duplicate and escaped names.
func randomGuardDocument(rng *rand.Rand, depth int) string {
	k := rng.Intn(10)
	if depth == 0 {
		k = 0
	} else if depth == 1 && k < 6 {
		k = 4 // the root's members are mostly lists
	}
	switch {
	case depth < 6 && k < 4:
		var b strings.Builder
		b.WriteByte('{')
		for n := 1 + rng.Intn(4); n > 0; n-- {
			if b.Len() > 1 {
				b.WriteByte(',')
			}
			keys := guardDocKeys
			if depth == 0 || (depth == 2 && rng.Intn(3) == 0) {
				keys = guardRootKeys
			}
			b.WriteString(`"` + keys[rng.Intn(len(keys))] + `":`)
			b.WriteString(randomGuardDocument(rng, depth+1))
		}
		b.WriteByte('}')
		return b.String()
	case depth < 6 && k < 6:
		items := make([]string, rng.Intn(4))
		for i := range items {
			items[i] = randomGuardDocument(rng, depth+1)
		}
		return "[" + strings.Join(items, ",") + "]"
	case k < 9:
		return `"` + guardDocStrings[rng.Intn(len(guardDocStrings))] + `"`
	default:
		return []string{"1", "-2.5e3", "true", "null"}[rng.Intn(4)]
	}
}

// TestJSONUnquoteMatchesUnmarshal: appendJSONUnquote decodes like
// json.Unmarshal (escapes, surrogate pairs, unpaired surrogates).
func TestJSONUnquoteMatchesUnmarshal(t *testing.T) {
	pieces := []string{"a", "é", "😀", `\"`, `\\`, `\/`, `\b`, `\f`, `\n`, `\r`, `\t`, `\u0041`, `\u00e9`, `\uD83D\uDE00`,
		`\ud800`, `\udc00`, `\ud800\u0041`, `\ud800\ud800\udc00`, `\uDBFF\uDFFF`, `\u0000`, " "}
	rng := rand.New(rand.NewSource(7))
	for i := 0; i < 5000; i++ {
		var b strings.Builder
		b.WriteByte('"')
		for n := rng.Intn(8); n > 0; n-- {
			b.WriteString(pieces[rng.Intn(len(pieces))])
		}
		b.WriteByte('"')
		lit := b.String()
		var want string
		require.NoError(t, json.Unmarshal([]byte(lit), &want), lit)
		require.Equal(t, want, string(appendJSONUnquote(nil, []byte(lit))), lit)
		require.True(t, jsonNameIs(jsonRaw(lit), want) || len(lit)-2 > 6*len(want) || len(lit)-2 > 128, lit)
	}
}

// TestValidJSONDocumentMatchesUnmarshal: the guard accepts exactly the
// documents json.Unmarshal into any accepts.
func TestValidJSONDocumentMatchesUnmarshal(t *testing.T) {
	for _, doc := range []string{`{}`, `[]`, `1e308`, `1.8e308`, `-1e400`, `{"a":[1e-400]}`, `"1e999"`,
		`1` + strings.Repeat("0", 400), `123456789012345678901234567890`, `{"a":1,"a":2}`, `"\xff"`, `"\ud800"`,
		`{"a":1}x`, ` [true, false, null] `, ``, strings.Repeat("[", 10000) + strings.Repeat("]", 10000),
		strings.Repeat("[", 10001) + strings.Repeat("]", 10001)} {
		var v any
		require.Equal(t, json.Unmarshal([]byte(doc), &v) == nil, validJSONDocument([]byte(doc)), "%.40q", doc)
	}
}

// TestIsMediaPromptKeyMatchesLegacy: the allocation-free key check equals
// the legacy lowercase-and-replace one.
func TestIsMediaPromptKeyMatchesLegacy(t *testing.T) {
	pieces := []string{"p", "P", "rompt", "ROMPT", "_", "-", " ", "İ", "\u212a", "input", "Negative", "description",
		"final", "zh", "en", "é", "gpt", "query", "lyrics", "image"}
	rng := rand.New(rand.NewSource(8))
	for i := 0; i < 20000; i++ {
		var b strings.Builder
		for n := rng.Intn(6); n > 0; n-- {
			b.WriteString(pieces[rng.Intn(len(pieces))])
		}
		key := b.String()
		require.Equal(t, legacyIsMediaPromptKey(key), isMediaPromptKey(key), "%q", key)
	}
	for _, key := range []string{"prompt", " Final_ZH-Prompt ", "inputprompt", strings.Repeat("prompt", 10), "lyrİcs"} {
		require.Equal(t, legacyIsMediaPromptKey(key), isMediaPromptKey(key), "%q", key)
	}
}

// guardShapeBodies are bodies of about n bytes whose cost is per JSON value
// or per text, not per byte of text (follow-up 6): many small items, bare
// one-letter strings, escaped names and strings, wide objects for the sorted
// walks, empty objects, and deep nesting. Keys are protocols.
func guardShapeBodies(n int) map[string][]byte {
	repeat := func(head, item, tail string) []byte {
		var b strings.Builder
		b.WriteString(head)
		for i := 0; b.Len() < n; i++ {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(strings.ReplaceAll(item, "#", strconv.Itoa(i)))
		}
		b.WriteString(tail)
		return []byte(b.String())
	}
	return map[string][]byte{
		"openai_responses/small-items": repeat(`{"input":[`, `{"type":"message","role":"user","content":[{"type":"input_text","text":"ok #"}]},{"role":"assistant","content":"r"}`, `]}`),
		"openai_responses/bare":        repeat(`{"input":[`, `"a"`, `]}`),
		"openai_responses/bare-escape": repeat(`{"input":[`, `"\u0061"`, `]}`),
		"openai_responses/roles":       repeat(`{"input":[`, `{"role":"\u0075ser","content":"\u0062","text":"c"}`, `]}`),
		"openai_responses/empty":       repeat(`{"input":[`, `{}`, `]}`),
		"openai_chat/tiny":             repeat(`{"messages":[`, `{"role":"user","content":"a"}`, `]}`),
		"gemini/tiny":                  repeat(`{"contents":[`, `{"parts":[{"text":"a"},{"text":"b"}]}`, `]}`),
		"media/distinct":               repeat(`{"prompt":[`, `"#"`, `]}`),
		"media/dups":                   repeat(`{"prompt":[`, `"a"`, `]}`),
		"media/wide":                   repeat(`{`, `"#":"x"`, `,"prompt":"p"}`),
		"media/escaped-keys":           repeat(`{`, `"\u0070rompt#":{"prompt":"#"}`, `}`),
		"typesafe_systemone/wide":      repeat(`{"questions":{"q":{`, `"#":"v"`, `}}}`),
		"typesafe_systemone/nested":    repeat(`{"state":[`, `{"#":{"a":"b"}}`, `]}`),
		"default/deep":                 []byte(strings.Repeat(`{"prompt":[`, 4000) + `"x"` + strings.Repeat(`]}`, 4000)),
	}
}

// randomProtocolDocument is a random Responses, chat/Anthropic or Gemini
// shaped document: roles, content forms, missing, null and duplicate
// members, and now and then a random value in place of a known one.
func randomProtocolDocument(rng *rand.Rand) string {
	str := func() string { return `"` + guardDocStrings[rng.Intn(len(guardDocStrings))] + `"` }
	obj := func(members ...string) string {
		var kept []string
		for i := 0; i+1 < len(members); i += 2 {
			if rng.Intn(5) > 0 {
				kept = append(kept, `"`+members[i]+`":`+members[i+1])
			}
			if rng.Intn(12) == 0 { // a duplicate name: the last one counts
				kept = append(kept, `"`+members[i]+`":`+str())
			}
		}
		return "{" + strings.Join(kept, ",") + "}"
	}
	list := func(item func() string) string {
		items := make([]string, rng.Intn(6))
		for i := range items {
			items[i] = item()
		}
		return "[" + strings.Join(items, ",") + "]"
	}
	maybe := func(v string) string {
		if rng.Intn(10) == 0 {
			return randomGuardDocument(rng, 3)
		}
		return v
	}
	part := func() string { return maybe(obj("type", str(), "text", str())) }
	content := func() string {
		switch rng.Intn(4) {
		case 0:
			return str()
		case 1:
			return list(part)
		case 2:
			return part()
		}
		return "null"
	}
	item := func() string {
		if rng.Intn(6) == 0 {
			return str()
		}
		role := str()
		if rng.Intn(2) == 0 {
			role = []string{`"user"`, `"assistant"`, `"system"`, `"tool"`, `"Developer"`}[rng.Intn(5)]
		}
		return maybe(obj("role", role, "content", content(), "text", str(), "type", str()))
	}
	gemini := func() string { return maybe(obj("role", str(), "parts", list(part))) }
	switch rng.Intn(3) {
	case 0:
		frame := []string{"instructions", content(), "input", maybe(list(item)), "response", obj("input", list(item))}
		if rng.Intn(4) == 0 {
			frame = append(frame, "type", []string{`"response.create"`, str()}[rng.Intn(2)])
		}
		return obj(frame...)
	case 1:
		return obj("system", content(), "messages", maybe(list(item)), "input", str())
	}
	return obj("systemInstruction", maybe(gemini()), "system_instruction", list(gemini), "contents", list(gemini),
		"content", gemini(), "instances", list(func() string { return obj("prompt", str()) }),
		"requests", list(func() string { return obj("contents", list(gemini), "systemInstruction", str()) }))
}
