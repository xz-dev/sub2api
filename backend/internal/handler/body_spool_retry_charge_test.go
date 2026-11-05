//go:build unit

package handler

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// invalidEncryptedContentUpstream answers every call with the 400 that
// makes Forward retry with a decoded, trimmed body.
type invalidEncryptedContentUpstream struct{ largeBodyDrainUpstream }

func (u *invalidEncryptedContentUpstream) Do(req *http.Request, proxy string, accountID int64, concurrency int) (*http.Response, error) {
	resp, err := u.largeBodyDrainUpstream.Do(req, proxy, accountID, concurrency)
	if err != nil {
		return nil, err
	}
	resp.StatusCode = http.StatusBadRequest
	resp.Body = io.NopCloser(bytes.NewBufferString(`{"error":{"code":"invalid_encrypted_content","type":"invalid_request_error",` +
		`"message":"The encrypted content could not be verified."}}`))
	return resp, nil
}

// TestBodySpoolRetryDecodeTopUpRefused (review rr4): the
// invalid_encrypted_content retry decodes the whole body after the first
// upstream call, so it takes the converter top-up first. On a passthrough
// account a body above S_conv (but within S) is sent once as is; the retry's
// top-up is refused with the named 413 (hop sub2api, limit_bytes S_conv):
// no second upstream call, no failover, the charge released.
func TestBodySpoolRetryDecodeTopUpRefused(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upstream := &invalidEncryptedContentUpstream{}
	h := newLargeBodyTestHandler(t, upstream, "apikey")
	enableBodySpoolForTest(t, h.cfg, true, 320<<20, nil)
	rt := bodySpoolRuntimeFor(h.cfg)
	sConv := conversionLimit(rt.cfg, 320<<20, true, false)
	body := largeResponsesTextBody(int(sConv) + 1<<20)
	c, rec := newLargeBodyTestContext("/v1/responses", body, "")
	h.Responses(c)
	releaseRequestBodySpool(c)
	require.Equal(t, http.StatusRequestEntityTooLarge, rec.Code, rec.Body.String())
	var out struct {
		Error map[string]any `json:"error"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out), rec.Body.String())
	require.Equal(t, "request_too_large", out.Error["code"])
	require.Equal(t, bodySpoolHop, out.Error["hop"])
	require.EqualValues(t, sConv, out.Error["limit_bytes"])
	require.EqualValues(t, 1, upstream.calls.Load(), "the first send only: no retry, no failover")
	require.Zero(t, rt.meter.Outstanding())
}
