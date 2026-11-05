package httputil

import (
	"bufio"
	"compress/gzip"
	"compress/zlib"
	"errors"
	"fmt"
	"io"

	"github.com/klauspost/compress/zstd"
)

// Streaming Content-Encoding decoder for callers that charge the decoded
// bytes as they read them (large-request-capacity, review a4a7d843 blocker 1).
// Unlike ReadRequestBodyWithPrealloc, it never holds the compressed body and
// never expands it in one step: the caller sees decoded bytes only as it
// reads, and its own limit applies to them.

const (
	// MaxRequestZstdWindow is the largest zstd window accepted for a request
	// body. RFC 9659 limits the window of zstd Content-Encoding to 8 MiB.
	MaxRequestZstdWindow = 8 << 20
	// requestZstdMinWindow: the decoder's window limit is at least this, so a
	// later frame of a concatenated body (EncodeAll per chunk) may be a little
	// larger than a small first frame.
	requestZstdMinWindow = 256 << 10
	// requestZstdBlockState bounds the zstd decoder's state beyond its window
	// (block and literal buffers). Measured with one decoder goroutine
	// (allocated bytes over a whole decode, an upper bound of its peak):
	// window 8 MiB 9.13 MiB, 2 MiB 3.13 MiB, 1 MiB 2.13 MiB, a 4 KiB
	// single-segment frame 14 KiB.
	requestZstdBlockState = 1536 << 10
	// requestFlateState bounds gzip and deflate decoder state (32 KiB window,
	// Huffman tables): measured 50 KiB.
	requestFlateState = 64 << 10
	requestPeekBuffer = 4 << 10
	// requestPeekState bounds the peek buffer, its reader and the first
	// frame header parse: measured 7.1 KiB.
	requestPeekState = 8 << 10

	// MaxRequestBodyDecoderState is the largest StateBytes of any decoder.
	MaxRequestBodyDecoderState = MaxRequestZstdWindow + requestZstdBlockState + requestFlateState + requestPeekState
)

// ErrRequestZstdWindow: a zstd request body declares a window above
// MaxRequestZstdWindow, or a later frame needs a larger window than the
// first frame's (the decoder state is charged from the first frame).
var ErrRequestZstdWindow = errors.New("zstd window exceeds the request limit")

// RequestBodyDecoder streams the decoded bytes of a Content-Encoding body.
type RequestBodyDecoder struct {
	r     io.Reader
	close func()
	// StateBytes bounds the decoder's own memory (window and buffers), not
	// counting the decoded bytes the caller keeps.
	StateBytes int64
}

func (d *RequestBodyDecoder) Read(p []byte) (int, error) {
	n, err := d.r.Read(p)
	// A later frame whose window, or single-segment size, is above the
	// first frame's: klauspost reports ErrWindowSizeExceeded or
	// ErrDecoderSizeExceeded (its decoded-size limit, 64 GiB by default, is
	// never reached by a request body). Both are the named window error
	// (review rr3 minor).
	if errors.Is(err, zstd.ErrWindowSizeExceeded) || errors.Is(err, zstd.ErrDecoderSizeExceeded) {
		err = fmt.Errorf("%w: %w", ErrRequestZstdWindow, err)
	}
	return n, err
}

// Close frees the decoder. It does not close the underlying reader.
func (d *RequestBodyDecoder) Close() error {
	if d.close != nil {
		d.close()
		d.close = nil
	}
	return nil
}

// NewRequestBodyDecoder returns a streaming decoder of r for encoding (the
// lower-cased Content-Encoding: zstd, gzip, x-gzip or deflate). For zstd it
// reads the first frame header to size the decoder's state, and limits every
// frame's window to that size.
func NewRequestBodyDecoder(encoding string, r io.Reader) (*RequestBodyDecoder, error) {
	return NewChargedRequestBodyDecoder(encoding, r, func(int64) error { return nil })
}

// NewChargedRequestBodyDecoder is NewRequestBodyDecoder that calls charge
// with the decoder's state bytes before it allocates them (for zstd, the
// peek buffer first, then the rest once the first frame header is read),
// and returns charge's error unchanged. In total it charges StateBytes.
func NewChargedRequestBodyDecoder(encoding string, r io.Reader, charge func(int64) error) (*RequestBodyDecoder, error) {
	switch encoding {
	case "zstd":
		if err := charge(requestPeekState); err != nil {
			return nil, err
		}
		br := bufio.NewReaderSize(r, requestPeekBuffer)
		window, err := firstZstdWindow(br)
		if err != nil {
			return nil, err
		}
		state := window + min(2*window, requestZstdBlockState) + requestFlateState
		if err := charge(state); err != nil {
			return nil, err
		}
		dec, err := zstd.NewReader(br, zstd.WithDecoderConcurrency(1), zstd.WithDecoderLowmem(true), zstd.WithDecoderMaxWindow(uint64(window)))
		if err != nil {
			return nil, err
		}
		return &RequestBodyDecoder{r: dec, close: dec.Close, StateBytes: state + requestPeekState}, nil
	case "gzip", "x-gzip":
		if err := charge(requestFlateState); err != nil {
			return nil, err
		}
		gr, err := gzip.NewReader(r)
		if err != nil {
			return nil, err
		}
		return &RequestBodyDecoder{r: gr, close: func() { _ = gr.Close() }, StateBytes: requestFlateState}, nil
	case "deflate":
		if err := charge(requestFlateState); err != nil {
			return nil, err
		}
		zr, err := zlib.NewReader(r)
		if err != nil {
			return nil, err
		}
		return &RequestBodyDecoder{r: zr, close: func() { _ = zr.Close() }, StateBytes: requestFlateState}, nil
	default:
		return nil, errors.New("unsupported Content-Encoding")
	}
}

// firstZstdWindow is the window the first zstd frame needs, at least
// requestZstdMinWindow. A body that does not start with a readable frame
// header (a skippable frame, or too short) gets the RFC 9659 maximum.
func firstZstdWindow(br *bufio.Reader) (int64, error) {
	head, err := br.Peek(zstd.HeaderMaxSize)
	if err != nil && !errors.Is(err, io.EOF) {
		return 0, err
	}
	var h zstd.Header
	window := int64(MaxRequestZstdWindow)
	if h.Decode(head) == nil && !h.Skippable {
		window = int64(h.WindowSize)
		if h.SingleSegment {
			window = int64(h.FrameContentSize)
		}
	}
	if window > MaxRequestZstdWindow {
		return 0, fmt.Errorf("%w: the first frame needs %d bytes, RFC 9659 allows %d", ErrRequestZstdWindow, window, MaxRequestZstdWindow)
	}
	return max(window, requestZstdMinWindow), nil
}
