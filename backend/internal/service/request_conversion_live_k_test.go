//go:build unit

package service

import (
	"bytes"
	"context"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"runtime/metrics"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// drainGeminiUpstream reads the request body and answers a small
// generateContent response.
type drainGeminiUpstream struct{ forwarded atomic.Int64 }

func (u *drainGeminiUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	if req.Body != nil {
		n, _ := io.Copy(io.Discard, req.Body)
		u.forwarded.Add(n)
	}
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}},
		Body: io.NopCloser(strings.NewReader(`{"candidates":[{"content":{"parts":[{"text":"ok"}]}}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":1}}`))}, nil
}

func (u *drainGeminiUpstream) DoWithTLS(req *http.Request, p string, id int64, n int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, p, id, n)
}

// claudeLiveKBody: Claude messages of ~30 bytes (block=false) or of one
// text block each (block=true), up to size bytes.
func claudeLiveKBody(size int, block bool) []byte {
	var b bytes.Buffer
	b.Grow(size + 256)
	b.WriteString(`{"model":"claude-sonnet-4-5","max_tokens":16,"messages":[`)
	for i := 0; b.Len() < size; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		text := `"m` + strconv.Itoa(i) + ` ok"`
		if block {
			text = `[{"type":"text","text":` + text + `}]`
		}
		b.WriteString(`{"role":"` + role + `","content":` + text + `}`)
	}
	b.WriteString(`]}`)
	return b.Bytes()
}

func liveKPeakHeap(fn func()) uint64 {
	runtime.GC()
	s := []metrics.Sample{{Name: "/memory/classes/heap/objects:bytes"}}
	metrics.Read(s)
	base := s[0].Value.Uint64()
	var peak atomic.Uint64
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		ls := []metrics.Sample{{Name: "/memory/classes/heap/objects:bytes"}}
		tk := time.NewTicker(time.Millisecond)
		defer tk.Stop()
		for {
			metrics.Read(ls)
			if v := ls[0].Value.Uint64(); v > peak.Load() {
				peak.Store(v)
			}
			select {
			case <-stop:
				return
			case <-tk.C:
			}
		}
	}()
	fn()
	close(stop)
	wg.Wait()
	return peak.Load() - min(base, peak.Load())
}

// TestRequestConversionLiveK measures k_conv for the Claude -> Gemini
// converter (review rr4 blocker c) in-process, by the method of
// TestBodySpoolConversionLiveK: peak live heap of
// GeminiMessagesCompatService.Forward (convert, send, answer) over a post-GC
// baseline, divided by the body bytes; 3 runs at 2, 4, 8 and 16 MiB. It
// requires k <= k_conv. Set SUB2API_KCONV_LIVE=1 to run it.
func TestRequestConversionLiveK(t *testing.T) {
	if os.Getenv("SUB2API_KCONV_LIVE") == "" {
		t.Skip("set SUB2API_KCONV_LIVE=1 to measure the Claude -> Gemini converter's k")
	}
	gin.SetMode(gin.TestMode)
	account := &Account{ID: 1, Type: AccountTypeAPIKey, Credentials: map[string]any{"api_key": "k"}}
	for _, block := range []bool{false, true} {
		for _, mib := range []int{2, 4, 8, 16} {
			body := claudeLiveKBody(mib<<20, block)
			worst := 0.0
			for run := 1; run <= 3; run++ {
				up := &drainGeminiUpstream{}
				svc := &GeminiMessagesCompatService{httpUpstream: up, cfg: &config.Config{}}
				c, _ := gin.CreateTestContext(httptest.NewRecorder())
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
				var err error
				d := liveKPeakHeap(func() { _, err = svc.Forward(context.Background(), c, account, body) })
				require.NoError(t, err)
				require.Positive(t, up.forwarded.Load())
				k := float64(d) / float64(len(body))
				worst = math.Max(worst, k)
			}
			t.Logf("KCONV claude->gemini block=%v %d MiB worst %.2f (k_conv %.1f)", block, mib, worst, config.DefaultBodySpoolKConv)
			assert.LessOrEqual(t, worst, config.DefaultBodySpoolKConv)
		}
	}
}
