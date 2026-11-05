//go:build !race

package securityaudit

import (
	"bytes"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// rawResponsesBody is a Responses body of message items whose texts are
// written as they are, invalid UTF-8 included (json.Marshal would replace
// it). Texts must not need escapes.
func rawResponsesBody(parts ...[2]string) []byte {
	var b bytes.Buffer
	b.WriteString(`{"model":"m","input":[`)
	for i, p := range parts {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`{"type":"message","role":"` + p[0] + `","content":[{"type":"input_text","text":"`)
		b.WriteString(p[1])
		b.WriteString(`"}]}`)
	}
	b.WriteString(`]}`)
	return b.Bytes()
}

// invalidUTF8Shapes are guard bodies with invalid UTF-8 text (review rr3
// blocker 1): one message, two, many small ones, and text with one invalid
// byte in eight.
func invalidUTF8Shapes(n int) map[string][]byte {
	bad := strings.Repeat("\xff", n)
	mixed := strings.Repeat("abc\xfedefg", n/8)
	small := make([][2]string, 0, n/40)
	for len(small) < cap(small) {
		i := strconv.Itoa(len(small))
		small = append(small, [2]string{"user", i + "\xff\xfe\xfd\xfc"}, [2]string{"assistant", i + "\xc3"})
	}
	return map[string][]byte{
		"one-message":   rawResponsesBody([2]string{"user", bad}),
		"two-messages":  rawResponsesBody([2]string{"assistant", bad[:n/2]}, [2]string{"user", bad[:n/2]}),
		"mixed-1-in-8":  rawResponsesBody([2]string{"user", mixed}, [2]string{"assistant", mixed[:n/4]}),
		"small-invalid": rawResponsesBody(small...),
	}
}

// allocatedBy is the bytes f allocates with GC off.
func allocatedBy(f func()) float64 {
	old := debug.SetGCPercent(-1)
	defer debug.SetGCPercent(old)
	runtime.GC()
	var a, b runtime.MemStats
	runtime.ReadMemStats(&a)
	f()
	runtime.ReadMemStats(&b)
	return float64(b.TotalAlloc - a.TotalAlloc)
}

// TestPromptSnapshotInvalidUTF8CostBounded: each invalid byte the guard
// keeps decodes to U+FFFD (3 bytes), so the kept text can be 3 × its bytes
// in the body. The guard's work stays within its charge: guardDecodeK (3.5)
// × body + InvalidUTF8ChargeK × the invalid bytes InvalidUTF8Bytes counts
// in the body + 512 KiB, async and blocking (chunks included).
func TestPromptSnapshotInvalidUTF8CostBounded(t *testing.T) {
	for name, body := range invalidUTF8Shapes(2 << 20) {
		req := Request{Protocol: "openai_responses", Body: body}
		invalid := InvalidUTF8Bytes(body)
		charge := 3.5*float64(len(body)) + float64(InvalidUTF8ChargeK*invalid) + 512<<10
		var snap PromptSnapshot
		var err error
		async := allocatedBy(func() { snap, err = ExtractPromptSnapshot(req) })
		require.NoError(t, err, name)
		var chunks []string
		blocking := allocatedBy(func() {
			s, _ := ExtractBlockingPromptSnapshot(req, false)
			chunks = SplitRunes(s.ScanText, DefaultInputLimit)
		})
		t.Logf("%-14s body %8d B, invalid %8d B, scan %8d B, %4d chunks: allocated/body async %.2f, blocking %.2f; /charge async %.2f, blocking %.2f",
			name, len(body), invalid, len(snap.ScanText), len(chunks), async/float64(len(body)), blocking/float64(len(body)), async/charge, blocking/charge)
		require.LessOrEqual(t, async, charge, name)
		require.LessOrEqual(t, blocking, charge, name)
	}
}
