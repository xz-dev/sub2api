package bodyspool

import (
	"bytes"
	"encoding/json"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Tests use a tiny T so that the small corpus bodies exercise spooling.
var testOpts = Options{Threshold: 16, KeepPrefix: 4}

func testDir(t *testing.T) *Dir {
	t.Helper()
	p := filepath.Join(t.TempDir(), "spool")
	require.NoError(t, os.Mkdir(p, 0o700))
	d, err := OpenDir(p, 1)
	require.NoError(t, err)
	return d
}

func ingestBytes(t *testing.T, d *Dir, body []byte, opt Options, r io.Reader) (*Spool, []byte) {
	t.Helper()
	f, err := d.Create()
	require.NoError(t, err)
	if r == nil {
		r = bytes.NewReader(body)
	}
	sp, skel, err := Ingest(r, f, opt)
	require.NoError(t, err)
	t.Cleanup(func() { _ = sp.Close() })
	return sp, skel
}

func splice(t *testing.T, sp *Spool, skel []byte) []byte {
	t.Helper()
	r, n, err := sp.Reader(skel)
	require.NoError(t, err)
	out, err := io.ReadAll(r)
	require.NoError(t, err)
	require.Equal(t, int64(len(out)), n, "Content-Length must equal the spliced bytes")
	return out
}

// goRoundTrip is the encoding/json decode+marshal rewrite the fork runs.
func goRoundTrip(t *testing.T, b []byte) []byte {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var v any
	require.NoError(t, dec.Decode(&v))
	out, err := json.Marshal(v)
	require.NoError(t, err)
	return out
}

func corpus(t *testing.T) map[string][]byte {
	t.Helper()
	files, err := filepath.Glob("testdata/corpus/*.json")
	require.NoError(t, err)
	require.NotEmpty(t, files)
	out := map[string][]byte{}
	for _, f := range files {
		b, err := os.ReadFile(f)
		require.NoError(t, err)
		out[filepath.Base(f)] = bytes.TrimRight(b, "\n")
	}
	return out
}

// TestCorpusDifferential: legacy(body) == splice(transform(skeleton(body)))
// for raw, sjson and encoding/json rewrites, with the same accept decision.
func TestCorpusDifferential(t *testing.T) {
	d := testDir(t)
	spooled := 0
	for name, body := range corpus(t) {
		t.Run(name, func(t *testing.T) {
			sp, skel := ingestBytes(t, d, body, testOpts, nil)
			spooled += sp.Blobs()
			require.Equal(t, gjson.ValidBytes(body), gjson.ValidBytes(skel))
			require.True(t, gjson.ValidBytes(skel))
			require.Equal(t, body, splice(t, sp, skel), "raw splice")

			setLegacy, err := sjson.SetBytes(body, "model", "gpt-x")
			require.NoError(t, err)
			setSkel, err := sjson.SetBytes(skel, "model", "gpt-x")
			require.NoError(t, err)
			require.Equal(t, setLegacy, splice(t, sp, setSkel), "sjson splice")

			require.Equal(t, goRoundTrip(t, body), splice(t, sp, goRoundTrip(t, skel)), "encoding/json splice")
		})
	}
	require.Greater(t, spooled, 5, "the corpus must exercise spooling")
}

// TestCorpusTierA pins what is externalized: canonical tier-A values only.
func TestCorpusTierA(t *testing.T) {
	d := testDir(t)
	c := corpus(t)
	for name, want := range map[string]int{
		"escapes.json":   1, // \/ and < & stay inline
		"unicode.json":   1, // only the canonical file_data
		"dupkeys.json":   2,
		"numbers.json":   1, // image_url.url
		"deep.json":      1, // the depth-80 image stays inline
		"dataurl.json":   4, // source.data, both data: URL values, top-level image_url
		"affinity.json":  1, // encrypted_content and signature stay inline
		"largetext.json": 0, // large text is never spooled
	} {
		sp, skel := ingestBytes(t, d, c[name], testOpts, nil)
		require.Equal(t, want, sp.Blobs(), name+": "+string(skel))
	}
}

// TestRejectionParity: invalid bodies are rejected the same way and never
// become valid through spooling.
func TestRejectionParity(t *testing.T) {
	d := testDir(t)
	img := `"image_url":"data:image/png;base64,` + strings.Repeat("Q", 40)
	for name, body := range map[string]string{
		"unterminated_blob": `{"input":[{` + img,
		"bad_escape":        `{"input":[{` + img + `\x"}]}`,
		"control_char":      `{"input":[{` + img + "\x01" + `"}]}`,
		"invalid_utf8":      `{"input":[{` + img + "\xff\xfe" + `"}]}`,
		"trailing_comma":    `{"input":[{` + img + `"},]}`,
		"missing_brace":     `{"input":[{` + img + `"}]`,
		"unterminated_text": `{"input":"` + strings.Repeat("A", 40) + `"`,
	} {
		sp, skel := ingestBytes(t, d, []byte(body), testOpts, nil)
		require.Equal(t, gjson.ValidBytes([]byte(body)), gjson.ValidBytes(skel), name)
		if gjson.ValidBytes(skel) { // gjson accepts invalid UTF-8: same bytes must come back
			require.Equal(t, []byte(body), splice(t, sp, skel), name)
		}
	}
}

// TestChunkSplitFuzz: random read boundaries give the same skeleton shape
// and a byte-identical splice.
func TestChunkSplitFuzz(t *testing.T) {
	d := testDir(t)
	rng := rand.New(rand.NewSource(1))
	for name, body := range corpus(t) {
		_, want := ingestBytes(t, d, body, testOpts, nil)
		for i := 0; i < 20; i++ {
			sp, skel := ingestBytes(t, d, body, testOpts, &chunkReader{b: body, rng: rng})
			require.Equal(t, len(want), len(skel), name)
			require.Equal(t, body, splice(t, sp, skel), name)
		}
	}
}

type chunkReader struct {
	b   []byte
	rng *rand.Rand
}

func (r *chunkReader) Read(p []byte) (int, error) {
	if len(r.b) == 0 {
		return 0, io.EOF
	}
	n := 1 + r.rng.Intn(7)
	n = min(n, len(p), len(r.b))
	copy(p, r.b[:n])
	r.b = r.b[n:]
	return n, nil
}

func newTestRand() *rand.Rand { return rand.New(rand.NewSource(7)) }
