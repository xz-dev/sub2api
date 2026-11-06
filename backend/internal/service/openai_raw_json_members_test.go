//go:build unit

package service

import (
	"math"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// rawMembersParityCorpus: valid, duplicate, escaped, invalid UTF-8, nested
// and malformed JSON texts on which forEachRawJSONMember must walk exactly
// as gjson's ForEach.
var rawMembersParityCorpus = []string{
	`{}`, `[]`, `  { "a" : 1 , "b":[1,2,{"c":"d"}] }  `,
	`{"type":"message","role":"user","content":"hi"}`,
	`{"type":"a","type":"b","type":"compaction_trigger"}`,
	`{"t\u0079pe":"compaction\u005ftrigger","x":"\"q\" \\ \/ \n \u00e9 \ud83d\ude00"}`,
	`{"a\"b":"c\\\"","d\\":"e\\\\","f":"\\\\\""}`,
	"{\"bad\":\"\xff\xc3 x\",\"\xfekey\":[\"\xc3\"]}",
	`[1,-2.5e3,true,false,null,"s",{"k":[[],{}]},[{"a":"(b)"}]]`,
	`{"n":nan,"i":Infinity,"m":-0,"o":{"p":{"q":{"r":[1,[2,[3]]]}}}}`,
	`{"content":[{"type":"input_image","image_url":"data:image/png;base64,"},{"type":"input_text","text":"x"}]}`,
	`{"paren":"(",")":"[","x":{"y":"}"}}`,
	// malformed
	`{"a":1} "b":2}`, `{"a":"unterminated`, `{"a":[1,2}`, `{"a" 1 "b" 2}`, `[1,,2]`, `{"a":tru}`, `{"a":nul}`, `x`, `{"a":`, `{"a":"b\`,
}

func TestForEachRawJSONMemberParity(t *testing.T) {
	type member struct {
		key, value gjson.Result
	}
	walk := func(r gjson.Result, raw bool) []member {
		var out []member
		f := r.ForEach
		if raw {
			f = func(it func(key, value gjson.Result) bool) { forEachRawJSONMember(r, it) }
		}
		f(func(key, value gjson.Result) bool {
			out = append(out, member{key, value})
			return true
		})
		return out
	}
	var check func(name string, r gjson.Result, depth int)
	check = func(name string, r gjson.Result, depth int) {
		want, got := walk(r, false), walk(r, true)
		require.Len(t, got, len(want), name)
		for i := range want {
			w, g := want[i], got[i]
			require.Equal(t, w.key, g.key, "%s key %d", name, i)
			require.Equal(t, w.value.Raw, g.value.Raw, "%s value %d", name, i)
			require.Equal(t, w.value.Type, g.value.Type, "%s type %d", name, i)
			require.Equal(t, w.value.Index, g.value.Index, "%s index %d", name, i)
			if w.value.Type == gjson.Number && !(math.IsNaN(w.value.Num) && math.IsNaN(g.value.Num)) {
				require.Equal(t, w.value.Num, g.value.Num, name)
			}
			require.Equal(t, w.value.String(), rawJSONStringValue(g.value), "%s string %d", name, i)
			if w.value.Type == gjson.String {
				require.Empty(t, g.value.Str, "a value is never unescaped")
				for _, want := range []string{"compaction_trigger", "b", w.value.Str, w.value.Str + "x"} {
					require.Equal(t, w.value.Str == want, rawJSONStringIs(g.value.Raw, want), "%s %q", name, want)
				}
			}
			if depth < 4 && g.value.Type == gjson.JSON {
				check(name+"/"+w.key.String(), g.value, depth+1)
			}
		}
		if r.IsObject() {
			for _, key := range []string{"type", "content", "a", "a\"b", "x", "missing"} {
				w, g := r.Get(key), firstRawJSONMember(r, key)
				require.Equal(t, w.Raw, g.Raw, "%s Get(%s)", name, key)
				require.Equal(t, w.Type, g.Type, "%s Get(%s)", name, key)
			}
		}
	}
	for _, text := range rawMembersParityCorpus {
		check(text, gjson.Parse(text), 0)
		check("view "+text, parseRawJSONView([]byte(text)), 0)
		if r := gjson.Parse(text); r.IsObject() || r.IsArray() {
			check("member "+text, gjson.Parse(`{"m":`+text+`}`).Get("m"), 0)
		}
	}
}

// Reference copies of the rr9 sites before the switch to
// forEachRawJSONMember.
func mayHaveCompactionTriggerGJSON(body []byte) bool {
	found := false
	parseRawJSONView(body).ForEach(func(key, input gjson.Result) bool {
		if key.Str != "input" || !input.IsArray() {
			return true
		}
		input.ForEach(func(_, item gjson.Result) bool {
			if item.IsObject() {
				item.ForEach(func(name, value gjson.Result) bool {
					found = name.Str == "type" && value.Type == gjson.String && value.Str == "compaction_trigger"
					return !found
				})
			}
			return !found
		})
		return !found
	})
	return found
}

func hasDuplicateJSONObjectKeysGJSON(object gjson.Result) bool {
	seen := make(map[string]struct{})
	duplicate := false
	object.ForEach(func(key, _ gjson.Result) bool {
		if _, exists := seen[key.Str]; exists {
			duplicate = true
			return false
		}
		seen[key.Str] = struct{}{}
		return true
	})
	return duplicate
}

func emptyBase64InputImageGJSON(value gjson.Result) bool {
	if !value.Exists() {
		return false
	}
	if value.IsArray() {
		found := false
		value.ForEach(func(_, item gjson.Result) bool {
			if emptyBase64InputImageGJSON(item) {
				found = true
				return false
			}
			return true
		})
		return found
	}
	if value.IsObject() {
		if strings.TrimSpace(value.Get("type").String()) == "input_image" && isEmptyBase64DataURI(value.Get("image_url").String()) {
			return true
		}
		return emptyBase64InputImageGJSON(value.Get("content"))
	}
	return false
}

// TestRawJSONMemberSitesParity (rr9): the switched sites answer as before on
// duplicate keys, escapes, invalid UTF-8 and nesting.
func TestRawJSONMemberSitesParity(t *testing.T) {
	bodies := []string{
		`{"input":[{"type":"compaction_trigger"}]}`,
		`{"input":[{"type":"message","type":"compaction_trigger"}]}`,
		`{"input":[{"t\u0079pe":"compaction\u005ftrigger"}]}`,
		`{"input":[{"type":"compaction_trigger\u0000"}]}`,
		`{"input":"compaction_trigger","input":[{"type":"x"},{"type":"compaction_trigger"}]}`,
		`{"input":[{"role":"user","content":"line \"q\" caf\u00e9\n"}]}`,
		"{\"input\":[{\"role\":\"user\",\"content\":\"\xff\xc3\"},{\"type\":\"input_image\",\"image_url\":\"data:image/png;base64,\"}]}",
		`{"input":[{"role":"user","content":[{"type":"input_image","image_url":"data:image/jpeg;base64,"}]}]}`,
		`{"input":[{"role":"user","content":[{"type":"input_image","image_url":"data:image/jpeg;base64,QUJD"}]}]}`,
		`{"input":[{"role":"user","content":{"type":"input_image","image_url":"data:,"},"content":"x"}]}`,
		`{"input":[{"role":"user","c\u006fntent":[{"type":"input_image","image_url":"data:image/png;base64,"}]}]}`,
		`{"input":[{"a":1,"a":2}],"x":{"y":{"z":[{"k":1,"k":1}]}}}`,
		`{"input":[1,"s",null,[{"type":"compaction_trigger"}]]}`,
	}
	bodies = append(bodies, rawMembersParityCorpus...)
	for _, text := range bodies {
		body := []byte(text)
		require.Equal(t, mayHaveCompactionTriggerGJSON(body), mayHaveCompactionTrigger(body), text)
		require.Equal(t, emptyBase64InputImageGJSON(gjson.GetBytes(body, "input")),
			openAIJSONValueMayContainEmptyBase64InputImage(parseRawJSONView(body).Get("input")), text)
		scratch := make([]string, 0, 32)
		var objects []gjson.Result
		var collect func(r gjson.Result)
		collect = func(r gjson.Result) {
			if r.IsObject() {
				objects = append(objects, r)
			}
			r.ForEach(func(_, v gjson.Result) bool {
				if v.Type == gjson.JSON {
					collect(v)
				}
				return true
			})
		}
		collect(gjson.Parse(text))
		for _, object := range objects {
			want := hasDuplicateJSONObjectKeysGJSON(object)
			require.Equal(t, want, hasDuplicateJSONObjectKeys(object), object.Raw)
			require.Equal(t, want, hasDuplicateJSONObjectKeysIn(object, &scratch), object.Raw)
			// deriveOpenAIContentSessionSeed reads content this way.
			w, g := object.Get("content"), firstRawJSONMember(object, "content")
			require.Equal(t, w.Raw, g.Raw, object.Raw)
			require.Equal(t, w.Exists(), g.Exists(), object.Raw)
		}
	}
}

// TestRawJSONMemberSitesDoNotCopyLargeStrings (rr9): one input item holding
// a 4 MiB escaped string costs the switched sites (almost) nothing.
func TestRawJSONMemberSitesDoNotCopyLargeStrings(t *testing.T) {
	text := strings.Repeat(`line \"quoted\" caf\u00e9 text\n`, 4<<20/30)
	body := []byte(`{"model":"m","instructions":"` + text + `","input":[{"role":"user","content":"` + text + `"}]}`)
	input := parseRawJSONView(body).Get("input")
	item := input.Get("0")
	scratch := make([]string, 0, 32)
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	require.False(t, mayHaveCompactionTrigger(body))
	require.False(t, hasDuplicateJSONObjectKeysIn(item, &scratch))
	require.False(t, hasDuplicateJSONObjectKeysIn(parseRawJSONView(body), &scratch))
	require.False(t, openAIRequestBodyMayContainEmptyBase64InputImage(body))
	require.False(t, storeFalseReplayMayChange(input))
	runtime.ReadMemStats(&after)
	require.Less(t, after.TotalAlloc-before.TotalAlloc, uint64(64<<10))
}
