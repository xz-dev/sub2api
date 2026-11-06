//go:build unit

package handler

import "strings"

// Client-shape corpus (rr6 follow-up item 6): whole Responses request
// bodies as real clients build them, padded to size with the tiny
// conversation. Each runs on the property test's Responses combos at
// 16 MiB, and TestBodySpoolClientShapesRawPath measures each on the
// API-key force_responses account.
//
// pi: openai-api-pi-extension on pi-ai's openai-responses provider
// (packages/ai/src/api/openai-responses.ts buildParams,
// openai-responses-shared.ts convertResponsesMessages/convertResponsesTools;
// the extension's payload hook sets store:true for checkpoints and the
// bridge chains previous_response_id). SDK: openai-python
// types/responses (response_create_params.py ResponseCreateParamsBase,
// response_input_item_param.py ResponseInputItemParam, tool_param.py
// ToolParam).
type coverClient struct {
	name  string
	root  string // root members before "input", each with a trailing ','
	items string // input items after the padding, comma-joined
	// big: structure-heavy root members and items of about n bytes in
	// all, instead of padding (stress shapes for the per-item and
	// content-seed decodes on the raw path).
	big func(n int) (root, items string)
}

var coverClientProto = coverProto{
	head: func(string) string { return `"input":[` },
	user: coverProtos["responses"].user,
	asst: coverProtos["responses"].asst,
	tail: coverProtos["responses"].tail,
}

func (cl coverClient) body(n int) []byte {
	root, items := cl.root, cl.items
	if cl.big != nil {
		root, items = cl.big(n - 64<<10)
	}
	return coverBody(coverClientProto, coverBases[0], n, "", root, items)
}

// pi building blocks (pi-ai buildParams and convertResponsesMessages).
const (
	piRoot = `"model":"gpt-5.5","stream":true,"prompt_cache_key":"pi-session-0123456789abcdef",` +
		`"prompt_cache_retention":"24h","store":false,"max_output_tokens":32000,"service_tier":"priority",` +
		`"tool_choice":"auto","reasoning":{"effort":"high","summary":"auto"},"include":["reasoning.encrypted_content"],` +
		`"top_p":0.95,`
	piTools = `"tools":[{"type":"function","name":"read","description":"Read a file",` +
		`"parameters":{"type":"object","properties":{"path":{"type":"string"}},"required":["path"],"additionalProperties":false},"strict":false},` +
		`{"type":"custom","name":"apply_patch","description":"Apply a patch","format":{"type":"grammar","syntax":"lark","definition":"start: /.+/"}}],`
	piItems = `{"role":"developer","content":"You are pi."},` +
		`{"role":"user","content":[{"type":"input_text","text":"look"},{"type":"input_image","detail":"auto","image_url":"data:image/png;base64,iVBORw0KGgo="}]},` +
		`{"type":"reasoning","id":"rs_0123","summary":[{"type":"summary_text","text":"thinking"}],"encrypted_content":"gAAAAABpi","content":[]},` +
		`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok","annotations":[]}],"status":"completed","id":"msg_0123","phase":"commentary"},` +
		`{"type":"function_call","id":"fc_0123","call_id":"call_1","name":"read","arguments":"{\"path\":\"a\"}"},` +
		`{"type":"function_call_output","call_id":"call_1","output":"file text"},` +
		`{"type":"function_call","id":"fc_0124","call_id":"call_2","name":"read","arguments":"{\"path\":\"b.png\"}"},` +
		`{"type":"function_call_output","call_id":"call_2","output":[{"type":"input_text","text":"image"},{"type":"input_image","detail":"auto","image_url":"data:image/png;base64,iVBORw0KGgo="}]},` +
		`{"type":"custom_tool_call","id":"ctc_0123","call_id":"call_3","name":"apply_patch","input":"*** Begin Patch"},` +
		`{"type":"custom_tool_call_output","call_id":"call_3","output":"done"}`
	piToolSearchItems = `{"type":"additional_tools","role":"developer","tools":[{"type":"function","name":"grep","description":"d","parameters":{"type":"object","properties":{}},"strict":false}]},` +
		`{"type":"tool_search_call","call_id":"pi_tool_load_1","execution":"client","status":"completed","arguments":{"query":"grep","limit":1}},` +
		`{"type":"tool_search_output","call_id":"pi_tool_load_1","execution":"client","status":"completed","tools":[{"type":"function","name":"grep","description":"d","parameters":{"type":"object","properties":{}},"strict":false,"defer_loading":true}]},` +
		`{"type":"configuration_update","reasoning":{"effort":"low"}}`
)

// sdkRoot: every ResponseCreateParamsBase member on the verified-raw list
// (parallel_tool_calls goes with tools: without tools the gateway drops it).
const sdkRoot = `"model":"gpt-5.5","stream":true,"store":true,"instructions":"be brief","background":false,` +
	`"include":["reasoning.encrypted_content","web_search_call.action.sources","message.output_text.logprobs"],` +
	`"metadata":{"app":"py"},"previous_response_id":"resp_1","truncation":"auto","user":"u-1","safety_identifier":"sid-1",` +
	`"prompt_cache_key":"pck-1","prompt_cache_retention":"in_memory","top_logprobs":2,"top_p":0.9,"temperature":0.7,` +
	`"max_output_tokens":4096,"service_tier":"auto","stream_options":{"include_obfuscation":false},` +
	`"text":{"format":{"type":"text"},"verbosity":"medium"},"reasoning":{"effort":"medium","summary":"auto"},`

// sdkTool: one built-in tool with the call items the SDK sends back for it.
func sdkTool(tool, items string) (string, string) {
	return sdkRoot + `"parallel_tool_calls":true,"tools":[` + tool + `],`, items
}

func coverClients() []coverClient {
	var out []coverClient
	add := func(name, root, items string) { out = append(out, coverClient{name: name, root: root, items: items}) }
	add("client_pi", piRoot+piTools, piItems)
	add("client_pi_checkpoint", strings.Replace(piRoot, `"store":false`, `"store":true,"previous_response_id":"resp_1"`, 1)+piTools, piItems)
	add("client_pi_tool_search", piRoot+strings.Replace(piTools, `"strict":false}`, `"strict":false,"defer_loading":true}`, 1),
		piToolSearchItems+`,`+strings.Replace(piItems, `"name":"read","arguments"`, `"name":"read","namespace":"pi_tools","arguments"`, 1))
	// The pi tool-search pieces one by one (pi-ai convertResponsesMessages:
	// supportsAdditionalTools / supportsToolSearch / configuration updates).
	add("client_pi_additional_tools", piRoot+piTools, `{"type":"additional_tools","role":"developer","tools":[{"type":"function","name":"grep","description":"d","parameters":{"type":"object","properties":{}},"strict":false}]}`)
	add("client_pi_tool_search_items", piRoot+strings.Replace(piTools, `"strict":false}`, `"strict":false,"defer_loading":true}`, 1),
		`{"type":"tool_search_call","call_id":"pi_tool_load_1","execution":"client","status":"completed","arguments":{"query":"grep","limit":1}},`+
			`{"type":"tool_search_output","call_id":"pi_tool_load_1","execution":"client","status":"completed","tools":[{"type":"function","name":"grep","description":"d","parameters":{"type":"object","properties":{}},"strict":false,"defer_loading":true}]}`)
	add("client_pi_configuration_update", piRoot+piTools, `{"type":"configuration_update","reasoning":{"effort":"low"}}`)
	add("client_pi_namespace_call", piRoot+piTools, `{"type":"function_call","id":"fc_0125","call_id":"call_9","name":"read","namespace":"pi_tools","arguments":"{}"},{"type":"function_call_output","call_id":"call_9","output":"ok"}`)
	add("client_sdk_base", sdkRoot+`"parallel_tool_calls":true,"tools":[{"type":"function","name":"f","parameters":{"type":"object","properties":{}},"strict":true}],`,
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"q"}]},{"type":"function_call","call_id":"c1","name":"f","arguments":"{}"},{"type":"function_call_output","call_id":"c1","output":"o"}`)
	add("client_sdk_max_tool_calls", sdkRoot+`"max_tool_calls":5,`, `{"role":"user","content":"q"}`)
	add("client_sdk_prompt", sdkRoot+`"prompt":{"id":"pmpt_0123","version":"2","variables":{"city":"Paris"}},`, `{"role":"user","content":"q"}`)
	add("client_sdk_conversation", strings.Replace(sdkRoot, `"previous_response_id":"resp_1",`, `"conversation":"conv_0123",`, 1), `{"role":"user","content":"q"}`)
	add("client_sdk_context_management", sdkRoot+`"context_management":[{"type":"compaction","compact_threshold":200000}],`, `{"role":"user","content":"q"}`)
	add("client_sdk_input_file", sdkRoot, `{"role":"user","content":[{"type":"input_file","file_id":"file-0123"},{"type":"input_file","filename":"a.pdf","file_data":"data:application/pdf;base64,JVBERi0="}]}`)
	add("client_sdk_item_reference", sdkRoot, `{"type":"item_reference","id":"msg_0123"}`)
	add("client_sdk_compaction", sdkRoot, `{"type":"compaction","id":"cmp_0123","encrypted_content":"gAAAAABcmp"}`)
	for _, t := range coverClientTools {
		root, items := sdkTool(t.tool, t.items)
		add("client_sdk_tool_"+t.name, root, items)
	}
	return out
}

// coverClientTools: the SDK's built-in tools (ToolParam) with their call
// items (ResponseInputItemParam).
var coverClientTools = []struct{ name, tool, items string }{
	{"web_search", `{"type":"web_search","search_context_size":"medium","user_location":{"type":"approximate","country":"US"},"filters":{"allowed_domains":["example.com"]}}`,
		`{"type":"web_search_call","id":"ws_0123","status":"completed","action":{"type":"search","query":"q","sources":[{"type":"url","url":"https://example.com"}]}}`},
	{"web_search_preview", `{"type":"web_search_preview","search_context_size":"low"}`,
		`{"type":"web_search_call","id":"ws_0124","status":"completed","action":{"type":"open_page","url":"https://example.com"}}`},
	{"file_search", `{"type":"file_search","vector_store_ids":["vs_0123"],"max_num_results":5,"ranking_options":{"ranker":"auto","score_threshold":0.1}}`,
		`{"type":"file_search_call","id":"fs_0123","status":"completed","queries":["q"],"results":[{"file_id":"file-1","filename":"a.txt","score":0.9,"text":"t","attributes":{}}]}`},
	{"code_interpreter", `{"type":"code_interpreter","container":{"type":"auto","file_ids":[]}}`,
		`{"type":"code_interpreter_call","id":"ci_0123","status":"completed","container_id":"cntr_1","code":"print(1)","outputs":[{"type":"logs","logs":"1"}]}`},
	{"image_generation", `{"type":"image_generation","output_format":"png","quality":"auto","size":"auto","background":"auto"}`,
		`{"type":"image_generation_call","id":"ig_0123","status":"completed","result":"iVBORw0KGgo="}`},
	{"mcp", `{"type":"mcp","server_label":"deepwiki","server_url":"https://mcp.example.invalid/mcp","require_approval":"never","allowed_tools":["ask"]}`,
		`{"type":"mcp_list_tools","id":"mcpl_0123","server_label":"deepwiki","tools":[{"name":"ask","input_schema":{"type":"object"}}]},` +
			`{"type":"mcp_approval_request","id":"mcpr_0123","server_label":"deepwiki","name":"ask","arguments":"{}"},` +
			`{"type":"mcp_approval_response","approval_request_id":"mcpr_0123","approve":true},` +
			`{"type":"mcp_call","id":"mcp_0123","server_label":"deepwiki","name":"ask","arguments":"{}","output":"ok","status":"completed"}`},
	{"computer_use_preview", `{"type":"computer_use_preview","display_width":1024,"display_height":768,"environment":"linux"}`,
		`{"type":"computer_call","id":"cu_0123","call_id":"call_cu","action":{"type":"click","button":"left","x":1,"y":2},"pending_safety_checks":[],"status":"completed"},` +
			`{"type":"computer_call_output","call_id":"call_cu","output":{"type":"computer_screenshot","image_url":"data:image/png;base64,iVBORw0KGgo="},"acknowledged_safety_checks":[]}`},
	{"local_shell", `{"type":"local_shell"}`,
		`{"type":"local_shell_call","id":"lsh_0123","call_id":"call_ls","action":{"type":"exec","command":["ls"],"env":{}},"status":"completed"},` +
			`{"type":"local_shell_call_output","id":"call_ls","output":"a.txt"}`},
	{"shell", `{"type":"shell"}`,
		`{"type":"shell_call","call_id":"call_sh","action":{"commands":["ls"]},"status":"completed"},` +
			`{"type":"shell_call_output","call_id":"call_sh","output":[{"stdout":"a.txt","stderr":"","outcome":{"type":"exit","exit_code":0}}]}`},
	{"apply_patch", `{"type":"apply_patch"}`,
		`{"type":"apply_patch_call","call_id":"call_ap","operation":{"type":"update_file","path":"a.txt","diff":"@@"},"status":"completed"},` +
			`{"type":"apply_patch_call_output","call_id":"call_ap","status":"completed","output":"ok"}`},
}

// coverRepeat: pre + elem repeated, comma-separated, to about n bytes + post.
func coverRepeat(n int, pre, elem, post string) string {
	var b strings.Builder
	b.Grow(n + len(pre) + len(post))
	b.WriteString(pre)
	for i := 0; b.Len() < n; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(elem)
	}
	b.WriteString(post)
	return b.String()
}

// coverStressClients: one structure-heavy part on the raw path. "noseed":
// no prompt_cache_key and no session header, so the content session seed
// (deriveOpenAIContentSessionSeed) reads tools and system/developer items.
func coverStressClients() []coverClient {
	noseed := strings.Replace(piRoot, `"prompt_cache_key":"pi-session-0123456789abcdef",`, "", 1)
	return []coverClient{
		{name: "stress_reasoning_rs_summary", big: func(n int) (string, string) {
			return piRoot, coverRepeat(n, `{"type":"reasoning","id":"rs_s","summary":[`, `{"type":"summary_text","text":"a"}`, `],"encrypted_content":"gAAAA","content":[]}`)
		}},
		{name: "stress_message_call_id", big: func(n int) (string, string) {
			return piRoot, coverRepeat(n, `{"type":"message","role":"assistant","call_id":"c","content":[`, `{"type":"output_text","text":"a"}`, `]}`)
		}},
		{name: "stress_developer_parts_noseed", big: func(n int) (string, string) {
			return noseed, coverRepeat(n, `{"role":"developer","content":[`, `{"type":"input_text","text":"a"}`, `]}`)
		}},
		{name: "stress_tools_noseed", big: func(n int) (string, string) {
			return noseed + coverRepeat(n, `"tools":[`, `{"type":"function","name":"f","parameters":{"type":"object"}}`, `],`), `{"role":"user","content":"q"}`
		}},
		{name: "stress_item_id_sanitize", big: func(n int) (string, string) {
			return piRoot, coverRepeat(n, ``, `{"type":"message","role":"user","id":"bad","content":"a"}`, ``)
		}},
		{name: "stress_fco_parts", big: func(n int) (string, string) {
			return piRoot, coverRepeat(n, `{"type":"function_call","call_id":"c","name":"read","arguments":"{}"},{"type":"function_call_output","call_id":"c","output":[`, `{"type":"input_text","text":"a"}`, `]}`)
		}},
	}
}
