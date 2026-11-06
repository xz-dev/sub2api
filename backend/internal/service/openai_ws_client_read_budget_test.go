package service

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/bodyspool"
	coderws "github.com/coder/websocket"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// fakeWSReadBudget charges k x the bytes Cover names into charged.
type fakeWSReadBudget struct {
	mu                sync.Mutex
	limit             int64
	k                 int64
	refuseAbove       int64 // Cover refuses a charge above this, when > 0
	charged, peak     int64
	active            bool
	begins, endsLive  int // endsLive: End calls that released a charge
	coveredMsg, alloc int64
}

func (b *fakeWSReadBudget) Limit() int64 { return b.limit }
func (b *fakeWSReadBudget) Begin() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.begins++
	b.active, b.charged = true, 0
	return nil
}
func (b *fakeWSReadBudget) Cover(n, alloc int64) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.coveredMsg, b.alloc = maxWSReadI64(b.coveredMsg, n), maxWSReadI64(b.alloc, alloc)
	target := maxWSReadI64(b.k*n, alloc)
	if b.refuseAbove > 0 && target > b.refuseAbove {
		return &bodyspool.InsufficientMemoryError{Need: target, Charged: b.charged, Step: target - b.charged}
	}
	b.charged = maxWSReadI64(b.charged, target)
	b.peak = maxWSReadI64(b.peak, b.charged)
	return nil
}
func (b *fakeWSReadBudget) End() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.active {
		b.endsLive++
	}
	b.active, b.charged = false, 0
}

// TestReadOpenAIWSClientMessageCharged: with a read budget a message is
// charged before it is buffered and returned whole; its charge lasts until
// the next read starts. Above the limit, or when the charge is refused, the
// client gets a named error event (code, hop, limit_bytes) and close 1000,
// and the charge is ended.
func TestReadOpenAIWSClientMessageCharged(t *testing.T) {
	for _, tc := range []struct {
		name                string
		refuseAbove         int64
		second              int
		wantCode            string
		wantStatus, wantLim int64
	}{
		{name: "oversize", second: 1<<20 + 1, wantCode: "request_too_large", wantStatus: 413, wantLim: 1 << 20},
		{name: "charge refused", refuseAbove: 1 << 20, second: 900 << 10, wantCode: "insufficient_memory", wantStatus: 503},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := &fakeWSReadBudget{limit: 1 << 20, k: 2, refuseAbove: tc.refuseAbove}
			first := bytes.Repeat([]byte("x"), 300<<10)
			type read struct {
				msg []byte
				err error
			}
			reads := make(chan read, 2)
			next := make(chan struct{}) // the first message's turn is over
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := coderws.Accept(w, r, nil)
				require.NoError(t, err)
				defer func() { _ = conn.CloseNow() }()
				conn.SetReadLimit(b.limit + 1)
				ctx := WithOpenAIWSClientReadBudget(context.Background(), b)
				for i := 0; i < 2; i++ {
					if i == 1 {
						<-next
					}
					_, msg, err := ReadOpenAIWSClientMessage(ctx, conn, 0, coderws.StatusNormalClosure, "")
					reads <- read{msg, err}
					if err != nil {
						return
					}
				}
			}))
			defer server.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			client, _, err := coderws.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
			require.NoError(t, err)
			defer func() { _ = client.CloseNow() }()

			require.NoError(t, client.Write(ctx, coderws.MessageText, first))
			r1 := <-reads
			require.NoError(t, r1.err)
			require.Equal(t, first, r1.msg)
			b.mu.Lock()
			require.True(t, b.active, "the message holds its charge for its turn")
			require.GreaterOrEqual(t, b.charged, 2*int64(len(first)), "k x the message")
			require.GreaterOrEqual(t, b.charged, int64(cap(r1.msg)), "the buffer")
			b.mu.Unlock()
			close(next)

			go func() { _ = client.Write(ctx, coderws.MessageText, bytes.Repeat([]byte("y"), tc.second)) }()
			// The client reads while the server refuses: the server's close
			// handshake waits for the client's close reply.
			var event []byte
			var closeErr error
			clientDone := make(chan struct{})
			go func() {
				defer close(clientDone)
				if _, event, closeErr = client.Read(ctx); closeErr == nil {
					_, _, closeErr = client.Read(ctx)
				}
			}()
			r2 := <-reads
			require.True(t, IsOpenAIWSClientReadRefusal(r2.err), "%v", r2.err)
			<-clientDone
			require.NotEmpty(t, event)
			require.Equal(t, "error", gjson.GetBytes(event, "type").String())
			require.Equal(t, tc.wantCode, gjson.GetBytes(event, "error.code").String(), string(event))
			require.Equal(t, "sub2api", gjson.GetBytes(event, "error.hop").String())
			require.Equal(t, tc.wantStatus, gjson.GetBytes(event, "status").Int())
			if tc.wantLim > 0 {
				require.Equal(t, tc.wantLim, gjson.GetBytes(event, "error.limit_bytes").Int())
			} else {
				require.Greater(t, gjson.GetBytes(event, "error.limit_bytes").Int(), tc.refuseAbove, "the bytes needed")
			}
			require.Equal(t, coderws.StatusNormalClosure, coderws.CloseStatus(closeErr))

			b.mu.Lock()
			defer b.mu.Unlock()
			require.Equal(t, 2, b.begins)
			require.Equal(t, 2, b.endsLive, "the first message's charge ended when the next read started, the failed one's on failure")
			require.False(t, b.active)
			require.LessOrEqual(t, b.coveredMsg, b.limit+1, "never read past limit+1")
		})
	}
}

// maxWSReadI64: a unit test of this package declares an int max.
func maxWSReadI64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
