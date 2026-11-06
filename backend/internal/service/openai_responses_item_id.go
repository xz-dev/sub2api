package service

import (
	"fmt"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func openAIResponsesInputItemIDPrefix(itemType string) (string, bool) {
	switch strings.TrimSpace(itemType) {
	case "message":
		return "msg", true
	case "reasoning":
		return "rs", true
	case "web_search_call":
		return "ws", true
	case "custom_tool_call":
		return openAIResponsesToolCallIDPrefix(itemType), true
	case "tool_search_call":
		return openAIResponsesToolCallIDPrefix(itemType), true
	case "custom_tool_call_output":
		// Although custom calls use ctc IDs, OpenAI validates replayed custom
		// call output item IDs against the generic fc namespace.
		return "fc", true
	default:
		if isCodexToolCallInputType(itemType) {
			return openAIResponsesToolCallIDPrefix(itemType), true
		}
		return "", false
	}
}

func openAIResponsesToolCallIDPrefix(itemType string) string {
	switch strings.TrimSpace(itemType) {
	case "custom_tool_call", "custom_tool_call_output":
		return "ctc"
	case "tool_search_call", "tool_search_output":
		return "tsc"
	default:
		return "fc"
	}
}

// Invalid replayed IDs are removed rather than rewritten because a fabricated
// ID may point at a different upstream object.
func shouldStripOpenAIResponsesInputItemID(itemType, id string) bool {
	prefix, constrained := openAIResponsesInputItemIDPrefix(itemType)
	if !constrained {
		return false
	}
	return id == "" || len(id) > 64 || !strings.HasPrefix(id, prefix)
}

func shouldStripOpenAIResponsesNonPairCallID(itemType string) bool {
	switch strings.TrimSpace(itemType) {
	case "message", "reasoning", "image_generation_call":
		return true
	default:
		return false
	}
}

func sanitizeOpenAIResponsesInputItemIDs(body []byte) ([]byte, bool, error) {
	input := firstRawJSONMember(parseRawJSONView(body), "input")
	if !input.IsArray() {
		return body, false, nil
	}

	type inputItem struct {
		raw         string
		stripID     bool
		stripCallID bool
	}

	parse := func(item gjson.Result) inputItem {
		parsed := inputItem{raw: item.Raw}
		if item.IsObject() {
			itemType := item.Get("type")
			id := item.Get("id")
			trimmedItemType := strings.TrimSpace(itemType.String())
			parsed.stripCallID = item.Get("call_id").Exists() && shouldStripOpenAIResponsesNonPairCallID(trimmedItemType)
			if id.Type == gjson.String {
				parsed.stripID = shouldStripOpenAIResponsesInputItemID(trimmedItemType, id.String())
			}
		}
		return parsed
	}
	// Find out first, keeping nothing, whether any item changes (review
	// rr3: the item list of a body of many small items cost about 7 x the
	// body on every passthrough request that changed nothing).
	hasSanitization := false
	input.ForEach(func(_, item gjson.Result) bool {
		parsed := parse(item)
		hasSanitization = parsed.stripID || parsed.stripCallID
		return !hasSanitization
	})
	if !hasSanitization {
		return body, false, nil
	}
	// One output buffer, no list of the items (rr6 follow-up: the two lists
	// cost about 3 x a body of many small items on the verified-raw path).
	index := -1
	out, err := rebuildOpenAIRawInput(body, input, func(raw gjson.Result) (string, bool, error) {
		index++
		item := parse(raw)
		if !item.stripID && !item.stripCallID {
			return item.raw, true, nil
		}
		edited := item.raw
		var err error
		if item.stripID {
			if edited, err = sjson.Delete(edited, "id"); err != nil {
				return "", false, fmt.Errorf("delete input.%d.id: %w", index, err)
			}
		}
		if item.stripCallID {
			if edited, err = sjson.Delete(edited, "call_id"); err != nil {
				return "", false, fmt.Errorf("delete input.%d.call_id: %w", index, err)
			}
		}
		return edited, true, nil
	})
	if err != nil {
		return nil, false, err
	}
	return out, true, nil
}
