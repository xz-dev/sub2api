package service

import (
	"bytes"
	"fmt"
	"reflect"
	"strings"

	"github.com/tidwall/gjson"
)

type openAIResponsesLiteValidationError struct {
	param   string
	message string
}

func (e *openAIResponsesLiteValidationError) Error() string { return e.message }

func newOpenAIResponsesLiteValidationError(param, format string, args ...any) error {
	return &openAIResponsesLiteValidationError{param: param, message: fmt.Sprintf(format, args...)}
}

// normalizeOpenAIResponsesLiteTools applies the Responses Lite request
// contract: reasoning must cover all turns, and private namespace declarations
// use the input.additional_tools carrier. Other top-level tools must belong to
// the small set accepted by the Lite endpoint; rejecting unsupported hosted
// tools is intentional because silently dropping them would change behavior.
func normalizeOpenAIResponsesLiteTools(reqBody map[string]any) (bool, error) {
	if reqBody == nil {
		return false, nil
	}
	if parallel, exists := reqBody["parallel_tool_calls"]; exists {
		if _, ok := parallel.(bool); !ok {
			return false, newOpenAIResponsesLiteValidationError("parallel_tool_calls", "responses Lite requires parallel_tool_calls to be a boolean")
		}
	}
	if rawReasoning, exists := reqBody["reasoning"]; exists && rawReasoning != nil {
		if _, ok := rawReasoning.(map[string]any); !ok {
			return false, newOpenAIResponsesLiteValidationError("reasoning", "responses Lite requires reasoning to be an object")
		}
	}
	rawTools, exists := reqBody["tools"]
	if !exists || rawTools == nil {
		changed, err := ensureOpenAIResponsesLiteReasoningContext(reqBody)
		if err != nil {
			return false, err
		}
		return ensureOpenAIResponsesLiteParallelToolCalls(reqBody, changed)
	}
	tools, ok := rawTools.([]any)
	if !ok {
		return false, newOpenAIResponsesLiteValidationError("tools", "responses Lite requires tools to be an array")
	}

	topLevelTools := make([]any, 0, len(tools))
	namespaceTools := make([]any, 0, len(tools))
	for index, rawTool := range tools {
		if customTool, ok := rawTool.(string); ok {
			if strings.TrimSpace(customTool) == "" {
				return false, fmt.Errorf("responses Lite custom tool at index %d must not be empty", index)
			}
			topLevelTools = append(topLevelTools, rawTool)
			continue
		}
		tool, ok := rawTool.(map[string]any)
		if !ok {
			return false, fmt.Errorf("responses Lite tool at index %d must be an object", index)
		}
		toolType := strings.TrimSpace(firstNonEmptyString(tool["type"]))
		switch toolType {
		case "function", "custom", "tool_search":
			topLevelTools = append(topLevelTools, rawTool)
		case "namespace":
			namespaceTools = append(namespaceTools, rawTool)
		case "":
			return false, fmt.Errorf("responses Lite tool at index %d is missing type", index)
		default:
			return false, fmt.Errorf("responses Lite does not support top-level tool type %q at index %d", toolType, index)
		}
	}
	if len(namespaceTools) == 0 {
		changed, err := ensureOpenAIResponsesLiteReasoningContext(reqBody)
		if err != nil {
			return false, err
		}
		return ensureOpenAIResponsesLiteParallelToolCalls(reqBody, changed)
	}

	input, err := appendOpenAIResponsesLiteAdditionalTools(reqBody["input"], namespaceTools)
	if err != nil {
		return false, err
	}
	if _, err := ensureOpenAIResponsesLiteReasoningContext(reqBody); err != nil {
		return false, err
	}
	reqBody["input"] = input
	if len(topLevelTools) == 0 {
		delete(reqBody, "tools")
	} else {
		reqBody["tools"] = topLevelTools
	}
	return ensureOpenAIResponsesLiteParallelToolCalls(reqBody, true)
}

func ensureOpenAIResponsesLiteParallelToolCalls(reqBody map[string]any, changed bool) (bool, error) {
	parallel, exists := reqBody["parallel_tool_calls"]
	if exists {
		if _, ok := parallel.(bool); !ok {
			return false, newOpenAIResponsesLiteValidationError("parallel_tool_calls", "responses Lite requires parallel_tool_calls to be a boolean")
		}
	}
	if parallel == false {
		return changed, nil
	}
	reqBody["parallel_tool_calls"] = false
	return true, nil
}

func ensureOpenAIResponsesLiteReasoningContext(reqBody map[string]any) (bool, error) {
	rawReasoning, exists := reqBody["reasoning"]
	if !exists || rawReasoning == nil {
		reqBody["reasoning"] = map[string]any{"context": "all_turns"}
		return true, nil
	}
	reasoning, ok := rawReasoning.(map[string]any)
	if !ok {
		return false, newOpenAIResponsesLiteValidationError("reasoning", "responses Lite requires reasoning to be an object")
	}
	if context, ok := reasoning["context"].(string); ok && context == "all_turns" {
		return false, nil
	}
	reasoning["context"] = "all_turns"
	return true, nil
}

func appendOpenAIResponsesLiteAdditionalTools(input any, namespaceTools []any) ([]any, error) {
	var items []any
	switch typed := input.(type) {
	case nil:
		items = make([]any, 0, 1)
	case string:
		items = []any{map[string]any{
			"type":    "message",
			"role":    "user",
			"content": typed,
		}}
	case []any:
		items = typed
	default:
		return nil, fmt.Errorf("responses Lite namespace tools require input to be a string or array")
	}

	var target map[string]any
	var targetTools []any
	var allAdditionalTools []any
	for _, rawItem := range items {
		item, ok := rawItem.(map[string]any)
		if !ok || strings.TrimSpace(firstNonEmptyString(item["type"])) != "additional_tools" {
			continue
		}
		rawAdditionalTools, exists := item["tools"]
		additionalTools := []any(nil)
		toolsOK := true
		if exists && rawAdditionalTools != nil {
			additionalTools, toolsOK = rawAdditionalTools.([]any)
		}
		if !toolsOK {
			return nil, fmt.Errorf("responses Lite input.additional_tools tools must be an array")
		}
		if target == nil {
			target = item
			targetTools = additionalTools
		}
		allAdditionalTools = append(allAdditionalTools, additionalTools...)
	}

	merged, err := mergeOpenAIResponsesLiteAdditionalTools(allAdditionalTools, namespaceTools)
	if err != nil {
		return nil, err
	}
	newTools := merged[len(allAdditionalTools):]
	if target != nil {
		if len(newTools) > 0 {
			target["tools"] = append(append([]any(nil), targetTools...), newTools...)
		}
		return items, nil
	}

	items = append(items, map[string]any{
		"type":  "additional_tools",
		"role":  "developer",
		"tools": newTools,
	})
	return items, nil
}

func mergeOpenAIResponsesLiteAdditionalTools(existing []any, moved []any) ([]any, error) {
	merged := append([]any(nil), existing...)
	seen := make(map[string]any, len(existing)+len(moved))
	for _, rawTool := range existing {
		if identity := openAIResponsesLiteToolIdentity(rawTool); identity != "" {
			if previous, exists := seen[identity]; exists && !reflect.DeepEqual(previous, rawTool) {
				return nil, fmt.Errorf("responses Lite additional_tools contains conflicting definitions for %s", openAIResponsesLiteToolIdentityForError(rawTool))
			}
			seen[identity] = rawTool
		}
	}
	for _, rawTool := range moved {
		identity := openAIResponsesLiteToolIdentity(rawTool)
		if identity != "" {
			if previous, exists := seen[identity]; exists {
				if reflect.DeepEqual(previous, rawTool) {
					continue
				}
				return nil, fmt.Errorf("responses Lite additional_tools conflicts with migrated %s", openAIResponsesLiteToolIdentityForError(rawTool))
			}
			seen[identity] = rawTool
		}
		merged = append(merged, rawTool)
	}
	return merged, nil
}

func openAIResponsesLiteToolIdentity(rawTool any) string {
	tool, ok := rawTool.(map[string]any)
	if !ok {
		return ""
	}
	toolType := strings.TrimSpace(firstNonEmptyString(tool["type"]))
	name := strings.TrimSpace(firstNonEmptyString(tool["name"]))
	if toolType == "" || name == "" {
		return ""
	}
	return toolType + "\x00" + name
}

func openAIResponsesLiteToolIdentityForError(rawTool any) string {
	tool, _ := rawTool.(map[string]any)
	return fmt.Sprintf("tool type %q name %q", strings.TrimSpace(firstNonEmptyString(tool["type"])), strings.TrimSpace(firstNonEmptyString(tool["name"])))
}

// normalizeOpenAIResponsesLiteToolsPayload decodes all of the body (the
// namespace tools move into input), so it takes charge, the k_conv top-up,
// first (DecodeFullRequestBody).
func normalizeOpenAIResponsesLiteToolsPayload(body []byte, charge func() error) ([]byte, bool, error) {
	var requestBody map[string]any
	if err := DecodeFullRequestBody(charge, body, &requestBody); err != nil {
		if IsRequestConversionRefused(err) {
			return body, false, err
		}
		return body, false, fmt.Errorf("decode responses Lite request body: %w", err)
	}
	changed, err := normalizeOpenAIResponsesLiteTools(requestBody)
	if err != nil || !changed {
		return body, false, err
	}
	rebuilt, err := marshalOpenAIUpstreamJSON(requestBody)
	if err != nil {
		return body, false, fmt.Errorf("encode responses Lite request body: %w", err)
	}
	return rebuilt, true, nil
}

// normalizeOpenAIResponsesLiteParallelToolCallsPayload sets the root
// parallel_tool_calls to false on raw JSON, as decoding and re-encoding
// would (the decoder keeps the last duplicate): an absent or true value
// becomes false, false stays, any other value is a validation error. Only
// a body that is not a JSON object or has a duplicate root name takes the
// decoding form, with its top-up (review rr5: the header alone decoded every
// API-key body, about 15 x its size).
func normalizeOpenAIResponsesLiteParallelToolCallsPayload(body []byte, charge func() error) ([]byte, bool, error) {
	root := parseRawJSONView(body)
	var scratch []string
	if !gjson.ValidBytes(body) || !root.IsObject() || hasDuplicateJSONObjectKeysIn(root, &scratch) {
		return normalizeOpenAIResponsesLiteParallelToolCallsDecoded(body, charge)
	}
	parallel := lastRawObjectMember(root, "parallel_tool_calls")
	switch {
	case parallel.Type == gjson.False:
		return body, false, nil
	case parallel.Type == gjson.True:
		return rewriteRawObjectMember(body, root, parallel, false, 5, func(out []byte) []byte {
			return append(out, "false"...)
		}), true, nil
	case parallel.Exists():
		return body, false, newOpenAIResponsesLiteValidationError("parallel_tool_calls", "responses Lite requires parallel_tool_calls to be a boolean")
	}
	const member = `"parallel_tool_calls":false`
	open := bytes.IndexByte(body, '{')
	rest := bytes.TrimLeft(body[open+1:], " \t\r\n")
	out := make([]byte, 0, len(body)+len(member)+1)
	out = append(out, body[:open+1]...)
	out = append(out, member...)
	if rest[0] != '}' {
		out = append(out, ',')
	}
	return append(out, body[open+1:]...), true, nil
}

// normalizeOpenAIResponsesLiteParallelToolCallsDecoded is the decoding form,
// for a body the raw form does not take.
func normalizeOpenAIResponsesLiteParallelToolCallsDecoded(body []byte, charge func() error) ([]byte, bool, error) {
	var requestBody map[string]any
	if err := DecodeFullRequestBody(charge, body, &requestBody); err != nil {
		if IsRequestConversionRefused(err) {
			return body, false, err
		}
		return body, false, fmt.Errorf("decode responses Lite request body: %w", err)
	}
	if requestBody == nil { // the body is null
		return body, false, nil
	}
	changed, err := ensureOpenAIResponsesLiteParallelToolCalls(requestBody, false)
	if err != nil || !changed {
		return body, false, err
	}
	rebuilt, err := marshalOpenAIUpstreamJSON(requestBody)
	if err != nil {
		return body, false, fmt.Errorf("encode responses Lite request body: %w", err)
	}
	return rebuilt, true, nil
}

// normalizeOpenAIResponsesLitePayloadForAccount: charge is the k_conv
// top-up for a form that decodes all of the body (RequestConversionCharger,
// or NoConversionCharge for a WebSocket message).
func normalizeOpenAIResponsesLitePayloadForAccount(body []byte, account *Account, charge func() error) ([]byte, bool, error) {
	if account == nil || !account.IsOpenAI() {
		return body, false, nil
	}
	if account.IsOpenAIOAuthLike() {
		return normalizeOpenAIResponsesLiteToolsPayload(body, charge)
	}
	return normalizeOpenAIResponsesLiteParallelToolCallsPayload(body, charge)
}
