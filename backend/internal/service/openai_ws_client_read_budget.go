package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/Wei-Shaw/sub2api/internal/pkg/bodyspool"
	coderws "github.com/coder/websocket"
)

// OpenAIWSClientReadBudget bounds and charges the messages one client
// WebSocket connection sends to the native WS ingress (large-request-capacity
// D2/D4, exit gate 7.5 finding 1). Without it a client message was read
// whole by the library, uncharged, up to the 64 MiB read limit, and then
// closed with a bare 1009; the read's heap stayed in the hop and no charge
// or release accounted for it.
//
// A message is charged before its bytes are buffered and holds its charge
// for its turn: the charge ends when the next read starts, when a read fails
// (after its buffer is dropped) and when the connection ends. Reads on one
// connection are sequential.
type OpenAIWSClientReadBudget interface {
	// Limit is the largest message in bytes. A larger one gets a named
	// request_too_large error event and close 1000.
	Limit() int64
	// Begin starts the charge of a message whose first frame has arrived,
	// before any of its payload is buffered.
	Begin() error
	// Cover makes the current message's charge cover a message of n bytes
	// held in buffers of alloc bytes in total.
	Cover(n, alloc int64) error
	// End returns the current message's charge and, when it was large,
	// triggers the hop's heap release. The caller has dropped the message's
	// buffers. A no-op without a current charge.
	End()
}

type openAIWSClientReadBudgetKey struct{}

// WithOpenAIWSClientReadBudget makes every client WS read under ctx bounded
// and charged by b.
func WithOpenAIWSClientReadBudget(ctx context.Context, b OpenAIWSClientReadBudget) context.Context {
	return context.WithValue(ctx, openAIWSClientReadBudgetKey{}, b)
}

func openAIWSClientReadBudgetFrom(ctx context.Context) OpenAIWSClientReadBudget {
	if ctx == nil {
		return nil
	}
	b, _ := ctx.Value(openAIWSClientReadBudgetKey{}).(OpenAIWSClientReadBudget)
	return b
}

// OpenAIWSClientMessageTooLargeError: a client message above the limit.
type OpenAIWSClientMessageTooLargeError struct{ Limit int64 }

func (e *OpenAIWSClientMessageTooLargeError) Error() string {
	return fmt.Sprintf("sub2api: the websocket message exceeds the %d-byte limit", e.Limit)
}

// openAIWSClientReadChunk bounds one Read, so the charge runs at most this
// far ahead of the bytes read.
const openAIWSClientReadChunk = 32 << 10

// readOpenAIWSClientMessageCharged reads one message, charging it through b
// before each piece is buffered, and stops at Limit()+1 bytes. Every failure
// drops the buffer, then ends the charge (which triggers the heap release
// for a large one). The caller sets the library read limit above Limit(), so
// the library never closes the connection with 1009 itself.
func readOpenAIWSClientMessageCharged(conn *coderws.Conn, b OpenAIWSClientReadBudget) (coderws.MessageType, []byte, error) {
	b.End() // the previous message's turn is over
	typ, r, err := conn.Reader(context.Background())
	if err != nil {
		return 0, nil, err
	}
	if err := b.Begin(); err != nil {
		return 0, nil, err
	}
	limit := b.Limit()
	var buf []byte
	fail := func(err error) (coderws.MessageType, []byte, error) {
		buf = nil
		b.End()
		return 0, nil, err
	}
	for {
		if int64(len(buf)) > limit {
			return fail(&OpenAIWSClientMessageTooLargeError{Limit: limit})
		}
		want := min(int64(len(buf))+openAIWSClientReadChunk, limit+1)
		have, grow := int64(cap(buf)), int64(0)
		if want > have {
			// Doubling, at least one chunk, at most limit+1. (No builtin max
			// here: a unit test of this package declares an int max.)
			grow = 2 * have
			if grow < want {
				grow = want
			}
			if grow > limit+1 {
				grow = limit + 1
			}
		}
		// The grown buffer plus every earlier one (they halve, so less than
		// 2 x the current), which stay in the heap until the GC sweeps them.
		if err := b.Cover(want, grow+2*have); err != nil {
			return fail(err)
		}
		if grow > 0 {
			grown := make([]byte, len(buf), grow)
			copy(grown, buf)
			buf = grown
		}
		n, err := r.Read(buf[len(buf):want])
		buf = buf[:len(buf)+n]
		if errors.Is(err, io.EOF) {
			return typ, buf, nil
		}
		if err != nil {
			return fail(err)
		}
	}
}

// openAIWSClientReadRefusal is the named error a refused client read gets.
type openAIWSClientReadRefusal struct {
	status     int
	code, msg  string
	limitBytes int64
}

func openAIWSClientReadRefusalFor(err error) *openAIWSClientReadRefusal {
	var tooLarge *OpenAIWSClientMessageTooLargeError
	var mem *bodyspool.InsufficientMemoryError
	switch {
	case errors.As(err, &tooLarge):
		return &openAIWSClientReadRefusal{http.StatusRequestEntityTooLarge, "request_too_large", tooLarge.Error(), tooLarge.Limit}
	case errors.As(err, &mem):
		return &openAIWSClientReadRefusal{http.StatusServiceUnavailable, bodyspool.CodeInsufficientMemory, "sub2api: " + mem.Error(), mem.Need}
	}
	return nil
}

// IsOpenAIWSClientReadRefusal reports whether err is a refused client read
// whose error event and close 1000 were already sent.
func IsOpenAIWSClientReadRefusal(err error) bool { return openAIWSClientReadRefusalFor(err) != nil }

// event is the Responses error event: code, hop and limit_bytes, as the
// HTTP refusals carry them.
func (r *openAIWSClientReadRefusal) event() []byte {
	typ := "server_error"
	if r.status < http.StatusInternalServerError {
		typ = "invalid_request_error"
	}
	body, _ := json.Marshal(map[string]any{
		"type":   "error",
		"status": r.status,
		"error": map[string]any{
			"type": typ, "code": r.code, "hop": "sub2api", "limit_bytes": r.limitBytes, "message": r.msg,
		},
	})
	return body
}

// send writes the error event, then closes with 1000 (the design's native
// WS rule: a named error event and a normal close, never a bare 1009). It
// returns the close error the readers hand on.
func (r *openAIWSClientReadRefusal) send(ctx context.Context, conn *coderws.Conn, cause error) error {
	_ = conn.Write(ctx, coderws.MessageText, r.event())
	_ = conn.Close(coderws.StatusNormalClosure, r.code)
	_ = conn.CloseNow()
	return NewOpenAIWSClientCloseError(coderws.StatusNormalClosure, r.code, cause)
}
