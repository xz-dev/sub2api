//go:build unit

package service

import (
	"math"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// minAllocsPerRun is the fewest allocations of one run of fn over 20
// tries: AllocsPerRun counts the whole process, and goroutines left by
// earlier tests of the package allocate too.
func minAllocsPerRun(fn func()) float64 {
	best := math.Inf(1)
	for i := 0; i < 20 && best > 0; i++ {
		best = min(best, testing.AllocsPerRun(1, fn))
	}
	return best
}

// totalAllocOf is the bytes fn allocates (TotalAlloc after a GC on both
// sides, which flushes the per-P caches). Other goroutines of the test
// binary count too; callers assert bounds far above that noise.
func totalAllocOf(fn func()) uint64 {
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	fn()
	runtime.GC()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

func advItemsBody(role, unit string, itemBytes, size int) []byte {
	item := `{"role":"` + role + `","content":"` + strings.Repeat(unit, itemBytes/len(unit)) + `"},`
	var b strings.Builder
	b.WriteString(`{"model":"gpt-5.5","input":[`)
	for b.Len()+len(item) < size {
		b.WriteString(item)
	}
	b.WriteString(`{"role":"user","content":"q"}]}`)
	return []byte(b.String())
}

// TestDerivedKeyBudgetBoundsCanonicalWork (rr10): thousands of system or
// developer items of 4 or 32 KiB of '<' (json.Marshal writes 6 bytes for
// each) cost the seed and the transcript O(derivedKeyBudgetBytes), not
// O(body): the seed's only body-sized cost is the copy of the raw items
// into it (Builder growth), the transcript has none.
func TestDerivedKeyBudgetBoundsCanonicalWork(t *testing.T) {
	for _, tc := range []struct {
		role, unit string
		item       int
	}{
		{"developer", "<", 4 << 10}, {"developer", "<", 32<<10 - 64}, {"system", `\"`, 4 << 10}, {"user", `\u00e9`, 4 << 10},
	} {
		body := advItemsBody(tc.role, tc.unit, tc.item, 16<<20)
		var seed string
		seedAlloc := totalAllocOf(func() { seed = deriveOpenAIContentSessionSeed(body) })
		require.Less(t, seedAlloc, uint64(3*len(seed)+4<<20), "%s %q %d: seed %d B for a %d B seed", tc.role, tc.unit, tc.item, seedAlloc, len(seed))
		keysAlloc := totalAllocOf(func() { _ = deriveOpenAICyberTranscriptBlockKeys(1, body) })
		// 256 lookup keys of 64 hex bytes each, plus the budget's work.
		require.Less(t, keysAlloc, uint64(4<<20), "%s %q %d: transcript %d B", tc.role, tc.unit, tc.item, keysAlloc)
	}
}

// TestDerivedKeyBudgetKeepsSmallKeys: below the budget the seed is the
// legacy canonical one; past it the later fields are raw: and as sent.
func TestDerivedKeyBudgetKeepsSmallKeys(t *testing.T) {
	small := `{"model":"gpt-5.5","instructions":"be \"brief\"","tools":[{"type":"function","name":"f"}],"input":[{"role":"developer","content":"a <b>"},{"role":"user","content":[{"type":"input_text","text":"hi"}]}]}`
	seed := deriveOpenAIContentSessionSeed([]byte(small))
	require.Equal(t, `compat_cs_model=gpt-5.5|tools=[{"name":"f","type":"function"}]|instructions=be "brief"|system="a \u003cb\u003e"|first_user=[{"text":"hi","type":"input_text"}]`, seed)

	body := advItemsBody("developer", "<", 16<<10, 1<<20)
	seed = deriveOpenAIContentSessionSeed(body)
	canonical := strings.Count(seed, `|system="\u003c`)
	raw := strings.Count(seed, `|system=raw:"<`)
	require.Positive(t, canonical)
	require.Positive(t, raw)
	require.LessOrEqual(t, canonical*(16<<10), derivedKeyBudgetBytes, "canonical forms past the budget")
	require.Equal(t, seed, deriveOpenAIContentSessionSeed(body), "stable")
}

// TestDerivedKeyBudgetStringFields (rr10): an escaped instructions or
// input string past contentSeedDecodeMaxBytes is written raw: and as sent,
// without an unescape; an escape-free one is a view, as before.
func TestDerivedKeyBudgetStringFields(t *testing.T) {
	esc := strings.Repeat(`a\"b\n\u00e9`, 1<<20)
	plain := strings.Repeat("abc ", 1<<20)
	for _, tc := range []struct{ name, body, want string }{
		{"instructions escaped", `{"model":"m","instructions":"` + esc + `","input":[{"role":"user","content":"q"}]}`, `|instructions=raw:"` + esc[:64]},
		{"instructions plain", `{"model":"m","instructions":"` + plain + `","input":[{"role":"user","content":"q"}]}`, `|instructions=` + plain[:64]},
		{"input escaped", `{"model":"m","input":"` + esc + `"}`, `|input=raw:"` + esc[:64]},
		{"input plain", `{"model":"m","input":"` + plain + `"}`, `|input=` + plain[:64]},
	} {
		var seed string
		alloc := totalAllocOf(func() { seed = deriveOpenAIContentSessionSeed([]byte(tc.body)) })
		require.Contains(t, seed, tc.want, tc.name)
		require.Less(t, alloc, uint64(3*len(seed)+1<<20), tc.name)
	}
	require.Equal(t, "compat_cs_model=m|instructions=a\"b", deriveOpenAIContentSessionSeed([]byte(`{"model":"m","instructions":"a\"b"}`)))
}

// TestDerivedKeyBudgetSpoolParity: a seed whose budget runs out before a
// field with a spooled string hashes the same from the skeleton as from
// the whole body.
func TestDerivedKeyBudgetSpoolParity(t *testing.T) {
	gin.SetMode(gin.TestMode)
	img := strings.Repeat("QUJD", 40000)
	dev := strings.Repeat(`{"role":"developer","content":"`+strings.Repeat(`x\"<`, 5000)+`"},`, 12)
	body := `{"model":"gpt-5.1","input":[` + dev + `{"role":"developer","content":[{"type":"input_text","text":"a"},{"type":"input_image","image_url":"data:image/png;base64,` + img + `"}]},` +
		`{"role":"developer","content":"tail"},{"role":"user","content":"hi"}]}`
	sp, skel := spoolForTest(t, []byte(body))
	require.Less(t, len(skel), len(body))
	seed := deriveOpenAIContentSessionSeed([]byte(body))
	require.Contains(t, seed, `|system=raw:"x`)
	svc := &OpenAIGatewayService{}
	want := svc.GenerateSessionHash(ginWithSpool(nil, ""), []byte(body))
	require.NotEmpty(t, want)
	require.Equal(t, want, svc.GenerateSessionHash(ginWithSpool(sp, ""), skel))
}

// TestParseJSONNumberBounded (rr10): the same float64 as strconv for any
// JSON number, and no allocation for a long literal.
func TestParseJSONNumberBounded(t *testing.T) {
	long := func(s string, n int) string { return strings.Repeat(s, n) }
	for _, lit := range []string{
		"0", "-0", "1", "123", "1.5", "-2.25e-3", "1e308", "1e309", "-1e309", "4.9e-324", "1e-400",
		"1" + long("0", 40), "1" + long("0", 400), "0." + long("0", 40) + "1", "0." + long("0", 400) + "1",
		"123456789012345678901234567890123456789", "1.7976931348623157e308", "1.7976931348623159e308",
		"9007199254740993" + long("0", 20) + "e-20", "9007199254740993000000000000000000001e-21",
		"2.4703282292062327208828439643411068618252990130716238221279284125033775363510437593264991818081799618989828234772285886546332835517796989819938739800539093906315035659515570226392290858392449105184435931802849936536152500319370457678249219365623669863658480757001585769269903706311928279558551332927834338409351978015531246597263579574622766465272827220056374006485499977096599470454020828166226237857393450736339007967761930577506740176324673600968951340535537458516661134223766678604162159680461914467291840300530057530849048765391711386591646239524912623653881879636239373280423891018672348497668235089863388587925628302755995657524455507255189313690836254779186948667994968324049705821028513185451396213837722826145437693412532098591327667236328125e-324",
		long("9", 50), "-" + long("9", 50) + "e-30", "1e" + long("9", 30), "1e-" + long("9", 30),
	} {
		want, werr := strconv.ParseFloat(lit, 64)
		got, gerr := parseJSONNumberBounded(lit)
		require.Equal(t, math.Float64bits(want), math.Float64bits(got), lit)
		require.Equal(t, werr == nil, gerr == nil, lit)
	}
	for _, bad := range []string{"0x" + long("1", 40), "1." + long("2", 40) + ".3", "Infinity" + long("0", 40), "01" + long("0", 40), "-" + long("a", 40)} {
		got, err := parseJSONNumberBounded(bad)
		require.Error(t, err, bad)
		require.Zero(t, got, bad)
	}
	huge := "1" + long("0", 16<<20)
	var v float64
	require.Zero(t, minAllocsPerRun(func() { v, _ = parseJSONNumberBounded(huge) }))
	require.True(t, math.IsInf(v, 1))
	body := []byte(`{"model":"m","max_output_tokens":` + huge + `,"input":"q"}`)
	require.Zero(t, minAllocsPerRun(func() { v = rawJSONRootMember(body, "max_output_tokens").Num }),
		"Forward's read of max_output_tokens does not allocate")
	require.True(t, math.IsInf(v, 1))
}

// TestRawJSONStringBlank: strings.TrimSpace of the value is "", decided
// without unescaping.
func TestRawJSONStringBlank(t *testing.T) {
	for _, raw := range []string{
		`""`, `" "`, `"\n\t\r "`, `"\u0020\u3000"`, `"\u00a0x"`, `"x"`, `" \u0085"`, `"\ud800"`, `"\ud83d\ude00"`,
		`"\f\b"`, `"\\ "`, `"\/"`, "\"\u2028 \"", `"\u000b"`, `"\u000B"`, `"\uZZZZ"`, `"\u12"`, `"\`, `null`,
	} {
		want := strings.TrimSpace(gjson.Parse(raw).Str) == "" && strings.HasPrefix(raw, `"`) && len(raw) >= 2
		require.Equal(t, want, rawJSONStringBlank(raw), raw)
	}
	big := `"` + strings.Repeat(`\n `, 1<<20) + `"`
	require.True(t, rawJSONStringBlank(big))
	require.Zero(t, minAllocsPerRun(func() { _ = rawJSONStringBlank(big) }))
}

// TestHashOpenAIContentSessionSeedMatchesString: hashing the seed while it
// is written gives deriveOpenAISessionHashesForRequest of the seed string,
// with and without a spool (trailing white space trimmed, markers
// expanded).
func TestHashOpenAIContentSessionSeedMatchesString(t *testing.T) {
	gin.SetMode(gin.TestMode)
	img := strings.Repeat("QUJD", 40000)
	bodies := []string{
		`{"model":"m","input":[{"role":"user","content":"hi"}]}`,
		`{"model":"m","instructions":"trailing   \n","input":"x"}`,
		`{"model":"m","input":"  only space at the end \u00a0\u3000  "}`,
		`{"model":"m","input":[{"type":"input_text","text":"t \t"}]}`,
		`{"model":"m","input":[{"role":"system","content":"s "},{"role":"system"}]}`,
		`{"model":"   "}`,
		`{"input":[]}`,
		string(advItemsBody("developer", "<", 16<<10, 1<<20)),
		`{"model":"gpt-5.1","input":[{"role":"user","content":[{"type":"input_text","text":"a"},{"type":"input_image","image_url":"data:image/png;base64,` + img + `"}]}]}`,
		`{"model":"gpt-5.1","input":[{"role":"developer","content":[{"type":"input_image","image_url":"data:image/png;base64,` + img + `"}]},{"role":"user","content":"hi "}]}`,
	}
	svc := &OpenAIGatewayService{}
	for _, body := range bodies {
		for _, spooled := range []bool{false, true} {
			if spooled && !strings.Contains(body, "base64,") {
				continue // nothing to spool
			}
			c, b := ginWithSpool(nil, ""), []byte(body)
			if spooled {
				sp, skel := spoolForTest(t, b)
				c, b = ginWithSpool(sp, ""), skel
			}
			seed := deriveOpenAIContentSessionSeed(b)
			wantCur, wantLegacy := "", ""
			if seed != "" {
				wantCur, wantLegacy = deriveOpenAISessionHashesForRequest(c, seed)
			}
			cur, legacy, ok := hashOpenAIContentSessionSeed(c, b)
			require.Equal(t, seed != "", ok, body[:min(len(body), 80)])
			require.Equal(t, wantCur, cur, body[:min(len(body), 80)])
			require.Equal(t, wantLegacy, legacy, body[:min(len(body), 80)])
			require.Equal(t, wantCur, svc.GenerateSessionHash(c, b))
		}
	}
}
