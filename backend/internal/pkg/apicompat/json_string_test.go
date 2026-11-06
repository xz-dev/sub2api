package apicompat

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestMarshalJSONStringMatchesJSONMarshal: the same bytes as json.Marshal
// (HTML escapes, control characters, invalid UTF-8, U+2028/2029), with
// one allocation of the exact size.
func TestMarshalJSONStringMatchesJSONMarshal(t *testing.T) {
	corpus := []string{"", "plain", `"q" \ /`, "<a href='x'>&amp;</a>", "\x00\x01\x07\b\t\n\v\f\r\x1f\x7f",
		"café 日本 🎉", "\u2028\u2029x\u2027", "bad \xff\xfe utf8 \xc3", "\xed\xa0\x80 surrogate", "ends \xe2\x80",
		strings.Repeat("<", 4096), strings.Repeat("a\"b\\c\nd<é\u2028\xff", 1000)}
	for b := 0; b < 256; b++ {
		corpus = append(corpus, string([]byte{byte(b)}), "x"+string([]byte{byte(b), 'y'}))
	}
	for _, s := range corpus {
		want, err := json.Marshal(s)
		if err != nil {
			t.Fatal(err)
		}
		got := marshalJSONString(s)
		if string(got) != string(want) {
			t.Fatalf("%q: got %s want %s", s, got, want)
		}
		if cap(got) != len(got) {
			t.Fatalf("%q: cap %d len %d", s, cap(got), len(got))
		}
	}
	text := strings.Repeat("<", 1<<20)
	if a := testing.AllocsPerRun(3, func() { _ = marshalJSONString(text) }); a != 1 {
		t.Fatalf("allocs %v", a)
	}
}
