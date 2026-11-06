package service

import (
	"strings"
	"unsafe"

	"github.com/tidwall/gjson"
)

// openAIRequestSkeletonDecodeMaxBytes: the largest skeleton decoded
// without the k_conv top-up (rewriteOpenAIRequestSkeleton). Measured: 64
// KiB of escaped instructions costs 842 KB with the new body.
const openAIRequestSkeletonDecodeMaxBytes = 64 << 10

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
//
// chargeConversion is taken before the decode when the skeleton has more
// than openAIRequestSkeletonDecodeMaxBytes bytes (rr9: one large string,
// for example instructions, is copied by the decode and again by the
// re-encode; about 13 x for escaped text) or more than
// contentSeedDecodeMaxValues JSON values (rr6 follow-up: a large tools
// array or additional_tools item of many small parts cost about 20 x its
// bytes on the verified-raw path, charged k_skel); the byte test comes
// first, before the skeleton is built. A smaller skeleton costs at most
// about 1 MiB.
func rewriteOpenAIRequestSkeleton(body []byte, fn func(map[string]any) bool, chargeConversion func() error) (out []byte, changed, ok bool, err error) {
	root := parseRawJSONView(body)
	if !root.IsObject() || !gjson.ValidBytes(body) {
		return body, false, false, nil
	}
	keys := make([]string, 0, 32)
	if hasDuplicateJSONObjectKeysIn(root, &keys) {
		return body, false, false, nil
	}
	// Raw members throughout (rr10): Result.ForEach and Get unescape every
	// root string they pass (13 MB for 12.9 MB of escaped instructions).
	input := firstRawJSONMember(root, "input")
	keptItem := func(item gjson.Result) bool {
		return strings.TrimSpace(lastRawObjectMember(item, "type").String()) == "additional_tools"
	}
	// An upper bound of the skeleton's size (one comma too many per
	// container), without building it.
	size := 2
	forEachRawJSONMember(root, func(name, value gjson.Result) bool {
		size += len(name.Raw) + 2 // ':' and ','
		if name.Str != "input" || !input.IsArray() {
			size += len(value.Raw)
			return true
		}
		size += 2
		forEachRawJSONMember(input, func(_, item gjson.Result) bool {
			if keptItem(item) {
				size += len(item.Raw) + 1
			}
			return true
		})
		return true
	})
	charged := false
	if size > openAIRequestSkeletonDecodeMaxBytes {
		if err := chargeConversion(); err != nil {
			return body, false, true, err
		}
		charged = true
	}
	skeleton := make([]byte, 0, size)
	skeleton = append(skeleton, '{')
	forEachRawJSONMember(root, func(name, value gjson.Result) bool {
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
		forEachRawJSONMember(input, func(_, item gjson.Result) bool {
			if !keptItem(item) {
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
	if !charged && rawJSONValuesAbove(unsafe.String(unsafe.SliceData(skeleton), len(skeleton)), contentSeedDecodeMaxValues) {
		if err := chargeConversion(); err != nil {
			return body, false, true, err
		}
	}
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
