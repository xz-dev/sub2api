//go:build unit

package handler

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime/debug"
	"runtime/metrics"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// newWSReadTestServer serves the native WS ingress with the body spool on.
func newWSReadTestServer(t *testing.T, cfg *config.Config) *httptest.Server {
	t.Helper()
	h := newOpenAIHandlerForPreviousResponseIDValidation(t, nil)
	h.cfg = cfg
	srv := newOpenAIWSHandlerTestServer(t, h, middleware.AuthSubject{UserID: 1, Concurrency: 1})
	t.Cleanup(srv.Close)
	return srv
}

// wsReadOutcome is what the client saw after sending one message.
type wsReadOutcome struct {
	event     string // the first message the server sent
	closeCode coderws.StatusCode
}

// sendWSMessage dials srv, sends msg as one text message and reads until the
// server closes.
func sendWSMessage(t *testing.T, srv *httptest.Server, msg []byte) wsReadOutcome {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, _, err := coderws.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/openai/v1/responses", nil)
	require.NoError(t, err)
	defer func() { _ = conn.CloseNow() }()
	conn.SetReadLimit(1 << 20)
	go func() { _ = conn.Write(ctx, coderws.MessageText, msg) }() // the server stops reading at its limit
	var out wsReadOutcome
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			out.closeCode = coderws.CloseStatus(err)
			return out
		}
		if out.event == "" {
			out.event = string(data)
		}
	}
}

// wsOversizeMessage is a response.create frame of n bytes.
func wsOversizeMessage(n int) []byte {
	head, tail := `{"type":"response.create","model":"gpt-5.5","input":"`, `"}`
	return []byte(head + strings.Repeat("a", n-len(head)-len(tail)) + tail)
}

// retainedHeapBytes is the heap the process keeps from the OS: live and
// dead objects plus free spans not yet returned (heap/released excluded).
func retainedHeapBytes() int64 {
	s := []metrics.Sample{
		{Name: "/memory/classes/heap/objects:bytes"},
		{Name: "/memory/classes/heap/unused:bytes"},
		{Name: "/memory/classes/heap/free:bytes"},
	}
	metrics.Read(s)
	var n int64
	for _, v := range s {
		n += int64(v.Value.Uint64())
	}
	return n
}

// TestOpenAIWSIngressOversizeReadIsChargedAndReleased (exit gate 7.5 finding
// 1): a client message above L at the native WS ingress is charged as it is
// read, refused with a named request_too_large error event (hop sub2api,
// limit_bytes = L) and close 1000, not a bare 1009, and the failed read's
// heap is returned by the event-triggered release: retained heap ends near
// where it started, with no later request and no GC by the test.
func TestOpenAIWSIngressOversizeReadIsChargedAndReleased(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{}
	env := enableBodySpoolForTest(t, cfg, false, 320<<20, nil)
	rt := bodySpoolRuntimeFor(cfg)
	require.NoError(t, rt.err)
	require.NoError(t, os.WriteFile(filepath.Join(env.cgroup, "memory.current"), []byte(strconv.FormatInt(rt.cfg.IdleBaseline, 10)+"\n"), 0o600))
	limit := fallbackLimit(rt.cfg, 320<<20)
	require.EqualValues(t, 11735040, limit, "L at 320m")
	released := make(chan struct{}, 4)
	rt.meter.ReleaseReclaim = func() { debug.FreeOSMemory(); released <- struct{}{} }
	var peakCharge int64
	srv := newWSReadTestServer(t, cfg)
	msg := wsOversizeMessage(32 << 20)

	debug.FreeOSMemory()
	base := retainedHeapBytes()
	stop := make(chan struct{})
	sampled := make(chan struct{})
	go func() {
		defer close(sampled)
		for {
			peakCharge = max(peakCharge, rt.meter.Outstanding())
			select {
			case <-stop:
				return
			case <-time.After(time.Millisecond):
			}
		}
	}()
	out := sendWSMessage(t, srv, msg)
	close(stop)
	<-sampled

	t.Logf("event %s close %d peak charge %d", out.event, out.closeCode, peakCharge)
	require.Equal(t, "error", gjson.Get(out.event, "type").String())
	require.Equal(t, "request_too_large", gjson.Get(out.event, "error.code").String(), out.event)
	require.Equal(t, "sub2api", gjson.Get(out.event, "error.hop").String())
	require.Equal(t, limit, gjson.Get(out.event, "error.limit_bytes").Int())
	require.Equal(t, coderws.StatusNormalClosure, out.closeCode, "close 1000, not a bare 1009")
	require.GreaterOrEqual(t, peakCharge, limit, "the read was charged as it grew")

	select {
	case <-released:
	case <-time.After(10 * time.Second):
		t.Fatal("the failed read did not trigger the heap release")
	}
	require.Eventually(t, func() bool { return rt.meter.Outstanding() == 0 }, 5*time.Second, time.Millisecond)
	msg = nil
	grown := retainedHeapBytes() - base
	t.Logf("retained heap after the failed read: %+d B (message %d B, limit %d B)", grown, 32<<20, limit)
	require.Less(t, grown, int64(4<<20), "the failed read's buffer was dropped and returned")
}
