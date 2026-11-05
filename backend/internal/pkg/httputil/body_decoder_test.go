package httputil

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"io"
	"runtime"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/require"
)

func zstdFrame(t *testing.T, plain []byte, window int) []byte {
	t.Helper()
	var b bytes.Buffer
	w, err := zstd.NewWriter(&b, zstd.WithWindowSize(window))
	require.NoError(t, err)
	_, _ = w.Write(plain)
	require.NoError(t, w.Close())
	return b.Bytes()
}

// TestRequestBodyDecoderStateBound: StateBytes bounds every byte the decoder
// allocates over a whole decode (an upper bound of its peak), for each window
// RFC 9659 allows and for a small single-segment frame.
func TestRequestBodyDecoderStateBound(t *testing.T) {
	raw := make([]byte, 6<<20)
	_, _ = rand.Read(raw)
	plain := []byte(base64.StdEncoding.EncodeToString(raw))
	var gz bytes.Buffer
	gw := gzip.NewWriter(&gz)
	_, _ = gw.Write(plain)
	require.NoError(t, gw.Close())
	cases := map[string]struct {
		enc  string
		body []byte
	}{
		"zstd-8MiB":    {"zstd", zstdFrame(t, plain, 8<<20)},
		"zstd-2MiB":    {"zstd", zstdFrame(t, plain, 2<<20)},
		"zstd-1MiB":    {"zstd", zstdFrame(t, plain, 1<<20)},
		"zstd-4KiB-ss": {"zstd", zstdFrame(t, plain[:4<<10], 8<<20)},
		"gzip":         {"gzip", gz.Bytes()},
	}
	buf := make([]byte, 32<<10)
	for name, tc := range cases {
		runtime.GC()
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		d, err := NewRequestBodyDecoder(tc.enc, bytes.NewReader(tc.body))
		require.NoError(t, err, name)
		_, err = io.CopyBuffer(io.Discard, struct{ io.Reader }{d}, buf)
		require.NoError(t, err, name)
		_ = d.Close()
		runtime.ReadMemStats(&after)
		alloc := int64(after.TotalAlloc - before.TotalAlloc)
		t.Logf("%s: allocated %d KiB, StateBytes %d KiB", name, alloc>>10, d.StateBytes>>10)
		require.LessOrEqual(t, alloc, d.StateBytes, name)
	}
}

// TestChargedRequestBodyDecoderChargesFirst (review rr3 nit): the state is
// charged before the decoder allocates it, StateBytes in total, and a
// refused charge allocates no decoder and returns the charge's error.
func TestChargedRequestBodyDecoderChargesFirst(t *testing.T) {
	plain := bytes.Repeat([]byte("0123456789abcdef"), 1<<16)
	var gz, zl bytes.Buffer
	gw := gzip.NewWriter(&gz)
	_, _ = gw.Write(plain)
	require.NoError(t, gw.Close())
	zw := zlib.NewWriter(&zl)
	_, _ = zw.Write(plain)
	require.NoError(t, zw.Close())
	refused := errors.New("refused")
	for enc, body := range map[string][]byte{"gzip": gz.Bytes(), "deflate": zl.Bytes(), "zstd": zstdFrame(t, plain, 1<<20)} {
		var ms runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&ms)
		start := ms.TotalAlloc
		var charged int64
		d, err := NewChargedRequestBodyDecoder(enc, bytes.NewReader(body), func(n int64) error {
			// At each charge, what is allocated so far was charged already.
			runtime.ReadMemStats(&ms)
			require.LessOrEqual(t, int64(ms.TotalAlloc-start), charged+1024, "%s: allocated before its charge", enc)
			charged += n
			return nil
		})
		require.NoError(t, err, enc)
		require.Equal(t, d.StateBytes, charged, enc)
		got, err := io.ReadAll(d)
		require.NoError(t, err, enc)
		require.Equal(t, plain, got, enc)
		_ = d.Close()

		runtime.GC()
		runtime.ReadMemStats(&ms)
		start = ms.TotalAlloc
		d, err = NewChargedRequestBodyDecoder(enc, bytes.NewReader(body), func(int64) error { return refused })
		runtime.ReadMemStats(&ms)
		require.ErrorIs(t, err, refused, enc)
		require.Nil(t, d, enc)
		require.Less(t, ms.TotalAlloc-start, uint64(1024), "%s: a refused charge allocates no decoder", enc)
	}
}

// TestRequestBodyDecoderZstdWindow: a first frame above RFC 9659's 8 MiB is
// refused before any decoder state exists; a later frame may not need a
// larger window than the one charged from the first frame.
func TestRequestBodyDecoderZstdWindow(t *testing.T) {
	plain := bytes.Repeat([]byte("0123456789abcdef"), 1<<20) // 16 MiB
	_, err := NewRequestBodyDecoder("zstd", bytes.NewReader(zstdFrame(t, plain, 16<<20)))
	require.ErrorIs(t, err, ErrRequestZstdWindow)

	small := zstdFrame(t, plain[:1<<10], 8<<20) // single segment, 1 KiB
	body := append(append([]byte(nil), small...), zstdFrame(t, plain, 8<<20)...)
	d, err := NewRequestBodyDecoder("zstd", bytes.NewReader(body))
	require.NoError(t, err)
	_, err = io.Copy(io.Discard, d)
	require.True(t, errors.Is(err, ErrRequestZstdWindow), "got %v", err)

	// A later single-segment frame (EncodeAll writes its content size, no
	// window) larger than the charged window: the same named error, not
	// klauspost's "decompressed size exceeds configured limit" (review rr3).
	enc, err := zstd.NewWriter(nil, zstd.WithEncoderConcurrency(1))
	require.NoError(t, err)
	later := enc.EncodeAll(plain[:4<<20], nil)
	var h zstd.Header
	require.NoError(t, h.Decode(later))
	require.True(t, h.SingleSegment)
	body = append(enc.EncodeAll(plain[:1<<10], nil), later...)
	d, err = NewRequestBodyDecoder("zstd", bytes.NewReader(body))
	require.NoError(t, err)
	_, err = io.Copy(io.Discard, d)
	require.True(t, errors.Is(err, ErrRequestZstdWindow), "got %v", err)

	// Two frames within the charged window decode in full.
	body = append(zstdFrame(t, plain[:1<<20], 1<<20), zstdFrame(t, plain[:1<<20], 1<<20)...)
	d, err = NewRequestBodyDecoder("zstd", bytes.NewReader(body))
	require.NoError(t, err)
	got, err := io.ReadAll(d)
	require.NoError(t, err)
	require.Len(t, got, 2<<20)
}
