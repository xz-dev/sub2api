package service

import (
	"strconv"
	"strings"

	"github.com/tidwall/gjson"
)

// forEachRawJSONMember is gjson's Result.ForEach without unescaping values
// (rr9): ForEach fills Str of every string value it passes, so each pass
// over an item copied a large escaped string once. It walks the members of
// an object view (or the elements of an array view) exactly as ForEach
// does, including on malformed input, and gives the same names, Raw, Type,
// Num and Index; a string value's Str is left empty (use
// rawJSONStringValue). A name is unescaped as ForEach does it (no copy
// without an escape).
func forEachRawJSONMember(t gjson.Result, iterator func(key, value gjson.Result) bool) {
	if !t.Exists() {
		return
	}
	if t.Type != gjson.JSON {
		iterator(gjson.Result{}, t)
		return
	}
	json := t.Raw
	var obj bool
	var i int
	var key gjson.Result
	for ; i < len(json); i++ {
		if json[i] == '{' {
			i++
			key.Type = gjson.String
			obj = true
			break
		} else if json[i] == '[' {
			i++
			key.Type = gjson.Number
			key.Num = -1
			break
		}
		if json[i] > ' ' {
			return
		}
	}
	var idx int
	for ; i < len(json); i++ {
		if obj {
			if json[i] != '"' {
				continue
			}
			s := i
			end, esc, ok := rawJSONStringEnd(json, i+1)
			if !ok {
				return
			}
			i = end
			key.Raw = json[s:end]
			if esc {
				key.Str = gjson.Parse(key.Raw).Str // a name: small on every on-list body
			} else {
				key.Str = key.Raw[1 : len(key.Raw)-1]
			}
			key.Index = s + t.Index
		} else {
			key.Num++
		}
		for ; i < len(json); i++ {
			if json[i] <= ' ' || json[i] == ',' || json[i] == ':' {
				continue
			}
			break
		}
		s := i
		var value gjson.Result
		var ok bool
		i, value, ok = rawJSONParseAny(json, i)
		if !ok {
			return
		}
		if t.Indexes != nil {
			if idx < len(t.Indexes) {
				value.Index = t.Indexes[idx]
			}
		} else {
			value.Index = s + t.Index
		}
		if !iterator(key, value) {
			return
		}
		idx++
	}
}

// firstRawJSONMember is object.Get(name) for a plain member name, without
// unescaping the value (see forEachRawJSONMember): the first member named
// name, as gjson selects it.
func firstRawJSONMember(object gjson.Result, name string) gjson.Result {
	var found gjson.Result
	if !object.IsObject() {
		return found
	}
	forEachRawJSONMember(object, func(key, value gjson.Result) bool {
		if key.Str == name {
			found = value
			return false
		}
		return true
	})
	return found
}

// rawJSONStringValue is value.String() for a value from
// forEachRawJSONMember: a string is unescaped here, once.
func rawJSONStringValue(value gjson.Result) string {
	if value.Type != gjson.String {
		return value.String()
	}
	if strings.IndexByte(value.Raw, '\\') < 0 {
		return value.Raw[1 : len(value.Raw)-1]
	}
	return gjson.Parse(value.Raw).Str
}

// rawJSONStringIs reports whether the JSON string raw unescapes to want,
// without unescaping a raw text that cannot (an escape is at least as long
// as what it stands for, at most 6 bytes for one).
func rawJSONStringIs(raw, want string) bool {
	if strings.IndexByte(raw, '\\') < 0 {
		return len(raw) == len(want)+2 && raw[1:len(raw)-1] == want
	}
	if len(raw) > 6*len(want)+2 {
		return false
	}
	return gjson.Parse(raw).Str == want
}

// rawJSONStringEnd is gjson's parseString: i is just after the opening
// quote; end is just after the closing one; esc reports a backslash.
func rawJSONStringEnd(json string, i int) (end int, esc, ok bool) {
	for ; i < len(json); i++ {
		if json[i] > '\\' {
			continue
		}
		if json[i] == '"' {
			return i + 1, false, true
		}
		if json[i] == '\\' {
			i++
			for ; i < len(json); i++ {
				if json[i] > '\\' {
					continue
				}
				if json[i] == '"' {
					// look for an escaped slash
					if json[i-1] == '\\' {
						n := 0
						for j := i - 2; j > 0; j-- {
							if json[j] != '\\' {
								break
							}
							n++
						}
						if n%2 == 0 {
							continue
						}
					}
					return i + 1, true, true
				}
			}
			break
		}
	}
	return i, false, false
}

// rawJSONParseAny is gjson's parseAny (hit) without filling Str.
func rawJSONParseAny(json string, i int) (int, gjson.Result, bool) {
	var res gjson.Result
	for ; i < len(json); i++ {
		if json[i] == '{' || json[i] == '[' {
			i, res.Raw = rawJSONSquash(json, i)
			res.Type = gjson.JSON
			return i, res, true
		}
		if json[i] <= ' ' {
			continue
		}
		var num bool
		switch json[i] {
		case '"':
			end, _, ok := rawJSONStringEnd(json, i+1)
			if !ok {
				return end, res, false
			}
			res.Type = gjson.String
			res.Raw = json[i:end]
			return end, res, true
		case 'n':
			if i+1 < len(json) && json[i+1] != 'u' {
				num = true
				break
			}
			fallthrough
		case 't', 'f':
			vc := json[i]
			s := i
			for i++; i < len(json); i++ {
				if json[i] < 'a' || json[i] > 'z' {
					break
				}
			}
			res.Raw = json[s:i]
			switch vc {
			case 't':
				res.Type = gjson.True
			case 'f':
				res.Type = gjson.False
			}
			return i, res, true
		case '+', '-', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9',
			'i', 'I', 'N':
			num = true
		}
		if num {
			s := i
			for i++; i < len(json); i++ {
				if json[i] <= ' ' || json[i] == ',' || json[i] == ']' || json[i] == '}' {
					break
				}
			}
			res.Raw = json[s:i]
			res.Type = gjson.Number
			res.Num, _ = strconv.ParseFloat(res.Raw, 64)
			return i, res, true
		}
	}
	return i, res, false
}

// rawJSONSquash is gjson's parseSquash: json[i] is '{' or '['; it returns
// the index after the matching close ('(' and ')' count too, as there).
func rawJSONSquash(json string, i int) (int, string) {
	s := i
	i++
	depth := 1
	for i < len(json) {
		switch json[i] {
		case '"':
			i++
			s2 := i
			for i < len(json) {
				if json[i] == '"' {
					n := 0
					for j := i - 1; j > s2-1 && json[j] == '\\'; j-- {
						n++
					}
					if n%2 == 0 {
						break
					}
				}
				i++
			}
		case '{', '[', '(':
			depth++
		case '}', ']', ')':
			depth--
			if depth == 0 {
				i++
				return i, json[s:i]
			}
		}
		i++
	}
	return i, json[s:]
}
