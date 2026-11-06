package service

import (
	"strings"

	"github.com/tidwall/gjson"
)

// rewriteOpenAIRequestSkeleton runs fn on the root object of a Responses
// body without decoding its input items (review rr4: a full decode costs
// about 20-30 x a body of many small items). fn sees every root member;
// "input", when it is an array, holds only its additional_tools items
// (enough for hasOpenAIImageGenerationTool). fn must not change "input".
// When fn changes the map, the members are re-encoded and the original
// input array is put back as sent: one new body.
//
// ok=false (nothing done) when the body is not a valid JSON object or has
// a duplicate root name; the caller then decodes all of it.
func rewriteOpenAIRequestSkeleton(body []byte, fn func(map[string]any) bool) (out []byte, changed, ok bool, err error) {
	root := parseRawJSONView(body)
	if !root.IsObject() || !gjson.ValidBytes(body) {
		return body, false, false, nil
	}
	keys := make([]string, 0, 32)
	if hasDuplicateJSONObjectKeysIn(root, &keys) {
		return body, false, false, nil
	}
	input := root.Get("input")
	skeleton := make([]byte, 0, 1024)
	skeleton = append(skeleton, '{')
	root.ForEach(func(name, value gjson.Result) bool {
		if len(skeleton) > 1 {
			skeleton = append(skeleton, ',')
		}
		skeleton = append(skeleton, name.Raw...)
		skeleton = append(skeleton, ':')
		if name.Str != "input" || !input.IsArray() {
			skeleton = append(skeleton, value.Raw...)
			return true
		}
		skeleton = append(skeleton, '[')
		first := true
		input.ForEach(func(_, item gjson.Result) bool {
			if strings.TrimSpace(lastRawObjectMember(item, "type").String()) != "additional_tools" {
				return true
			}
			if !first {
				skeleton = append(skeleton, ',')
			}
			first = false
			skeleton = append(skeleton, item.Raw...)
			return true
		})
		skeleton = append(skeleton, ']')
		return true
	})
	skeleton = append(skeleton, '}')
	var decoded map[string]any
	if err := decodeOpenAIJSONUseNumber(skeleton, &decoded); err != nil {
		return body, false, true, err
	}
	if !fn(decoded) {
		return body, false, true, nil
	}
	if input.IsArray() {
		delete(decoded, "input")
	}
	encoded, err := marshalOpenAIUpstreamJSON(decoded)
	if err != nil {
		return body, false, true, err
	}
	if !input.IsArray() {
		return encoded, true, true, nil
	}
	out = make([]byte, 0, len(encoded)+len(input.Raw)+10)
	out = append(out, encoded[:len(encoded)-1]...)
	if len(encoded) > 2 {
		out = append(out, ',')
	}
	out = append(out, `"input":`...)
	out = append(out, input.Raw...)
	return append(out, '}'), true, true, nil
}
