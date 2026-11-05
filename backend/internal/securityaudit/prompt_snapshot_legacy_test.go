package securityaudit

// The prompt guard's extraction before follow-up 6, unchanged: json into any,
// then the protocol extractors over maps. It is kept as the reference for
// the parity tests of the in-place extraction (prompt_json.go,
// prompt_segments.go) and is not used by production code.

import (
	"sort"
	"strings"
)

type promptSegment struct {
	text string
	user bool
	role string
}

func extractProtocolSegments(protocol string, document any) []promptSegment {
	root, _ := document.(map[string]any)
	protocol = strings.ToLower(strings.TrimSpace(protocol))
	switch protocol {
	case "openai_chat_completions", "openai_chat", "chat_completions":
		return extractChatLikeSegments(root)
	case "anthropic_messages", "claude_messages", "messages":
		return append(extractAnthropicSystem(root["system"]), extractMessages(root["messages"], clientInstructionRoles...)...)
	case "gemini", "gemini_generate_content":
		return extractGeminiRoot(root)
	case "openai_responses", "responses", "responses_websocket":
		if frameType := stringValue(root["type"]); frameType != "" || protocol == "responses_websocket" {
			if frameType != "response.create" {
				return nil
			}
			if input, exists := root["input"]; exists && input != nil {
				return append(extractInstructions(root["instructions"]), extractResponses(input)...)
			}
			if response, ok := root["response"].(map[string]any); ok {
				return append(extractInstructions(response["instructions"]), extractResponses(response["input"])...)
			}
			return extractInstructions(root["instructions"])
		}
		return append(extractInstructions(root["instructions"]), extractResponses(root["input"])...)
	case "openai_images", "grok_media", "media", "images":
		return userPromptSegments(extractMediaPrompts(root))
	case "typesafe_systemone":
		return extractSystemOneSegments(root)
	default:
		if segments := extractChatLikeSegments(root); len(segments) > 0 {
			return segments
		}
		if responses := append(extractInstructions(root["instructions"]), extractResponses(root["input"])...); len(responses) > 0 {
			return responses
		}
		if gemini := extractGeminiRoot(root); len(gemini) > 0 {
			return gemini
		}
		return userPromptSegments(extractMediaPrompts(root))
	}
}

// extractSystemOneSegments collects every client-controlled text of a TypeSafe
// System One request: question IDs, every question field except the validated
// type, unknown top-level extension fields, and the evaluated state. Object keys
// are text too (Jev reads the whole JSON), so they are collected with values.
// The state comes last so it is the prioritized segment, and keys are visited
// in sorted order to keep the prompt hash stable across requests.
func extractSystemOneSegments(root map[string]any) []promptSegment {
	if root == nil {
		return nil
	}
	texts := make([]string, 0, 4)
	questions, isObject := root["questions"].(map[string]any)
	if !isObject {
		texts = appendJSONStringLeaves(texts, root["questions"])
	}
	for _, id := range sortedJSONKeys(questions) {
		texts = appendJSONStringLeaves(texts, id)
		question, ok := questions[id].(map[string]any)
		if !ok {
			texts = appendJSONStringLeaves(texts, questions[id])
			continue
		}
		for _, field := range sortedJSONKeys(question) {
			switch field {
			case "type":
				continue
			case "instructions", "criteria":
			default:
				texts = appendJSONStringLeaves(texts, field)
			}
			texts = appendJSONStringLeaves(texts, question[field])
		}
	}
	for _, field := range sortedJSONKeys(root) {
		switch field {
		case "model", "stream", "state", "questions":
			continue
		}
		texts = appendJSONStringLeaves(texts, field)
		texts = appendJSONStringLeaves(texts, root[field])
	}
	texts = appendJSONStringLeaves(texts, root["state"])
	return userPromptSegments(texts)
}

func appendJSONStringLeaves(texts []string, value any) []string {
	switch typed := value.(type) {
	case string:
		if text := strings.TrimSpace(typed); text != "" {
			texts = append(texts, text)
		}
	case []any:
		for _, item := range typed {
			texts = appendJSONStringLeaves(texts, item)
		}
	case map[string]any:
		for _, key := range sortedJSONKeys(typed) {
			texts = appendJSONStringLeaves(texts, key)
			texts = appendJSONStringLeaves(texts, typed[key])
		}
	}
	return texts
}

func sortedJSONKeys(values map[string]any) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// clientInstructionRoles are roles a client may freely populate. Attackers can
// place jailbreak/PII text in assistant/tool turns, so blocking audit must scan
// them too—not only user/system/developer instructions.
var clientInstructionRoles = []string{"user", "system", "developer", "assistant", "tool"}

func extractChatLikeSegments(root map[string]any) []promptSegment {
	if root == nil {
		return nil
	}
	return extractMessages(root["messages"], clientInstructionRoles...)
}

func extractMessages(value any, wantedRoles ...string) []promptSegment {
	items, ok := value.([]any)
	if !ok {
		return nil
	}
	wanted := make(map[string]struct{}, len(wantedRoles))
	for _, role := range wantedRoles {
		wanted[strings.ToLower(strings.TrimSpace(role))] = struct{}{}
	}
	result := make([]promptSegment, 0, len(items))
	for _, item := range items {
		message, ok := item.(map[string]any)
		if !ok {
			continue
		}
		role := strings.ToLower(stringValue(message["role"]))
		if _, match := wanted[role]; !match {
			continue
		}
		texts := contentTexts(message["content"])
		for _, text := range texts {
			result = append(result, promptSegment{text: text, user: role == "user", role: role})
		}
	}
	return result
}

func extractInstructions(value any) []promptSegment {
	switch typed := value.(type) {
	case string:
		if text := strings.TrimSpace(typed); text != "" {
			return []promptSegment{{text: text, role: "system"}}
		}
	case []any:
		return systemPromptSegments(contentTexts(typed))
	case map[string]any:
		return systemPromptSegments(contentTexts(typed))
	}
	return nil
}

func extractAnthropicSystem(value any) []promptSegment {
	switch typed := value.(type) {
	case string:
		if text := strings.TrimSpace(typed); text != "" {
			return []promptSegment{{text: text, role: "system"}}
		}
	case []any:
		return systemPromptSegments(contentTexts(typed))
	case map[string]any:
		return systemPromptSegments(contentTexts(typed))
	}
	return nil
}

func extractResponses(value any) []promptSegment {
	switch typed := value.(type) {
	case string:
		return []promptSegment{{text: typed, user: true, role: "user"}}
	case []any:
		result := make([]promptSegment, 0, len(typed))
		for _, item := range typed {
			switch entry := item.(type) {
			case string:
				result = append(result, promptSegment{text: entry, user: true, role: "user"})
			case map[string]any:
				role := strings.ToLower(stringValue(entry["role"]))
				if role != "" && !isClientInstructionRole(role) {
					continue
				}
				if content, exists := entry["content"]; exists {
					for _, text := range contentTexts(content) {
						result = append(result, promptSegment{text: text, user: role == "" || role == "user", role: role})
					}
				} else if text := stringValue(entry["text"]); text != "" {
					result = append(result, promptSegment{text: text, user: role == "" || role == "user", role: role})
				}
			}
		}
		return result
	case map[string]any:
		role := strings.ToLower(stringValue(typed["role"]))
		if role != "" && !isClientInstructionRole(role) {
			return nil
		}
		return promptSegmentsForRole(contentTexts(typed["content"]), role)
	default:
		return nil
	}
}

func extractGemini(value any) []promptSegment {
	var contents []any
	switch typed := value.(type) {
	case []any:
		contents = typed
	case map[string]any:
		contents = []any{typed}
	default:
		return nil
	}
	result := make([]promptSegment, 0, len(contents))
	for _, item := range contents {
		content, ok := item.(map[string]any)
		if !ok {
			continue
		}
		role := strings.ToLower(stringValue(content["role"]))
		if role != "" && !isClientInstructionRole(role) {
			continue
		}
		parts, _ := content["parts"].([]any)
		for _, part := range parts {
			if object, ok := part.(map[string]any); ok {
				if text := stringValue(object["text"]); text != "" {
					result = append(result, promptSegment{text: text, user: role == "" || role == "user", role: role})
				}
			}
		}
	}
	return result
}

func extractGeminiRoot(root map[string]any) []promptSegment {
	if root == nil {
		return nil
	}
	result := extractGeminiSystemInstruction(root["systemInstruction"])
	result = append(result, extractGeminiSystemInstruction(root["system_instruction"])...)
	result = append(result, extractGemini(root["contents"])...)
	result = append(result, extractGemini(root["content"])...)
	result = append(result, extractGeminiInstances(root["instances"])...)
	if requests, ok := root["requests"].([]any); ok {
		for _, item := range requests {
			request, ok := item.(map[string]any)
			if !ok {
				continue
			}
			result = append(result, extractGeminiSystemInstruction(request["systemInstruction"])...)
			result = append(result, extractGeminiSystemInstruction(request["system_instruction"])...)
			result = append(result, extractGemini(request["contents"])...)
			result = append(result, extractGemini(request["content"])...)
			result = append(result, extractGeminiInstances(request["instances"])...)
		}
	}
	return result
}

func extractGeminiSystemInstruction(value any) []promptSegment {
	switch typed := value.(type) {
	case string:
		if text := strings.TrimSpace(typed); text != "" {
			return []promptSegment{{text: text, role: "system"}}
		}
	case map[string]any:
		if parts, ok := typed["parts"].([]any); ok {
			result := make([]promptSegment, 0, len(parts))
			for _, part := range parts {
				if object, ok := part.(map[string]any); ok {
					if text := stringValue(object["text"]); text != "" {
						result = append(result, promptSegment{text: text, role: "system"})
					}
				}
			}
			return result
		}
		return systemPromptSegments(contentTexts(typed))
	case []any:
		segments := extractGemini(typed)
		for index := range segments {
			segments[index].user = false
			segments[index].role = "system"
		}
		return segments
	}
	return nil
}

func extractGeminiInstances(value any) []promptSegment {
	instances, ok := value.([]any)
	if !ok {
		return nil
	}
	result := make([]promptSegment, 0, len(instances))
	for _, item := range instances {
		if instance, ok := item.(map[string]any); ok {
			if prompt := stringValue(instance["prompt"]); prompt != "" {
				result = append(result, promptSegment{text: prompt, user: true, role: "user"})
			}
		}
	}
	return result
}

func extractMediaPrompts(root map[string]any) []string {
	if root == nil {
		return nil
	}
	result := make([]string, 0, 4)
	seen := map[string]struct{}{}
	var walk func(any, string)
	walk = func(value any, key string) {
		switch typed := value.(type) {
		case map[string]any:
			keys := make([]string, 0, len(typed))
			for childKey := range typed {
				keys = append(keys, childKey)
			}
			sort.Strings(keys)
			for _, childKey := range keys {
				walk(typed[childKey], childKey)
			}
		case []any:
			for _, item := range typed {
				walk(item, key)
			}
		case string:
			if !legacyIsMediaPromptKey(key) || legacyLooksLikeMediaPayload(typed) {
				return
			}
			text := strings.TrimSpace(typed)
			if text == "" {
				return
			}
			if _, duplicate := seen[text]; duplicate {
				return
			}
			seen[text] = struct{}{}
			result = append(result, text)
		}
	}
	walk(root, "")
	return result
}

func legacyIsMediaPromptKey(key string) bool {
	normalized := strings.NewReplacer("_", "", "-", "").Replace(strings.ToLower(strings.TrimSpace(key)))
	switch normalized {
	case "prompt", "inputprompt", "textprompt", "description", "query", "lyrics", "negativeprompt",
		"positiveprompt", "gptdescriptionprompt", "prompten", "finalprompt", "finalzhprompt",
		"origprompt", "actualprompt", "imageprompt", "input":
		return true
	default:
		return false
	}
}

func contentTexts(value any) []string {
	switch typed := value.(type) {
	case string:
		return []string{typed}
	case []any:
		result := make([]string, 0, len(typed))
		for _, part := range typed {
			object, ok := part.(map[string]any)
			if !ok {
				continue
			}
			typeName := strings.ToLower(stringValue(object["type"]))
			if typeName != "" && typeName != "text" && typeName != "input_text" && typeName != "output_text" {
				continue
			}
			if text := stringValue(object["text"]); text != "" {
				result = append(result, text)
			}
		}
		return result
	case map[string]any:
		if text := stringValue(typed["text"]); text != "" {
			return []string{text}
		}
	}
	return nil
}

func normalizeSegmentsLatestUserFirst(values []promptSegment) []string {
	normalized := normalizedPromptSegments(values)
	if len(normalized) == 0 {
		return nil
	}
	priorityIndex := len(normalized) - 1
	for index := len(normalized) - 1; index >= 0; index-- {
		if isUserSegment(normalized[index]) {
			priorityIndex = index
			break
		}
	}
	result := make([]string, 0, len(normalized))
	result = append(result, normalized[priorityIndex].text)
	for index, segment := range normalized {
		if index != priorityIndex {
			result = append(result, segment.text)
		}
	}
	return result
}

// blockingSegmentsLatestUserAndPreviousOutput limits synchronous guard input to
// the current user turn and the nearest preceding assistant/model turn. It is
// deliberately opt-in because full transcript scanning remains stronger at
// finding client-controlled content placed in older or non-user messages.
//
// The latest turn's parts are returned as the first priority segments, not
// joined: buildPrioritizedScanText joins them with the same "\n\n", so the
// scan text is unchanged and no extra copy is made.
func blockingSegmentsLatestUserAndPreviousOutput(values []promptSegment) (segments []string, priority int) {
	normalized := normalizedPromptSegments(values)
	latestUserStart := latestUserSegmentStart(normalized)
	if latestUserStart < 0 {
		// A request without user content cannot be narrowed safely. Preserve the
		// established full-snapshot behavior for unusual protocol payloads.
		return normalizeSegmentsLatestUserFirst(values), 1
	}
	latestUserEnd := latestUserStart
	for latestUserEnd < len(normalized) && isUserSegment(normalized[latestUserEnd]) {
		latestUserEnd++
	}
	// A single client turn may have several text content parts. Keep them in
	// the priority segment so every part of the latest input is scanned before
	// the prior output begins.
	selected := normalized[latestUserStart:latestUserEnd:latestUserEnd]
	priority = len(selected)
	for index := latestUserStart - 1; index >= 0; index-- {
		if !isAssistantOutputSegment(normalized[index]) {
			continue
		}
		start := index
		for start > 0 && isAssistantOutputSegment(normalized[start-1]) {
			start--
		}
		selected = append(selected, normalized[start:index+1]...)
		break
	}
	return promptSegmentTexts(selected), priority
}

func normalizedPromptSegments(values []promptSegment) []promptSegment {
	normalized := make([]promptSegment, 0, len(values))
	for _, value := range values {
		value.text = strings.TrimSpace(value.text)
		if value.text != "" {
			normalized = append(normalized, value)
		}
	}
	return normalized
}

func latestUserSegmentStart(values []promptSegment) int {
	latest := -1
	for index := len(values) - 1; index >= 0; index-- {
		if isUserSegment(values[index]) {
			latest = index
			break
		}
	}
	for latest > 0 && isUserSegment(values[latest-1]) {
		latest--
	}
	return latest
}

func isUserSegment(segment promptSegment) bool {
	return segment.user || segment.role == "user"
}

func isAssistantOutputSegment(segment promptSegment) bool {
	return segment.role == "assistant" || segment.role == "model"
}

func promptSegmentTexts(values []promptSegment) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		result = append(result, value.text)
	}
	return result
}

func promptSegmentsForRole(texts []string, role string) []promptSegment {
	result := make([]promptSegment, 0, len(texts))
	for _, text := range texts {
		result = append(result, promptSegment{text: text, user: role == "" || role == "user", role: role})
	}
	return result
}

func userPromptSegments(texts []string) []promptSegment {
	return promptSegmentsForRole(texts, "user")
}

func systemPromptSegments(texts []string) []promptSegment {
	return promptSegmentsForRole(texts, "system")
}

func stringValue(value any) string {
	text, _ := value.(string)
	return strings.TrimSpace(text)
}

func legacyLooksLikeMediaPayload(value string) bool {
	trimmed := strings.TrimSpace(value)
	lower := strings.ToLower(trimmed)
	if strings.HasPrefix(lower, "data:image/") || strings.HasPrefix(lower, "data:video/") ||
		strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://") {
		return true
	}
	if len(trimmed) >= 256 {
		for _, r := range trimmed {
			alphaNumeric := (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
			if !alphaNumeric && r != '+' && r != '/' && r != '=' {
				return false
			}
		}
		return true
	}
	return false
}

func isClientInstructionRole(role string) bool {
	switch strings.ToLower(strings.TrimSpace(role)) {
	case "user", "system", "developer", "assistant", "tool", "model":
		return true
	default:
		return false
	}
}
