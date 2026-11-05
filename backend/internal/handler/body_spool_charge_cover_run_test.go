//go:build unit

package handler

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"sync/atomic"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
)

// coverUpstream drains the forwarded body and answers in the protocol the
// upstream URL asks for, so each route runs to its response writer.
type coverUpstream struct {
	calls     atomic.Int32
	forwarded atomic.Int64
	lastPath  atomic.Value // string: the last upstream URL path
	// rejectEncrypted > 0: the next Responses sends get a 400
	// invalid_encrypted_content (one per count), as for a replayed item the
	// upstream cannot decrypt.
	rejectEncrypted atomic.Int32
}

// streamScan discards what it is given and notes whether `"stream":true`
// went by, keeping a small overlap across writes.
type streamScan struct {
	tail   []byte
	stream bool
}

func (s *streamScan) Write(p []byte) (int, error) {
	const key = `"stream":true`
	if !s.stream {
		buf := append(s.tail, p[:min(len(p), len(key))]...)
		s.stream = bytes.Contains(buf, []byte(key)) || bytes.Contains(p, []byte(key))
		s.tail = append(s.tail[:0], p[max(0, len(p)-len(key)):]...)
	}
	return len(p), nil
}

func (u *coverUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	u.calls.Add(1)
	scan := &streamScan{}
	if req.Body != nil {
		n, _ := io.Copy(scan, req.Body)
		u.forwarded.Add(n)
	}
	path := req.URL.Path
	u.lastPath.Store(path)
	if strings.HasSuffix(path, "/responses") && u.rejectEncrypted.Load() > 0 {
		u.rejectEncrypted.Add(-1)
		return &http.Response{
			StatusCode: http.StatusBadRequest,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(`{"error":{"type":"invalid_request_error","code":"invalid_encrypted_content",` +
				`"message":"The encrypted content could not be verified."}}`)),
			Request: req,
		}, nil
	}
	if scan.stream && strings.HasSuffix(path, "/messages") { // Anthropic stream
		return coverSSE(req, `event: message_start`+"\n"+`data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-sonnet-4-5","content":[],"usage":{"input_tokens":10,"output_tokens":0}}}`,
			`event: content_block_start`+"\n"+`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
			`event: content_block_delta`+"\n"+`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}`,
			`event: content_block_stop`+"\n"+`data: {"type":"content_block_stop","index":0}`,
			`event: message_delta`+"\n"+`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}`,
			`event: message_stop`+"\n"+`data: {"type":"message_stop"}`), nil
	}
	if scan.stream && strings.HasSuffix(path, "/chat/completions") { // Chat stream
		return coverSSE(req, `data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"gpt-5.5","choices":[{"index":0,"delta":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":1,"total_tokens":11}}`,
			`data: [DONE]`), nil
	}
	if scan.stream && !strings.Contains(path, "/chat/completions") && !strings.Contains(path, "/messages") && !strings.Contains(path, "GenerateContent") {
		// A Responses stream (the converters ask for one): one completed event.
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body: io.NopCloser(strings.NewReader("data: {\"type\":\"response.completed\",\"response\":" +
				`{"id":"resp_1","object":"response","status":"completed","model":"gpt-5.5",` +
				`"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],` +
				`"usage":{"input_tokens":10,"output_tokens":1,"total_tokens":11}}}` + "\n\ndata: [DONE]\n\n")),
			Request: req,
		}, nil
	}
	body := `{"id":"resp_1","object":"response","status":"completed","model":"gpt-5.5",` +
		`"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],` +
		`"usage":{"input_tokens":10,"output_tokens":1,"total_tokens":11}}`
	switch {
	case strings.HasSuffix(path, "/input_tokens"):
		body = `{"object":"response.input_tokens","input_tokens":10}`
	case strings.HasSuffix(path, "/chat/completions"):
		body = `{"id":"c1","object":"chat.completion","created":1,"model":"gpt-5.5","choices":[{"index":0,` +
			`"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],` +
			`"usage":{"prompt_tokens":10,"completion_tokens":1,"total_tokens":11}}`
	case strings.Contains(path, ":countTokens"):
		body = `{"totalTokens":10}`
	case strings.Contains(path, ":generateContent") || strings.Contains(path, ":streamGenerateContent"):
		body = `{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP"}],` +
			`"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":1,"totalTokenCount":11}}`
		if strings.Contains(path, "/v1internal") { // Antigravity wraps the Gemini response
			body = `{"response":` + body + `}`
		}
	case strings.HasSuffix(path, "/messages/count_tokens"):
		body = `{"input_tokens":10}`
	case strings.HasSuffix(path, "/messages"):
		body = `{"id":"msg_1","type":"message","role":"assistant","model":"claude-sonnet-4-5",` +
			`"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn",` +
			`"usage":{"input_tokens":10,"output_tokens":1}}`
	}
	ctype := "application/json"
	if strings.Contains(path, ":streamGenerateContent") {
		ctype, body = "text/event-stream", "data: "+body+"\n\n"
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{ctype}},
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    req,
	}, nil
}

// coverSSE is an event-stream response of the given events.
func coverSSE(req *http.Request, events ...string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(strings.Join(events, "\n\n") + "\n\n")),
		Request:    req,
	}
}

func (u *coverUpstream) DoWithTLS(req *http.Request, p string, id int64, n int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, p, id, n)
}
