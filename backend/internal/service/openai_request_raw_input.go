package service

import "github.com/tidwall/gjson"

// replaceOpenAIRawInput copies unchanged image strings only into the final
// request, without allocating a second complete input array first.
func replaceOpenAIRawInput(body []byte, input gjson.Result, items []string) []byte {
	size := len(body) - len(input.Raw) + 2
	for index, item := range items {
		size += len(item)
		if index > 0 {
			size++
		}
	}
	result := make([]byte, 0, size)
	result = append(result, body[:input.Index]...)
	result = append(result, '[')
	for index, item := range items {
		if index > 0 {
			result = append(result, ',')
		}
		result = append(result, item...)
	}
	result = append(result, ']')
	return append(result, body[input.Index+len(input.Raw):]...)
}

// appendRawInputItems appends input's items to dst as a JSON array, each
// item as fn gives it (keep false drops it), without a list of the items.
func appendRawInputItems(dst []byte, input gjson.Result, fn func(item gjson.Result) (string, bool, error)) ([]byte, error) {
	dst = append(dst, '[')
	first := true
	var err error
	input.ForEach(func(_, item gjson.Result) bool {
		out, keep, itemErr := fn(item)
		if itemErr != nil {
			err = itemErr
			return false
		}
		if keep {
			if !first {
				dst = append(dst, ',')
			}
			first = false
			dst = append(dst, out...)
		}
		return true
	})
	if err != nil {
		return nil, err
	}
	return append(dst, ']'), nil
}

// rebuildOpenAIRawInput is body with its input array rebuilt by fn
// (appendRawInputItems) in one buffer the size of body.
func rebuildOpenAIRawInput(body []byte, input gjson.Result, fn func(item gjson.Result) (string, bool, error)) ([]byte, error) {
	result := make([]byte, 0, len(body)+4<<10)
	result = append(result, body[:input.Index]...)
	result, err := appendRawInputItems(result, input, fn)
	if err != nil {
		return nil, err
	}
	return append(result, body[input.Index+len(input.Raw):]...), nil
}

// The standard decoder keeps the last duplicate key; GJSON selects the first.
// hasDuplicateJSONObjectKeysIn is hasDuplicateJSONObjectKeys with the keys
// kept in *scratch, reused across calls: no map for an object of up to 32
// keys (review rr3: a map per item of a body of many small items). Values
// are never unescaped (forEachRawJSONMember; rr9).
func hasDuplicateJSONObjectKeysIn(object gjson.Result, scratch *[]string) bool {
	keys := (*scratch)[:0]
	duplicate, large := false, false
	forEachRawJSONMember(object, func(key, _ gjson.Result) bool {
		if len(keys) == 32 {
			large = true
			return false
		}
		for _, seen := range keys {
			if seen == key.Str {
				duplicate = true
				return false
			}
		}
		keys = append(keys, key.Str)
		return true
	})
	*scratch = keys[:0]
	if large {
		return hasDuplicateJSONObjectKeys(object)
	}
	return duplicate
}

func hasDuplicateJSONObjectKeys(object gjson.Result) bool {
	seen := make(map[string]struct{})
	duplicate := false
	forEachRawJSONMember(object, func(key, _ gjson.Result) bool {
		if _, exists := seen[key.Str]; exists {
			duplicate = true
			return false
		}
		seen[key.Str] = struct{}{}
		return true
	})
	return duplicate
}

// lastRawObjectMember returns the value of the last member named key of a
// JSON object view: the standard decoder keeps the last duplicate. Only
// that member's string value is unescaped (rr10: Result.ForEach unescaped
// every string member of the object, a large content string included, on
// each call).
func lastRawObjectMember(object gjson.Result, key string) gjson.Result {
	var last gjson.Result
	forEachRawJSONMember(object, func(name, value gjson.Result) bool {
		if name.Str == key {
			last = value
		}
		return true
	})
	if last.Type == gjson.String {
		last.Str = gjson.Parse(last.Raw).Str
	}
	return last
}

// rewriteRawObjectMember returns body with the value target (a member value
// of root, the object view of all of body) replaced by what write appends,
// without decoding the body. With dedup set, it also keeps only the last
// member of each name, as decoding and re-encoding would. size is the
// expected length of the written value. The result is one new body.
func rewriteRawObjectMember(body []byte, root, target gjson.Result, dedup bool, size int, write func([]byte) []byte) []byte {
	out := make([]byte, 0, len(body)-len(target.Raw)+size)
	if !dedup {
		out = append(out, body[:target.Index]...)
		out = write(out)
		return append(out, body[target.Index+len(target.Raw):]...)
	}
	last := make(map[string]int)
	position := 0
	root.ForEach(func(name, _ gjson.Result) bool {
		last[name.Str] = position
		position++
		return true
	})
	out = append(out, '{')
	position = 0
	root.ForEach(func(name, value gjson.Result) bool {
		if last[name.Str] == position {
			if len(out) > 1 {
				out = append(out, ',')
			}
			out = append(out, name.Raw...)
			out = append(out, ':')
			if value.Index == target.Index {
				out = write(out)
			} else {
				out = append(out, value.Raw...)
			}
		}
		position++
		return true
	})
	return append(out, '}')
}
