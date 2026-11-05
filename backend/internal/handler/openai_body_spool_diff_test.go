//go:build unit

package handler

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// Differential test (task 7.2): with the spool on, the upstream receives the
// same bytes, Content-Length and headers as the legacy path, and the session
// hash routes to the same account.

type captureUpstream struct {
	service.HTTPUpstream
	mu       sync.Mutex
	bodies   [][]byte
	lengths  []int64
	headers  []http.Header
	accounts []int64
	urls     []string
}

func (u *captureUpstream) Do(req *http.Request, _ string, accountID int64, _ int) (*http.Response, error) {
	b, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	u.mu.Lock()
	u.bodies = append(u.bodies, b)
	u.lengths = append(u.lengths, req.ContentLength)
	u.headers = append(u.headers, req.Header.Clone())
	u.accounts = append(u.accounts, accountID)
	u.urls = append(u.urls, req.URL.String())
	u.mu.Unlock()
	if strings.HasSuffix(req.URL.Path, "/chat/completions") {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(bytes.NewBufferString(`{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"gpt-5.1",` +
				`"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],` +
				`"usage":{"prompt_tokens":10,"completion_tokens":1,"total_tokens":11}}`)),
		}, nil
	}
	return (&largeBodyDrainUpstream{}).Do(&http.Request{}, "", 0, 0)
}

func diffBodies() map[string][]byte {
	img := strings.Repeat("iVBORw0KGgo", 20000) // 220 KB canonical base64
	return map[string][]byte{
		"first_user_image": []byte(`{"model":"gpt-5.1","stream":false,"instructions":"be brief","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"what is this <b>&amp;</b>?"},{"type":"input_image","image_url":"data:image/png;base64,` + img + `"}]}]}`),
		"file_and_url":     []byte(`{"model":"gpt-5.1","stream":false,"input":[{"role":"user","content":[{"type":"input_file","filename":"a.pdf","file_data":"data:application/pdf;base64,` + img + `"},{"type":"input_image","image_url":{"url":"data:image/jpeg;base64,` + img + `"}}]}],"metadata":{"k":"v"}}`),
		"escaped_media":    []byte(`{"model":"gpt-5.1","stream":false,"input":[{"role":"user","content":[{"type":"input_image","image_url":"data:image/png;base64,` + strings.Replace(img, "w0", `w\/0`, 3) + `"}]}]}`),
		"many_images":      largeResponsesImagesBody(3 << 20),
	}
}

func runCapture(t *testing.T, accountType string, body []byte, spool bool) *captureUpstream {
	t.Helper()
	upstream := &captureUpstream{}
	h := newLargeBodyTestHandler(t, upstream, accountType)
	if spool {
		enableBodySpoolForTest(t, h.cfg, true, 8<<30, nil)
	}
	c, rec := newLargeBodyTestContext("/v1/responses", body, "")
	h.Responses(c)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	return upstream
}

func TestOpenAIResponsesBodySpoolDifferential(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for name, body := range diffBodies() {
		for _, accountType := range []string{"oauth", "apikey", "apikey_passthrough", "apikey_chat"} {
			t.Run(accountType+"/"+name, func(t *testing.T) {
				legacy := runCapture(t, accountType, body, false)
				spooled := runCapture(t, accountType, body, true)
				require.Len(t, spooled.bodies, len(legacy.bodies))
				require.NotEmpty(t, spooled.urls)
				if accountType == "apikey_chat" {
					require.True(t, strings.HasSuffix(spooled.urls[0], "/chat/completions"), spooled.urls[0])
					require.Contains(t, string(spooled.bodies[0]), `"messages"`)
				}
				for i := range legacy.bodies {
					require.True(t, bytes.Equal(legacy.bodies[i], spooled.bodies[i]), "upstream bytes differ")
					require.Equal(t, int64(len(legacy.bodies[i])), spooled.lengths[i], "Content-Length")
					require.Equal(t, legacy.accounts[i], spooled.accounts[i], "session routing")
					for _, k := range []string{"Session_id", "Conversation_id", "Content-Type", "Openai-Beta", "Originator"} {
						require.Equal(t, legacy.headers[i].Values(k), spooled.headers[i].Values(k), k)
					}
				}
			})
		}
	}
}
