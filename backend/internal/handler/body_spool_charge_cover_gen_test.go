//go:build unit

package handler

import (
	"sort"
	"strings"
)

// Schema-driven corpus for the charge-covers-allocation test (rr6 item 4),
// on with SUB2API_CHARGE_COVER_GEN=1. Every Responses input item type, each
// root member, each tool type; the combos give every route (/v1/responses,
// /responses, /backend-api/codex/responses, /v1/responses/compact) × every
// account (API-key force_responses, force_chat_completions, passthrough,
// OAuth) and the test runs each at 2, 8 and 16 MiB. Shapes are named
// gen_item_<type>[_<field>], gen_tool_<type>, gen_root_<member>.
//
// Fields: an item type on the verified-raw list (service/request_raw_list.go)
// gets one shape per optional field, so each field is measured on its own
// at k_skel. A type not on the list takes the k_conv top-up whatever its
// fields, so it gets its minimal form and one form with every optional field.

// coverGenItem: an item type, its required members and its optional ones.
type coverGenItem struct {
	typ      string
	base     string            // required members, without type
	optional map[string]string // member -> value
	listed   bool              // on the verified-raw list
}

var coverGenItems = []coverGenItem{
	{typ: "message", base: `"role":"user","content":"x"`, listed: true, optional: map[string]string{
		"id": `"msg_1"`, "status": `"completed"`, "phase": `"final_answer"`,
	}},
	{typ: "function_call", base: `"call_id":"gc1","name":"f","arguments":"{}"`, listed: true, optional: map[string]string{
		"id": `"fc_1"`, "status": `"completed"`, "namespace": `"ns"`,
	}},
	{typ: "function_call_output", base: `"call_id":"gc1","output":"ok"`, listed: true, optional: map[string]string{
		"id": `"fco_1"`, "status": `"completed"`, "output": `[{"type":"input_text","text":"ok"}]`,
	}},
	{typ: "custom_tool_call", base: `"call_id":"gc2","name":"apply_patch","input":"x"`, listed: true, optional: map[string]string{
		"id": `"ctc_1"`, "status": `"completed"`, "namespace": `"ns"`,
	}},
	{typ: "custom_tool_call_output", base: `"call_id":"gc2","output":"ok"`, listed: true, optional: map[string]string{
		"id": `"ctco_1"`, "output": `[{"type":"input_text","text":"ok"}]`,
	}},
	{typ: "reasoning", base: `"summary":[]`, listed: true, optional: map[string]string{
		"id": `"rs_1"`, "summary": `[{"type":"summary_text","text":"s"}]`, "encrypted_content": `"gAAAAenc"`,
		"content": `[{"type":"reasoning_text","text":"t"}]`, "content_empty": `[]`, "status": `"completed"`,
	}},
	{typ: "compaction_trigger"},
	{typ: "compaction", listed: true, base: `"encrypted_content":"gAAAAenc"`, optional: map[string]string{"id": `"cmp_1"`}},
	{typ: "item_reference", listed: true, base: `"id":"msg_0"`},
	{typ: "web_search_call", listed: true, base: `"id":"ws_1","status":"completed"`, optional: map[string]string{"action": `{"type":"search","query":"q"}`}},
	{typ: "file_search_call", listed: true, base: `"id":"fs_1","status":"completed","queries":["q"]`, optional: map[string]string{"results": `[{"file_id":"f","text":"t"}]`}},
	{typ: "computer_call", listed: true, base: `"id":"cc_1","call_id":"gc3","action":{"type":"click","x":1,"y":2,"button":"left"},"pending_safety_checks":[],"status":"completed"`},
	{typ: "computer_call_output", listed: true, base: `"call_id":"gc3","output":{"type":"computer_screenshot","image_url":"data:image/png;base64,QUJD"}`, optional: map[string]string{"acknowledged_safety_checks": `[]`, "id": `"cco_1"`}},
	{typ: "local_shell_call", base: `"id":"ls_1","call_id":"gc4","action":{"type":"exec","command":["ls"],"env":{}},"status":"completed"`},
	{typ: "local_shell_call_output", base: `"id":"lso_1","output":"ok"`, optional: map[string]string{"status": `"completed"`}},
	{typ: "shell_call", listed: true, base: `"call_id":"gc5","action":{"commands":["ls"]},"status":"completed"`, optional: map[string]string{"id": `"sh_1"`}},
	{typ: "shell_call_output", listed: true, base: `"call_id":"gc5","output":[{"stdout":"ok","stderr":"","outcome":{"type":"exit","exit_code":0}}]`},
	{typ: "apply_patch_call", listed: true, base: `"call_id":"gc6","operation":{"type":"update_file","path":"a","diff":"d"},"status":"completed"`},
	{typ: "apply_patch_call_output", listed: true, base: `"call_id":"gc6","status":"completed"`, optional: map[string]string{"output": `"ok"`}},
	{typ: "mcp_list_tools", listed: true, base: `"id":"ml_1","server_label":"s","tools":[]`},
	{typ: "mcp_approval_request", listed: true, base: `"id":"ma_1","server_label":"s","name":"t","arguments":"{}"`},
	{typ: "mcp_approval_response", listed: true, base: `"approval_request_id":"ma_1","approve":true`},
	{typ: "mcp_call", listed: true, base: `"id":"mc_1","server_label":"s","name":"t","arguments":"{}"`, optional: map[string]string{"output": `"ok"`, "error": `null`}},
	{typ: "image_generation_call", base: `"id":"ig_1","status":"completed"`, optional: map[string]string{"result": `"QUJD"`}},
	{typ: "code_interpreter_call", listed: true, base: `"id":"ci_1","status":"completed","container_id":"c","code":"1"`, optional: map[string]string{"outputs": `[]`}},
	{typ: "tool_search_call", base: `"call_id":"gc7","execution":"client","status":"completed","arguments":{"query":"q","limit":1}`},
	{typ: "tool_search_output", base: `"call_id":"gc7","execution":"client","status":"completed","tools":[{"type":"function","name":"f","parameters":{"type":"object","properties":{}}}]`},
	{typ: "additional_tools", base: `"role":"developer","tools":[{"type":"function","name":"f","parameters":{"type":"object","properties":{}}}]`},
	{typ: "unknown_item_rr6"},
}

// coverGenMessageContent: message content forms (part types and their
// decode-triggering fields), each a gen_item_message_<name> shape.
var coverGenMessageContent = map[string]string{
	"role_assistant":     `"role":"assistant","content":"x"`,
	"role_system":        `"role":"system","content":"x"`,
	"role_developer":     `"role":"developer","content":"x"`,
	"part_input_text":    `"role":"user","content":[{"type":"input_text","text":"x"}]`,
	"part_output_text":   `"role":"assistant","content":[{"type":"output_text","text":"x","annotations":[],"logprobs":[]}]`,
	"part_refusal":       `"role":"assistant","content":[{"type":"refusal","refusal":"no"}]`,
	"part_input_image":   `"role":"user","content":[{"type":"input_image","image_url":"data:image/png;base64,QUJD","detail":"auto"}]`,
	"part_image_file_id": `"role":"user","content":[{"type":"input_image","file_id":"file_1","detail":"low"}]`,
	"part_image_empty":   `"role":"user","content":[{"type":"input_image","image_url":"data:image/png;base64,"}]`,
	"part_input_file":    `"role":"user","content":[{"type":"input_file","file_id":"file_1","filename":"a.txt"}]`,
	"part_input_audio":   `"role":"user","content":[{"type":"input_audio","input_audio":{"data":"QUJD","format":"wav"}}]`,
}

// coverGenTools: tool declarations, each a gen_tool_<type> shape.
var coverGenTools = map[string]string{
	"function":           `{"type":"function","name":"f","description":"d","parameters":{"type":"object","properties":{"a":{"type":"string"}}},"strict":false}`,
	"function_deferred":  `{"type":"function","name":"f","parameters":{"type":"object","properties":{}},"defer_loading":true}`,
	"custom":             `{"type":"custom","name":"apply_patch","description":"d","format":{"type":"grammar","syntax":"lark","definition":"start: /.+/"}}`,
	"tool_search":        `{"type":"tool_search"}`,
	"namespace":          `{"type":"namespace","name":"ns","description":"d","tools":[{"type":"function","name":"f","parameters":{"type":"object","properties":{}}}]}`,
	"web_search":         `{"type":"web_search"}`,
	"web_search_preview": `{"type":"web_search_preview","search_context_size":"low"}`,
	"file_search":        `{"type":"file_search","vector_store_ids":["vs_1"]}`,
	"image_generation":   `{"type":"image_generation","output_format":"png"}`,
	"code_interpreter":   `{"type":"code_interpreter","container":{"type":"auto"}}`,
	"mcp":                `{"type":"mcp","server_label":"s","server_url":"https://example.invalid/mcp","require_approval":"never"}`,
	"local_shell":        `{"type":"local_shell"}`,
	"shell":              `{"type":"shell"}`,
	"apply_patch":        `{"type":"apply_patch"}`,
	"computer_use":       `{"type":"computer_use_preview","display_width":1024,"display_height":768,"environment":"linux"}`,
}

// coverGenRoot: root members other than model, input and stream (the
// base body has them), each a gen_root_<member> shape. "unlisted_*" are
// members not on the verified-raw list.
var coverGenRoot = map[string]string{
	"instructions":           `"instructions":"be brief",`,
	"store_true":             `"store":true,`,
	"background":             `"background":false,`,
	"parallel_tool_calls":    `"parallel_tool_calls":false,`,
	"tool_choice":            `"tool_choice":"auto",`,
	"reasoning":              `"reasoning":{"effort":"high","summary":"auto"},`,
	"reasoning_none":         `"reasoning":{"effort":"none"},`,
	"include":                `"include":["reasoning.encrypted_content"],`,
	"text_verbosity":         `"text":{"verbosity":"low"},`,
	"metadata":               `"metadata":{"k":"v"},`,
	"stream_options":         `"stream_options":{"include_obfuscation":false},`,
	"client_metadata":        `"client_metadata":{"x-codex-turn-metadata":"{}"},`,
	"prompt_cache_key":       `"prompt_cache_key":"pck-1",`,
	"prompt_cache_retention": `"prompt_cache_retention":"24h",`,
	"prompt_cache_options":   `"prompt_cache_options":{"mode":"auto"},`,
	"safety_identifier":      `"safety_identifier":"s",`,
	"previous_response_id":   `"previous_response_id":"resp_1",`,
	"service_tier":           `"service_tier":"priority",`,
	"user":                   `"user":"u",`,
	"truncation":             `"truncation":"auto",`,
	"max_output_tokens":      `"max_output_tokens":1024,`,
	"temperature":            `"temperature":0.7,`,
	"top_p":                  `"top_p":0.9,`,
	"top_logprobs":           `"top_logprobs":2,`,
	"unlisted_max_tokens":    `"max_tokens":1024,`,
	"conversation":           `"conversation":"conv_1",`,
	"unlisted_prompt":        `"prompt":{"id":"pmpt_1"},`,
}

// coverGenMods renders the shapes, sorted by name.
func coverGenMods() []coverMod {
	var out []coverMod
	item := func(name, members string) {
		out = append(out, coverMod{proto: "responses", name: name, item: "{" + members + "}"})
	}
	join := func(parts ...string) string {
		var keep []string
		for _, p := range parts {
			if p != "" {
				keep = append(keep, p)
			}
		}
		return strings.Join(keep, ",")
	}
	for _, it := range coverGenItems {
		typ := `"type":"` + it.typ + `"`
		item("gen_item_"+it.typ, join(typ, it.base))
		if len(it.optional) == 0 {
			continue
		}
		fields := make([]string, 0, len(it.optional))
		for f := range it.optional {
			fields = append(fields, f)
		}
		sort.Strings(fields)
		if !it.listed {
			var all []string
			for _, f := range fields {
				all = append(all, `"`+f+`":`+it.optional[f])
			}
			item("gen_item_"+it.typ+"_all", join(typ, it.base, strings.Join(all, ",")))
			continue
		}
		for _, f := range fields {
			member := f
			if f == "content_empty" {
				member = "content"
			}
			// A field that is also a base member replaces it (last wins in
			// the decoders; the skeleton reads the last member too).
			item("gen_item_"+it.typ+"_"+f, join(typ, it.base, `"`+member+`":`+it.optional[f]))
		}
	}
	for name, members := range coverGenMessageContent {
		item("gen_item_message_"+name, join(`"type":"message"`, members))
	}
	for name, tool := range coverGenTools {
		out = append(out, coverMod{proto: "responses", name: "gen_tool_" + name, root: `"tools":[` + tool + `],`})
	}
	for name, root := range coverGenRoot {
		out = append(out, coverMod{proto: "responses", name: "gen_root_" + name, root: root})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

// coverGenCombo: the generated shapes run on the four Responses routes ×
// the four account kinds (coverOpenAIAccounts). The guard, failover,
// Gemini/Antigravity/Anthropic Responses combos run the hand-written corpus
// only: their body path is the same as one of these (guard: the snapshot
// scan is shape-independent; failover: the passthrough then the
// force_responses account) or converts every request.
func coverGenCombo(cb coverCombo) bool {
	switch cb.route {
	case "v1/responses", "responses", "codex/responses", "v1/responses/compact":
	default:
		return false
	}
	for _, a := range coverOpenAIAccounts {
		if cb.account == a.name {
			return true
		}
	}
	return false
}
