package service

import (
	"net/http"
	"sort"
	"strings"
	"unsafe"

	"github.com/Wei-Shaw/sub2api/internal/pkg/openai_compat"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// Verified-raw list (review rr6, fail-safe charge). The body-spool ingest
// charges a request its route's base factor only when every part of the
// request is on this list: k_skel (8) on a covered Responses route, the
// measured k_path 17 on a Responses-protocol fallback route (compact, or a
// Content-Encoding body). Anything else takes the k_conv top-up up front,
// before any decode, or gets the named S_conv / L_conv 413 or the 503
// (hop sub2api, limit_bytes); no failover, no retry. Every other fallback
// route (chat, messages, count_tokens, Gemini, Antigravity, Grok) is not on
// the list: the ingest charges it k_conv from the first byte.
//
// The request half (route, headers, root members, input items, tools) is
// checked by the ingest on the skeleton (VerifiedRawResponsesRequest); the
// account half (IsVerifiedRawAccount) by the gate before each forward
// attempt (ChargeRequestUnlessVerifiedRaw). Each entry was measured as a
// passthrough at k <= 6 by the charge-cover property test
// (handler/body_spool_charge_cover_*_test.go, TestBodySpoolChargeCovers
// Allocation) on the API-key force_responses account at 2, 8 and 16 MiB:
// the generated "gen_*" shapes populate every root member, item type,
// item field and tool type below. Report rows: report.md, "rr6 fail-safe".
// A shape moves onto the list only after a raw-JSON rewrite and a parity
// test, never by assumption.

// rawRootMembers: root members of a Responses body, each with the check its
// value must pass ("" = verified). Measured: gen_root_<name>, k <= 2.3.
//
// Identifier members (rawIdent*, review rr10): an identifier, mode or
// metadata value is read by many gjson Get sites, each of which unescapes
// it, so a body-sized one costs k ~13 (adv_leaf_root_prompt_cache_key_*,
// adv_leaf_root_reasoning_*_reasoning_effort, adv_leaf_item_*_id_id). Such
// a value is capped at rawIdentMaxBytes raw bytes; above it the request is
// off the list (k_conv). Content-bearing members (input, instructions,
// text, arguments, output, encrypted_content, summary text, image and file
// data, tool descriptions and parameters) are never capped.
var rawRootMembers = map[string]func(string) string{
	"model":                  rawIdentString,
	"input":                  rawInput,
	"instructions":           rawString,
	"stream":                 rawBool,
	"store":                  rawBool,
	"background":             rawBool,
	"parallel_tool_calls":    rawBool, // raw rewrite (normalizeOpenAIParallelToolCallsWithoutTools)
	"tools":                  rawTools,
	"tool_choice":            rawAny,
	"reasoning":              rawReasoning,
	"include":                rawArray,
	"text":                   rawObject, // json_schema: skeleton rewrite, no input decode (gen_root_text_json_schema)
	"metadata":               rawMetadata,
	"stream_options":         rawObject,
	"client_metadata":        rawObject,
	"prompt_cache_key":       rawIdentString,
	"prompt_cache_retention": rawAny,      // deleted for non-Codex clients by one sjson patch
	"prompt_cache_options":   rawAny,      // same
	"safety_identifier":      rawIdentAny, // same
	"previous_response_id":   rawIdentString,
	"service_tier":           rawIdentAny,
	"user":                   rawIdentAny,
	"truncation":             rawIdentAny,
	"max_output_tokens":      rawNumber,
	"temperature":            rawNumber,
	"top_p":                  rawNumber,
	"top_logprobs":           rawNumber,
	// Client-shape corpus (rr6 follow-up, TestBodySpoolClientShapesRawPath,
	// 16 MiB, API-key force_responses, forwarded unchanged):
	"max_tool_calls":     rawNumber,       // client_sdk_max_tool_calls k 3.01
	"conversation":       rawConversation, // client_sdk_conversation k 3.22
	"context_management": rawArray,        // client_sdk_context_management k 3.22
}

// rawItemTypes: input item types (an item's last "type" member; an item
// with a role and no type is a message), each with its field check.
// Measured: gen_item_<type>, k <= 2.3. Production pi evidence (local
// session scan, 854,886 reasoning items): every reasoning item has
// content [] plus summary and encrypted_content.
var rawItemTypes = map[string]func(string) string{
	"message":                 rawMessageItem,
	"function_call":           rawPlainItem,
	"function_call_output":    rawPlainItem,
	"custom_tool_call":        rawPlainItem,
	"custom_tool_call_output": rawPlainItem,
	"reasoning":               rawReasoningItem,
	// Client-shape corpus (rr6 follow-up, TestBodySpoolClientShapesRawPath,
	// 16 MiB, API-key force_responses, forwarded unchanged; the input-ID
	// sanitizer's raw id delete applies to web_search_call):
	"item_reference":          rawPlainItem, // client_sdk_item_reference k 3.22
	"compaction":              rawPlainItem, // client_sdk_compaction k 3.22
	"web_search_call":         rawPlainItem, // client_sdk_tool_web_search k 3.22
	"file_search_call":        rawPlainItem, // client_sdk_tool_file_search k 3.22
	"code_interpreter_call":   rawPlainItem, // client_sdk_tool_code_interpreter k 3.22
	"mcp_list_tools":          rawPlainItem, // client_sdk_tool_mcp k 3.22
	"mcp_approval_request":    rawPlainItem, // same
	"mcp_approval_response":   rawPlainItem, // same
	"mcp_call":                rawPlainItem, // same
	"computer_call":           rawPlainItem, // client_sdk_tool_computer_use_preview k 3.22
	"computer_call_output":    rawPlainItem, // same
	"shell_call":              rawPlainItem, // client_sdk_tool_shell k 3.23
	"shell_call_output":       rawPlainItem, // same
	"apply_patch_call":        rawPlainItem, // client_sdk_tool_apply_patch k 3.01
	"apply_patch_call_output": rawPlainItem, // same
	// pi tool search (pi-ai convertResponsesMessages; tool_search_call ids
	// get the sanitizer's raw delete):
	"additional_tools":     rawPlainItem, // client_pi_additional_tools k 2.23
	"tool_search_call":     rawPlainItem, // client_pi_tool_search_items k 2.23
	"tool_search_output":   rawPlainItem, // same
	"configuration_update": rawPlainItem, // client_pi_configuration_update k 2.23
}

// rawMessageRoles and rawContentPartTypes: a message's role and the types
// of its content parts. Measured: gen_item_message_<role>, gen_part_<type>.
var rawMessageRoles = map[string]bool{"user": true, "assistant": true, "system": true, "developer": true}

var rawContentPartTypes = map[string]bool{"input_text": true, "output_text": true, "input_image": true, "refusal": true,
	"input_file": true, // client_sdk_input_file k 3.22 (rr6 follow-up)
}

// rawToolTypes: tool declarations. Measured: gen_tool_<type>, k <= 2.3.
// custom: no rewrite on a non-passthrough API-key account (the passthrough
// normalizer that decodes it is behind the account half).
var rawToolTypes = map[string]bool{"function": true, "custom": true,
	// Client-shape corpus (rr6 follow-up, TestBodySpoolClientShapesRawPath,
	// 16 MiB, API-key force_responses, forwarded unchanged), each with its
	// call items:
	"web_search":           true, // client_sdk_tool_web_search k 3.22
	"web_search_preview":   true, // client_sdk_tool_web_search_preview k 3.22
	"file_search":          true, // client_sdk_tool_file_search k 3.22
	"code_interpreter":     true, // client_sdk_tool_code_interpreter k 3.22
	"mcp":                  true, // client_sdk_tool_mcp k 3.22
	"computer_use_preview": true, // client_sdk_tool_computer_use_preview k 3.22
	"shell":                true, // client_sdk_tool_shell k 3.23
	"apply_patch":          true, // client_sdk_tool_apply_patch k 3.01
}

// rawDecisiveHeaders: request headers the body path reads that change how
// the body is decoded or rewritten, each with its verified values. Headers
// not listed are not read by the body path. Content-Encoding is the
// ingest's: such a body is never covered, it is read as a Responses
// fallback body at k_path and checked here once decoded.
var rawDecisiveHeaders = map[string]func(string) bool{
	// Responses-Lite on an API-key account: raw parallel_tool_calls rewrite
	// (TestResponsesLiteParallelToolCallsRawMatchesDecoded), k 2.22
	// (gen_header_lite).
	responsesLiteHeader: func(string) bool { return true },
}

// VerifiedRawResponsesRequest reports whether a Responses request body
// (the ingest's skeleton or the decoded fallback body) and its headers are
// on the verified-raw list; reason names the first part that is not. It
// walks the raw JSON once and allocates nothing in proportion to the body
// (no value is unescaped; a member name or a type with an escape is not on
// the list).
func VerifiedRawResponsesRequest(body []byte, header http.Header) (ok bool, reason string) {
	for name, verified := range rawDecisiveHeaders {
		if v := header.Get(name); v != "" && !verified(v) {
			return false, "header " + name
		}
	}
	if !gjson.ValidBytes(body) {
		return false, "body is not valid JSON"
	}
	raw := strings.TrimSpace(unsafe.String(unsafe.SliceData(body), len(body)))
	if len(raw) == 0 || raw[0] != '{' {
		return false, "body is not a JSON object"
	}
	var seen [32]string
	n := 0
	forEachRawMember(raw, func(key, value string) bool {
		check, listed := rawRootMembers[key]
		switch {
		case !listed:
			reason = "root member " + rawReasonName(key)
		case n == len(seen):
			reason = "too many root members"
		default:
			for _, k := range seen[:n] {
				if k == key {
					reason = "duplicate root member " + rawReasonName(key)
				}
			}
			seen[n] = key
			n++
			if reason == "" {
				if r := check(value); r != "" {
					reason = "root member " + key + ": " + r
				}
			}
		}
		return reason == ""
	})
	return reason == "", reason
}

// rawValueEnd: the index just past the JSON value at raw[i] (raw is valid
// JSON, i at the value's first byte).
func rawValueEnd(raw string, i int) int {
	switch raw[i] {
	case '"':
		for i++; i < len(raw); i++ {
			switch raw[i] {
			case '\\':
				i++
			case '"':
				return i + 1
			}
		}
		return len(raw)
	case '{', '[':
		depth, inString := 0, false
		for ; i < len(raw); i++ {
			c := raw[i]
			if inString {
				if c == '\\' {
					i++
				} else if c == '"' {
					inString = false
				}
				continue
			}
			switch c {
			case '"':
				inString = true
			case '{', '[':
				depth++
			case '}', ']':
				depth--
				if depth == 0 {
					return i + 1
				}
			}
		}
		return len(raw)
	}
	for i < len(raw) && !strings.ContainsRune(",}] \t\r\n", rune(raw[i])) {
		i++
	}
	return i
}

func rawSkipSpace(raw string, i int) int {
	for i < len(raw) && (raw[i] == ' ' || raw[i] == '\t' || raw[i] == '\r' || raw[i] == '\n') {
		i++
	}
	return i
}

// forEachRawMember calls fn with each member of the raw JSON object obj:
// the name without quotes ("\\" when it has an escape, never on a list) and
// the raw value. It stops when fn returns false.
func forEachRawMember(obj string, fn func(key, value string) bool) {
	i := rawSkipSpace(obj, 1)
	for i < len(obj) && obj[i] == '"' {
		end := rawValueEnd(obj, i)
		key := obj[i+1 : end-1]
		if strings.IndexByte(key, '\\') >= 0 {
			key = `\`
		}
		i = rawSkipSpace(obj, end)
		i = rawSkipSpace(obj, i+1) // ':'
		end = rawValueEnd(obj, i)
		if !fn(key, obj[i:end]) {
			return
		}
		i = rawSkipSpace(obj, end)
		if i < len(obj) && obj[i] == ',' {
			i = rawSkipSpace(obj, i+1)
		}
	}
}

// forEachRawElement calls fn with each raw element of the JSON array arr.
func forEachRawElement(arr string, fn func(value string) bool) {
	i := rawSkipSpace(arr, 1)
	for i < len(arr) && arr[i] != ']' {
		end := rawValueEnd(arr, i)
		if !fn(arr[i:end]) {
			return
		}
		i = rawSkipSpace(arr, end)
		if i < len(arr) && arr[i] == ',' {
			i = rawSkipSpace(arr, i+1)
		}
	}
}

// rawMember: the raw value of obj's last member named key ("" if none).
func rawMember(obj, key string) (value string) {
	forEachRawMember(obj, func(k, v string) bool {
		if k == key {
			value = v
		}
		return true
	})
	return value
}

// rawStr: the content of a raw JSON string without escapes; ok=false for
// anything else.
func rawStr(v string) (string, bool) {
	if len(v) < 2 || v[0] != '"' || strings.IndexByte(v, '\\') >= 0 {
		return "", false
	}
	return v[1 : len(v)-1], true
}

func rawAny(string) string { return "" }

// rawIdentMaxBytes: the cap on an identifier member's raw bytes (a
// string's bytes between the quotes, as sent; any other value's whole raw
// text).
const rawIdentMaxBytes = 4 << 10

func rawIdentLen(v string) int {
	if len(v) >= 2 && v[0] == '"' {
		return len(v) - 2
	}
	return len(v)
}

// rawIdent: "" when v is within rawIdentMaxBytes.
func rawIdent(v string) string {
	if rawIdentLen(v) > rawIdentMaxBytes {
		return "longer than 4096 bytes"
	}
	return ""
}

func rawIdentAny(v string) string { return rawIdent(v) }

func rawIdentString(v string) string {
	if r := rawString(v); r != "" {
		return r
	}
	return rawIdent(v)
}

// rawConversation: a conversation id, or an object of ids ({"id":...}),
// each within the cap.
func rawConversation(v string) (reason string) {
	if rawFirst(v) != '{' {
		return rawIdent(v)
	}
	forEachRawMember(v, func(k, m string) bool {
		if r := rawIdent(m); r != "" {
			reason = rawReasonName(k) + " " + r
		}
		return reason == ""
	})
	return reason
}

// rawReasoning: the reasoning object; its effort and summary modes within
// the cap.
func rawReasoning(v string) (reason string) {
	if r := rawObject(v); r != "" || v == "null" {
		return r
	}
	forEachRawMember(v, func(k, m string) bool {
		if k == "effort" || k == "summary" {
			if r := rawIdent(m); r != "" {
				reason = k + " " + r
			}
		}
		return reason == ""
	})
	return reason
}

// rawMetadata: the metadata object; every key and value within the cap.
func rawMetadata(v string) (reason string) {
	if r := rawObject(v); r != "" || v == "null" {
		return r
	}
	i := rawSkipSpace(v, 1)
	for i < len(v) && v[i] == '"' {
		end := rawValueEnd(v, i)
		if end-i-2 > rawIdentMaxBytes {
			return "key longer than 4096 bytes"
		}
		i = rawSkipSpace(v, end)
		i = rawSkipSpace(v, i+1) // ':'
		end = rawValueEnd(v, i)
		if r := rawIdent(v[i:end]); r != "" {
			return "value " + r
		}
		i = rawSkipSpace(v, end)
		if i < len(v) && v[i] == ',' {
			i = rawSkipSpace(v, i+1)
		}
	}
	return ""
}

// rawItemIdentMembers: item members capped at rawIdentMaxBytes.
var rawItemIdentMembers = map[string]bool{"id": true, "call_id": true, "status": true, "name": true, "namespace": true}

func rawFirst(v string) byte {
	if v == "" {
		return 0
	}
	return v[0]
}

func rawString(v string) string {
	if c := rawFirst(v); c == '"' || v == "null" {
		return ""
	}
	return "not a string"
}

func rawBool(v string) string {
	if v == "true" || v == "false" || v == "null" {
		return ""
	}
	return "not a boolean"
}

func rawNumber(v string) string {
	if c := rawFirst(v); c == '-' || (c >= '0' && c <= '9') || v == "null" {
		return ""
	}
	return "not a number"
}

func rawObject(v string) string {
	if rawFirst(v) == '{' || v == "null" {
		return ""
	}
	return "not an object"
}

func rawArray(v string) string {
	if rawFirst(v) == '[' || v == "null" {
		return ""
	}
	return "not an array"
}

// rawInput: a string, or an array of listed items.
func rawInput(v string) (reason string) {
	switch rawFirst(v) {
	case '"':
		return ""
	case '[':
	default:
		return "not a string or an array"
	}
	forEachRawElement(v, func(item string) bool {
		reason = rawItem(item)
		return reason == ""
	})
	return reason
}

func rawItem(item string) string {
	if rawFirst(item) != '{' {
		return "item is not an object"
	}
	var typ string
	var namespace string
	hasType, hasRole, hasNamespace, nameEscaped := false, false, false, false
	longIdent := ""
	// Member names, for duplicates: an item with a duplicate name is
	// decoded alone by the store=false replay and the input-ID sanitizer.
	var seen [32]string
	n, dup := 0, ""
	forEachRawMember(item, func(k, v string) bool {
		for _, s := range seen[:n] {
			if s == k && dup == "" {
				dup = k
			}
		}
		if n < len(seen) {
			seen[n] = k
		}
		n++
		switch k {
		case "type":
			typ, hasType = v, true
		case "role":
			hasRole = true
		case "namespace":
			namespace, hasNamespace = v, true
		case `\`:
			nameEscaped = true
		}
		if longIdent == "" && rawItemIdentMembers[k] && rawIdent(v) != "" {
			longIdent = k
		}
		return true
	})
	switch {
	case nameEscaped:
		return "item member name with an escape"
	case longIdent != "":
		return "item " + longIdent + " longer than 4096 bytes"
	case n > len(seen):
		return "item with more than 32 members"
	case dup != "":
		return "item duplicate member " + rawReasonName(dup)
	}
	if hasNamespace {
		if reason := rawItemNamespace(namespace); reason != "" {
			return reason
		}
	}
	name, ok := rawStr(typ)
	if !hasType {
		name, ok = "message", hasRole
	}
	check, listed := rawItemTypes[name]
	switch {
	case !hasType && !hasRole:
		return "item without type or role"
	case !ok:
		return "item type not a plain string"
	case !listed:
		return "item type " + rawReasonName(name)
	}
	return check(item)
}

// rawItemNamespace: a plain string other than codex_app and codex_tui. The
// handler's Codex bootstrap scan decodes the whole body only for those two
// values (gjson input.#(namespace==...), which unescapes, so an escaped
// value is off too). The API-key forward strips namespace from input items
// with one raw gjson/sjson edit (stripOpenAIResponsesInputNamespaces).
func rawItemNamespace(v string) string {
	ns, ok := rawStr(v)
	switch {
	case !ok:
		return "item namespace not a plain string"
	case ns == "codex_app" || ns == "codex_tui":
		return "item namespace " + ns
	}
	return ""
}

func rawPlainItem(string) string { return "" }

// rawReasoningItem: content absent, null or []. A non-empty content is
// replayed through a whole-body decode (Forward, reasoning content).
func rawReasoningItem(item string) string {
	content := rawMember(item, "content")
	if content == "" || content == "null" {
		return ""
	}
	empty := rawFirst(content) == '['
	forEachRawElement(content, func(string) bool {
		empty = false
		return false
	})
	if empty {
		return ""
	}
	return "reasoning content"
}

func rawMessageItem(item string) (reason string) {
	role, ok := rawStr(rawMember(item, "role"))
	if !ok || !rawMessageRoles[role] {
		return "message role"
	}
	content := rawMember(item, "content")
	switch rawFirst(content) {
	case 0, '"':
		return ""
	case '[':
	default:
		if content == "null" {
			return ""
		}
		return "message content"
	}
	forEachRawElement(content, func(part string) bool {
		reason = rawContentPart(part)
		return reason == ""
	})
	return reason
}

func rawContentPart(part string) string {
	if rawFirst(part) != '{' {
		return "content part is not an object"
	}
	typ, ok := rawStr(rawMember(part, "type"))
	if !ok {
		return "content part type not a plain string"
	}
	if !rawContentPartTypes[typ] {
		return "content part type " + rawReasonName(typ)
	}
	if typ == "input_image" {
		// An empty base64 image is sanitized by a whole-body decode. Such
		// a value is short; a long one (also a spooled one, whose marker
		// has escapes) carries data.
		url := rawMember(part, "image_url")
		if rawFirst(url) == '"' && len(url) <= 512 {
			s := gjson.Parse(url).Str
			if strings.HasPrefix(s, "data:") && strings.HasSuffix(s, ";base64,") {
				return "empty base64 input_image"
			}
		}
	}
	return ""
}

func rawTools(v string) (reason string) {
	if v == "null" {
		return ""
	}
	if rawFirst(v) != '[' {
		return "not an array"
	}
	forEachRawElement(v, func(tool string) bool {
		typ, ok := "", false
		if rawFirst(tool) == '{' {
			typ, ok = rawStr(rawMember(tool, "type"))
		}
		switch {
		case !ok:
			reason = "tool type not a plain string"
		case !rawToolTypes[typ]:
			reason = "tool type " + rawReasonName(typ)
		}
		return reason == ""
	})
	return reason
}

// rawAccountFlag is one decisive account read of the body path: verified
// reports whether a has its verified value.
type rawAccountFlag struct {
	verified func(a *Account) bool
	why      string
}

func rawAnyValue(*Account) bool { return true }

// rawDecisiveAccountReads: every account field, method or Extra key a
// body-path file (request_raw_list_scan_test.go, rawBodyPathFiles) reads
// that changes how the body is decoded, converted, rewritten or routed.
// IsVerifiedRawAccount requires each at its verified value. The measured
// account: platform openai, type apikey, extra openai_responses_mode
// force_responses, openai_passthrough false, Sub2API WS mode off (the
// charge-cover property test's apikey-force_responses account, plus the
// gen_account_* variants for the "any value" entries).
var rawDecisiveAccountReads = map[string]rawAccountFlag{
	"Platform":                   {func(a *Account) bool { return a.Platform == PlatformOpenAI }, "openai"},
	"Type":                       {func(a *Account) bool { return a.Type == AccountTypeAPIKey }, "apikey"},
	"IsOpenAI":                   {func(a *Account) bool { return a.IsOpenAI() }, "true (platform openai)"},
	"IsOpenAIApiKey":             {func(a *Account) bool { return a.IsOpenAIApiKey() }, "true"},
	"IsOpenAIOAuthLike":          {func(a *Account) bool { return !a.IsOpenAIOAuthLike() }, "false: the Codex OAuth transform decodes"},
	"UsesOpenAICodexProtocol":    {func(a *Account) bool { return !a.UsesOpenAICodexProtocol() }, "false"},
	"IsOAuth":                    {func(a *Account) bool { return !a.IsOAuth() }, "false"},
	"IsGrok":                     {func(a *Account) bool { return !a.IsGrok() }, "false"},
	"IsBedrock":                  {func(a *Account) bool { return !a.IsBedrock() }, "false"},
	"IsOpenCodeGo":               {func(a *Account) bool { return !a.IsOpenCodeGo() }, "false"},
	"IsOpenCodeGoPlan":           {func(a *Account) bool { return !a.IsOpenCodeGoPlan() }, "false"},
	"IsOpenAIAgentIdentity":      {func(a *Account) bool { return !a.IsOpenAIAgentIdentity() }, "false (OAuth only)"},
	"IsOpenAIPassthroughEnabled": {func(a *Account) bool { return !a.IsOpenAIPassthroughEnabled() }, "false: extra openai_passthrough / openai_oauth_passthrough; the passthrough normalizers decode (rr6)"},
	"routesByModel":              {func(a *Account) bool { return !a.routesByModel() }, "false: model protocol_rules may convert"},
	"RoutesProtocolByInbound":    {func(a *Account) bool { return !a.RoutesProtocolByInbound() }, "false: CN providers may convert"},
	"GetAPIProtocol":             {rawAnyValue, "any: read only under RoutesProtocolByInbound or SupportsNativeCNResponses"},
	"IsAdaptiveAPIProtocol":      {func(a *Account) bool { return !a.IsAdaptiveAPIProtocol() }, "false"},
	"SupportsNativeCNResponses":  {func(a *Account) bool { return !a.SupportsNativeCNResponses() }, "false"},
	"UsesNativeCNResponses":      {func(a *Account) bool { return !a.UsesNativeCNResponses() }, "false: DeepSeek client-tool adaptation decodes"},
	"Extra->ShouldUseResponsesAPI": {func(a *Account) bool {
		return openai_compat.NormalizeResponsesSupportMode(rawExtraString(a, openai_compat.ExtraKeyResponsesMode)) == openai_compat.ResponsesSupportModeForceResponses
	}, "extra openai_responses_mode force_responses: never the Responses->Chat conversion"},
	"IsOpenAIResponsesFlattenNamespacesEnabled": {func(a *Account) bool { return !a.IsOpenAIResponsesFlattenNamespacesEnabled() }, "false: the flatten decodes"},
	"CodexImageGenerationExplicitToolPolicy":    {rawAnyValue, "any: acts only on image_generation tools, which are not on the tool list"},
	"GetCodexFingerprintMode":                   {rawAnyValue, "any: read only on the Codex OAuth and passthrough paths"},
	"Extra->codexFingerprintSeed":               {rawAnyValue, "any: Codex OAuth and passthrough paths only"},
	"GetChatGPTAccountID":                       {rawAnyValue, "any: Codex OAuth identity only"},
	"GetCredential(chatgpt_user_id)":            {rawAnyValue, "any: Codex OAuth identity only"},
	"GetMappedModel":                            {rawAnyValue, "any: one raw sjson set of model (gen_account_model_mapping)"},
	"ResolveCompactMappedModel":                 {rawAnyValue, "any: compact model, one raw set (gen_account_model_mapping)"},
	"GetCredential(base_url)":                   {rawAnyValue, "any: decides one raw sjson delete of reasoning.effort none (gen_account_base_url)"},
	"Extra[OllamaCloudMaxTokensCapExtraKey]":    {rawAnyValue, "any: one raw sjson set of max_output_tokens, Ollama Cloud base URL only"},
	"Extra":                                     {rawAnyValue, "any: nil check before the Ollama Cloud cap"},
}

// rawInertAccountReads: body-path account reads that never change how the
// body is decoded, converted, rewritten or routed.
var rawInertAccountReads = map[string]string{
	"ID":                               "logs, keys, usage",
	"Name":                             "logs",
	"Proxy":                            "transport",
	"ProxyID":                          "transport",
	"ParentAccountID":                  "shadow usage bookkeeping",
	"IsShadow":                         "shadow usage bookkeeping and agent identity (OAuth)",
	"GetOpenAIBaseURL":                 "upstream URL",
	"GetCNProtocolBaseURL":             "upstream URL",
	"GetOpenAIUserAgent":               "header",
	"ApplyHeaderOverrides":             "headers",
	"GetOpenAIAccessToken":             "auth header",
	"GetOpenAIDeviceID":                "header",
	"GetCredential(agent_private_key)": "agent identity signing (OAuth)",
	"GetCredential(agent_runtime_id)":  "agent identity header (OAuth)",
	"GetCredential(task_id)":           "agent identity task check (OAuth)",
	"Credentials":                      "agent identity credential persistence (OAuth)",
	"IsPoolMode":                       "retry policy: resends the same bytes",
	"IsPoolModeRetryableStatus":        "retry policy",
	"IsCodexCLIOnlyEnabled":            "client restriction: a 403, no body change",
	"IsCodexCLIOnlyAppServerAllowed":   "client restriction",
}

func rawExtraString(a *Account, key string) string {
	if a == nil || a.Extra == nil {
		return ""
	}
	v, _ := a.Extra[key].(string)
	return v
}

// IsVerifiedRawAccount reports whether a request to account over
// transport stays raw on the body path: every decisive read at its
// verified value and the Sub2API upstream transport HTTP (WS mode off: the
// WS v2 payload is decoded). reason names the first read that is not.
func IsVerifiedRawAccount(account *Account, transport OpenAIUpstreamTransport) (ok bool, reason string) {
	if account == nil {
		return false, "no account"
	}
	if transport != OpenAIUpstreamTransportHTTPSSE {
		return false, "upstream transport " + string(transport)
	}
	names := make([]string, 0, len(rawDecisiveAccountReads))
	for name := range rawDecisiveAccountReads {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if !rawDecisiveAccountReads[name].verified(account) {
			return false, "account " + name
		}
	}
	return true, ""
}

// ChargeRequestUnlessVerifiedRaw is the account half of the fail-safe
// charge, called before each forward attempt and before the attempt's body
// is derived: unless the account is verified raw for this request, it takes
// the k_conv top-up (once per request; a refusal is written and final),
// recorded as an off-list decision (ChargeRequestOffList).
func (s *OpenAIGatewayService) ChargeRequestUnlessVerifiedRaw(c *gin.Context, account *Account) error {
	decision := s.getOpenAIWSProtocolResolver().Resolve(account)
	decision = resolveOpenAIWSDecisionByClientTransport(decision, GetOpenAIClientTransport(c))
	ok, reason := IsVerifiedRawAccount(account, decision.Transport)
	if ok {
		return nil
	}
	return ChargeRequestOffList(c, "account", reason)
}
