package bodyspool

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func TestNonceIs128BitAndFresh(t *testing.T) {
	a, err := NewNonce()
	require.NoError(t, err)
	b, err := NewNonce()
	require.NoError(t, err)
	require.Len(t, a.Hex(), 32)
	require.NotEqual(t, a, b)
}

// TestMarkerCollisionFailsClosed: a body that contains the nonce is refused,
// even when the nonce straddles a read boundary.
func TestMarkerCollisionFailsClosed(t *testing.T) {
	fixed := Nonce{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
	old := newNonce
	newNonce = func() (Nonce, error) { return fixed, nil }
	t.Cleanup(func() { newNonce = old })
	d := testDir(t)
	body := []byte(`{"input":"x` + fixed.Hex()[:31] + fixed.Hex() + `"}`)
	f, err := d.Create()
	require.NoError(t, err)
	defer f.Close()
	_, _, err = Ingest(&chunkReader{b: body, rng: newTestRand()}, f, testOpts)
	require.ErrorIs(t, err, ErrMarkerCollision)
}

// TestMangledMarkerFailsClosed: a rewrite that re-escapes, truncates or
// invents a marker makes the splice refuse instead of sending.
func TestMangledMarkerFailsClosed(t *testing.T) {
	d := testDir(t)
	body := []byte(`{"input":[{"image_url":"data:image/png;base64,` + strings.Repeat("A", 64) + `"}]}`)
	sp, skel := ingestBytes(t, d, body, testOpts, nil)
	require.Equal(t, 1, sp.Blobs())
	tag := string(sp.tag)
	for name, bad := range map[string][]byte{
		"embedded":    mustSet(t, skel, "copy", gjson.GetBytes(skel, "input").Raw), // as a string: \\u0000
		"bad_index":   bytes.Replace(skel, []byte(tag+"0"), []byte(tag+"7"), 1),
		"no_open_esc": bytes.Replace(skel, []byte(`\u0000`+tag), []byte(tag), 1),
		"no_close":    bytes.Replace(skel, []byte(tag+`0\u0000`), []byte(tag+`0`), 1),
	} {
		_, _, err := sp.Reader(bad)
		require.ErrorIs(t, err, ErrMarkerUnresolved, name)
	}
	// A rewrite that copies the value as JSON keeps it resolvable.
	dup := mustSetRaw(t, skel, "copy", gjson.GetBytes(skel, "input").Raw)
	legacy := mustSetRaw(t, body, "copy", gjson.GetBytes(body, "input").Raw)
	require.Equal(t, legacy, splice(t, sp, dup))
}

func mustSet(t *testing.T, b []byte, path, v string) []byte {
	out, err := sjson.SetBytes(b, path, v)
	require.NoError(t, err)
	return out
}

func mustSetRaw(t *testing.T, b []byte, path, raw string) []byte {
	out, err := sjson.SetRawBytes(b, path, []byte(raw))
	require.NoError(t, err)
	return out
}

// TestWriteExpandedHashesLikeLegacy: a value decoded from the skeleton (raw
// NUL markers) and its JSON form both expand to the legacy bytes.
func TestWriteExpandedHashesLikeLegacy(t *testing.T) {
	d := testDir(t)
	body := corpus(t)["affinity.json"]
	sp, skel := ingestBytes(t, d, body, testOpts, nil)
	for _, path := range []string{"input.1.content.1.image_url", "input.1.content"} {
		for _, form := range []func(gjson.Result) string{gjson.Result.String, func(r gjson.Result) string { return r.Raw }} {
			want := form(gjson.GetBytes(body, path))
			got := form(gjson.GetBytes(skel, path))
			require.True(t, sp.Contains(got))
			h := sha256.New()
			require.NoError(t, sp.WriteExpanded(h, got))
			require.Equal(t, sha256.Sum256([]byte(want)), [32]byte(h.Sum(nil)), path)
		}
	}
}

// TestInlineLimitAndCharge: S stops a text body by name; Charge runs before
// each step of skeleton growth and its error stops the ingest.
func TestInlineLimitAndCharge(t *testing.T) {
	d := testDir(t)
	text := []byte(`{"input":"` + strings.Repeat("t", 5000) + `"}`)
	f, _ := d.Create()
	_, _, err := Ingest(bytes.NewReader(text), f, Options{InlineLimit: 4096})
	var lim *InlineLimitError
	require.ErrorAs(t, err, &lim)
	require.EqualValues(t, 4096, lim.Limit)
	_ = f.Close()

	var steps int
	stop := errors.New("no memory")
	f, _ = d.Create()
	_, _, err = Ingest(bytes.NewReader(text), f, Options{ChargeStep: 1024, Charge: func(n int64) error {
		require.EqualValues(t, 1024, n)
		if steps++; steps == 3 {
			return stop
		}
		return nil
	}})
	require.ErrorIs(t, err, stop)
	require.Equal(t, 3, steps)
	_ = f.Close()

	// Media does not count: a 64 KiB image under a 4 KiB inline limit passes.
	img := []byte(`{"input":[{"image_url":"data:image/png;base64,` + strings.Repeat("B", 64<<10) + `"}]}`)
	sp, skel := ingestBytes(t, d, img, Options{InlineLimit: 4096}, nil)
	require.Equal(t, 1, sp.Blobs())
	require.Less(t, len(skel), 4096)
}
