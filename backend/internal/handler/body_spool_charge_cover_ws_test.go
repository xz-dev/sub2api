//go:build unit

package handler

import (
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// runCoverWSReadCases adds the failed native WS ingress read (exit gate 7.5
// finding 1) to the charge-covers-allocation property: a client message
// twice the read limit, with the limit at 2, 8 and 16 MiB, is read up to the
// limit and refused. Its total allocation stays within its peak charge;
// the refusal is the named request_too_large event with close 1000; and
// the charge is returned. The 8 GiB fake cgroup puts L at 16 MiB, so
// client_read_limit_bytes sets the smaller limits. It returns the number of
// cases run.
func runCoverWSReadCases(t *testing.T, only string) int {
	cases := 0
	for _, size := range []int{2 << 20, 8 << 20, 16 << 20} {
		name := "ws/ingress/oversize_read/" + coverSize(size)
		if !coverSelected(name, only) {
			continue
		}
		cases++
		t.Run(name, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			cfg := &config.Config{}
			enableBodySpoolForTest(t, cfg, false, 8<<30, nil)
			cfg.Gateway.OpenAIWS.ClientReadLimitBytes = int64(size)
			budget, err := newOpenAIWSReadBudget(cfg)
			require.NoError(t, err)
			require.EqualValues(t, size, budget.Limit())
			srv := newWSReadTestServer(t, cfg)
			msg := wsOversizeMessage(2 * size)
			sendWSMessage(t, srv, msg) // one unmeasured request warms the server up
			charged := requestChargedBytes(cfg)
			require.Eventually(t, func() bool { return charged() == 0 }, coverWSSettle, coverWSPoll)

			var out wsReadOutcome
			r := measureChargeCover(t, charged, func() { out = sendWSMessage(t, srv, msg) })
			logCoverResult(t, name, coverResult{chargeCoverReading: r, status: int(out.closeCode), bytes: len(msg)})
			assert.Equal(t, "request_too_large", gjson.Get(out.event, "error.code").String(), out.event)
			assert.Equal(t, "sub2api", gjson.Get(out.event, "error.hop").String())
			assert.EqualValues(t, size, gjson.Get(out.event, "error.limit_bytes").Int())
			assert.Equal(t, coderws.StatusNormalClosure, out.closeCode)
			assertChargeCovers(t, r)
			assert.Eventually(t, func() bool { return charged() == 0 }, coverWSSettle, coverWSPoll, "the failed read's charge was returned")
		})
	}
	return cases
}

// The server ends the charge after the client has seen the close.
const (
	coverWSSettle = 5 * time.Second
	coverWSPoll   = time.Millisecond
)
