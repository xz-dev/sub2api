package service

import (
	"bytes"
	"errors"
	"math"
	"strconv"
	"strings"
	"unsafe"

	"github.com/tidwall/gjson"
)

// forEachRawJSONMember is gjson's Result.ForEach without unescaping values
// (rr9): ForEach fills Str of every string value it passes, so each pass
// over an item copied a large escaped string once. It walks the members of
// an object view (or the elements of an array view) exactly as ForEach
// does, including on malformed input, and gives the same names, Raw, Type,
// Num and Index; a string value's Str is left empty
// (derivedKeyBudget.stringValue unescapes one within a budget). A number
// is parsed by parseJSONNumberBounded (same value, no copy of a long
// literal). A name is unescaped as ForEach does it (no copy without an
// escape).
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
				// One copy of the name per pass. On an on-list body the
				// objects the verified-raw list walks have no escaped
				// name and none over rawNameMaxBytes (request_raw_list.go);
				// names inside values it does not walk (metadata keys up to
				// rawIdentMaxBytes, tool parameters, schemas) can be escaped
				// and long, and are in the charge-cover generator
				// (adv_rv_*key*, adv_rv1_key_*).
				key.Str = gjson.Parse(key.Raw).Str
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

// rawJSONMemberString is value.String() for a value from
// forEachRawJSONMember or firstRawJSONMember: a string is unescaped here
// (no copy without an escape).
func rawJSONMemberString(value gjson.Result) string {
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
			res.Num, _ = parseJSONNumberBounded(res.Raw)
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

// jsonNumberParseMaxBytes: a number literal longer than this is not handed
// to strconv as sent (rr10). strconv.ParseFloat copies the whole literal
// into its error when the value is out of range: a max_output_tokens of 16
// M digits cost 144 MB in copies over the passes that parse it.
const jsonNumberParseMaxBytes = 32

// errJSONNumberSyntax: a long literal that is not a JSON number.
var errJSONNumberSyntax = errors.New("json number: invalid syntax")

// parseJSONNumberBounded is strconv.ParseFloat(raw, 64) for a JSON number
// literal, without allocating for a long one: a literal of more than
// jsonNumberParseMaxBytes bytes is first written as its 800 leading
// significant digits, a sticky 1 when a later digit is not 0, and the
// decimal exponent (saturated), on the stack. strconv keeps exactly 800
// digits and a truncation flag itself, so it parses to the same float64,
// rounding included; an exponent past the float64 range is still out of
// range. A long literal that is not JSON number syntax
// (strconv also takes hex and Inf forms) is a syntax error with value 0.
func parseJSONNumberBounded(raw string) (float64, error) {
	if len(raw) <= jsonNumberParseMaxBytes {
		return strconv.ParseFloat(raw, 64)
	}
	var buf [shortJSONNumberMax]byte
	short, ok := shortJSONNumber(raw, buf[:0])
	if !ok {
		return 0, errJSONNumberSyntax
	}
	if e := bytes.LastIndexByte(short, 'e'); e > 0 && len(short)-e > 4 && short[e+1] != '-' {
		// 0.d... x 10^1000 or more: past float64's range without asking
		// strconv, whose range error would copy the short form.
		if short[0] == '-' {
			return math.Inf(-1), errJSONNumberRange
		}
		return math.Inf(1), errJSONNumberRange
	}
	return strconv.ParseFloat(unsafe.String(unsafe.SliceData(short), len(short)), 64)
}

// errJSONNumberRange: a long literal past float64's range (the value is
// ±Inf, as strconv gives it).
var errJSONNumberRange = errors.New("json number: value out of range")

// shortJSONNumberMax: sign, "0.", 801 digits, 'e' and the exponent.
const shortJSONNumberMax = 832

// shortJSONNumber writes the JSON number raw into dst as
// [-]0.<digits>e<exp> with at most 801 significant digits; ok=false when
// raw is not JSON number syntax.
func shortJSONNumber(raw string, dst []byte) ([]byte, bool) {
	const keep = 800         // strconv's decimal holds 800 digits
	const expLimit = 1 << 20 // far past float64's range either way
	i := 0
	neg := false
	if i < len(raw) && raw[i] == '-' {
		neg, i = true, i+1
	}
	digits := func(from int) int {
		for from < len(raw) && raw[from] >= '0' && raw[from] <= '9' {
			from++
		}
		return from
	}
	intEnd := digits(i)
	if intEnd == i || (raw[i] == '0' && intEnd-i > 1) {
		return nil, false
	}
	intDigits, fracDigits := raw[i:intEnd], ""
	i = intEnd
	if i < len(raw) && raw[i] == '.' {
		end := digits(i + 1)
		if end == i+1 {
			return nil, false
		}
		fracDigits, i = raw[i+1:end], end
	}
	exp := 0
	if i < len(raw) && (raw[i] == 'e' || raw[i] == 'E') {
		i++
		expNeg := false
		if i < len(raw) && (raw[i] == '+' || raw[i] == '-') {
			expNeg, i = raw[i] == '-', i+1
		}
		end := digits(i)
		if end == i {
			return nil, false
		}
		for _, c := range raw[i:end] {
			if exp < expLimit {
				exp = exp*10 + int(c-'0')
			}
		}
		if expNeg {
			exp = -exp
		}
		i = end
	}
	if i != len(raw) {
		return nil, false
	}
	// value = 0.<intDigits fracDigits> x 10^(len(intDigits)+exp)
	point := len(intDigits) + exp
	if neg {
		dst = append(dst, '-')
	}
	dst = append(dst, '0', '.')
	n, sticky := 0, false
	for _, part := range [2]string{intDigits, fracDigits} {
		for j := 0; j < len(part); j++ {
			c := part[j]
			switch {
			case n == 0 && c == '0':
				point-- // a leading zero
			case n < keep:
				dst = append(dst, c)
				n++
			case c != '0':
				sticky = true
			}
		}
	}
	if n == 0 {
		return append(dst, '0'), true
	}
	if sticky {
		dst = append(dst, '1')
	}
	point = max(-expLimit, min(expLimit, point))
	dst = append(dst, 'e')
	return strconv.AppendInt(dst, int64(point), 10), true
}

// rawJSONRootMember is gjson.GetBytes(body, name) for a plain root member
// name without its copies (rr10): GetBytes copies the value's Raw and gjson
// parses a number with strconv.ParseFloat, which copies a literal that is
// out of range (176 MB for a max_output_tokens of 16 M digits). The value
// is read raw from a view of body (firstRawJSONMember, a number parsed by
// parseJSONNumberBounded); only a string value is unescaped into Str,
// which copies nothing unless it has an escape. The result must not be
// kept past body.
func rawJSONRootMember(body []byte, name string) gjson.Result {
	v := firstRawJSONMember(parseRawJSONView(body), name)
	if v.Type == gjson.String {
		v.Str = rawJSONMemberString(v)
	}
	return v
}
