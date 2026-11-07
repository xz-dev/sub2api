//go:build unit

package service

import (
	"math/rand"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// rawIDCorpus: JSON string literals (quotes included) for the in-place id
// checks: plain, escaped, whitespace-led, surrogates, malformed escapes,
// control bytes, invalid UTF-8, and random mixes of all of them.
func rawIDCorpus() []string {
	lits := []string{
		`""`, `"rs_1"`, `"rs"`, `"rs_"`, `"r"`, `"msg_abc"`, `"fc_1"`, `"x"`,
		`"\u0072s_1"`, `"\u0072\u0073\u005f"`, `"r\u0073_"`, `"\nrs_1"`, `" \t rs_1"`, `"\u00a0rs_1"`, `"\u3000rs_x"`,
		`"\u2028\u2029rs_"`, `"\u0085rs_"`, "\"\xc2\xa0rs_\"", "\"\xe3\x80\x80rs_\"", "\"\xe2rs_\"", "\"\xffrs_\"",
		`"\ud83d\ude00rs_"`, `"\ud83drs_"`, `"\ude00rs_"`, `"\ud83d\u0041rs_"`, `"rs_\ud83d"`,
		`"rs\_1"`, `"rs_\x"`, `"\u12"`, `"rs_\u12"`, `"\uzzzzrs_"`, `"rs_\uZZZZ"`, `"\"rs_"`, `"\\rs_"`, `"\/rs_"`,
		"\"rs_\x01tail\"", "\"\x01rs_\"", `"rs_` + strings.Repeat(`\n`, 40) + `"`, `"` + strings.Repeat(`\t`, 100) + `rs_x"`,
		`"msg_` + strings.Repeat("a", 60) + `"`, `"msg_` + strings.Repeat("a", 61) + `"`, `"m\u0073g_` + strings.Repeat("a", 60) + `"`,
		`"fc_` + strings.Repeat(`\u00e9`, 30) + `"`, `"fc_` + strings.Repeat(`\u00e9`, 31) + `"`, `"\u0000"`, `"rs_\u0000"`,
		`"\b\f\r rs_"`, `"\u000brs_"`, `"\u001crs_"`,
	}
	units := []string{"r", "s", "_", "m", "g", "f", "c", "t", "w", "a", " ", `\t`, `\n`, `\u0020`, `\u00a0`, `\u0072`, `\u0073`, `\u005f`,
		`\ud83d`, `\ude00`, `\u12`, `\q`, `\"`, `\\`, "\xc2\xa0", "\xe2", "\xff", "\x01", `\u3000`, `\uzz00`}
	rng := rand.New(rand.NewSource(11))
	for i := 0; i < 20000; i++ {
		var b strings.Builder
		b.WriteByte('"')
		for j, n := 0, rng.Intn(12); j < n; j++ {
			b.WriteString(units[rng.Intn(len(units))])
		}
		if rng.Intn(3) == 0 {
			b.WriteString(strings.Repeat("a", rng.Intn(70)))
		}
		b.WriteByte('"')
		lits = append(lits, b.String())
	}
	return lits
}

func TestRawJSONStringDecodedBytesMatchesGJSON(t *testing.T) {
	for _, lit := range rawIDCorpus() {
		var got []byte
		rawJSONStringDecodedBytes(lit, func(b byte) bool { got = append(got, b); return true })
		require.Equal(t, gjson.Parse(lit).Str, string(got), "%q", lit)
	}
}

// The in-place id checks give the decisions of the decoded checks they
// replace (store=false replay, input-ID sanitizer), on every value type.
func TestRawItemIDChecksMatchDecoded(t *testing.T) {
	lits := append(rawIDCorpus(), `123`, `-1`, `true`, `false`, `null`, `{"a":"rs_"}`, `["rs_"]`)
	types := []string{"message", "reasoning", "web_search_call", "function_call", "custom_tool_call", "tool_search_call", "custom_tool_call_output", "item_reference", "other"}
	for _, lit := range lits {
		item := gjson.Parse(`{"type":"x","id":` + lit + `}`)
		if !item.IsObject() {
			continue
		}
		got := firstRawJSONMember(item, "id")
		want := item.Get("id")
		require.Equal(t, strings.HasPrefix(strings.TrimSpace(want.String()), "rs_"), rawItemIDTrimmedHasPrefix(got, "rs_"), "%q", lit)
		if want.Type != gjson.String {
			continue
		}
		for _, typ := range types {
			require.Equal(t, shouldStripOpenAIResponsesInputItemID(typ, want.String()), shouldStripOpenAIResponsesInputItemIDRaw(typ, got), "%s %q", typ, lit)
		}
	}
}

// The checks allocate nothing, also for a long escaped id.
func TestRawItemIDChecksAllocateNothing(t *testing.T) {
	id := gjson.Parse(`{"id":"x1_\n` + strings.Repeat("a", 4090) + `"}`)
	v := firstRawJSONMember(id, "id")
	ws := firstRawJSONMember(gjson.Parse(`{"id":"`+strings.Repeat(`\t`, 2000)+`rs_1"}`), "id")
	allocs := testing.AllocsPerRun(100, func() {
		_ = rawItemIDTrimmedHasPrefix(v, "rs_")
		_ = rawItemIDTrimmedHasPrefix(ws, "rs_")
		_ = shouldStripOpenAIResponsesInputItemIDRaw("reasoning", v)
		_ = shouldStripOpenAIResponsesInputItemIDRaw("message", ws)
		_ = firstRawJSONMember(id, "id").Exists()
	})
	require.Zero(t, allocs)
}
