package service

import (
	"sync"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

// Off-list observability (rr6 follow-up). A request on a covered route
// that is not on the verified-raw list (request_raw_list.go) gets the
// conservative k_conv charge, and so the lower S_conv / L_conv limit. Each
// such decision is counted per route class and logged once per request,
// with the first failing reason: a member name, an item, part or tool
// type, a header name or an account flag name. Never a value.

// LargeRequestRouteClassKey holds the route class of a request the
// body-spool ingest put under the verified-raw list (a string: "responses"
// for a covered route, "responses_compact" or "responses_encoded" for a
// Responses fallback). Absent on every other route and when the spool is
// off; then no off-list decision is recorded.
const LargeRequestRouteClassKey = "large_request_route_class"

// largeRequestOffListKey marks a request whose off-list decision is
// recorded (once per request: the ingest and each forward attempt can
// each find it off the list).
const largeRequestOffListKey = "large_request_off_list"

var largeRequestOffList struct {
	mu sync.Mutex
	n  map[string]uint64 // route class -> decisions
}

// LargeRequestOffListSnapshot returns the off-list decisions per route
// class since the process started.
func LargeRequestOffListSnapshot() map[string]uint64 {
	largeRequestOffList.mu.Lock()
	defer largeRequestOffList.mu.Unlock()
	out := make(map[string]uint64, len(largeRequestOffList.n))
	for k, v := range largeRequestOffList.n {
		out[k] = v
	}
	return out
}

func countLargeRequestOffList(class string) uint64 {
	largeRequestOffList.mu.Lock()
	defer largeRequestOffList.mu.Unlock()
	if largeRequestOffList.n == nil {
		largeRequestOffList.n = map[string]uint64{}
	}
	largeRequestOffList.n[class]++
	return largeRequestOffList.n[class]
}

// ChargeRequestOffList takes the k_conv top-up for c's request, which is
// not on the verified-raw list for reason (stage "request": the body or
// headers; "account": the account or transport of a forward attempt), and
// records the decision the first time for this request: the counter and
// one structured log line "large_request.off_list".
func ChargeRequestOffList(c *gin.Context, stage, reason string) error {
	err := ChargeRequestConversion(c)
	if c == nil {
		return err
	}
	class := c.GetString(LargeRequestRouteClassKey)
	if class == "" {
		return err
	}
	if _, done := c.Get(largeRequestOffListKey); done {
		return err
	}
	c.Set(largeRequestOffListKey, reason)
	outcome := "charged"
	if err != nil {
		outcome = "refused"
	}
	total := countLargeRequestOffList(class)
	log := logger.L()
	if c.Request != nil {
		log = logger.FromContext(c.Request.Context())
	}
	log.Info("large_request.off_list",
		zap.String("route_class", class),
		zap.String("stage", stage),
		zap.String("reason", reason),
		zap.String("outcome", outcome),
		zap.Uint64("off_list_total", total),
	)
	return err
}

// rawReasonName is a client-sent name (a member name or a type) as it
// may appear in an off-list reason: at most 64 bytes, each byte outside
// [A-Za-z0-9_.:/-] replaced by '_', and "..." when it was cut.
func rawReasonName(s string) string {
	const maxLen = 64
	cut := len(s) > maxLen
	if cut {
		s = s[:maxLen]
	}
	b := []byte(s)
	for i, ch := range b {
		if !(ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' ||
			ch == '_' || ch == '.' || ch == ':' || ch == '/' || ch == '-') {
			b[i] = '_'
		}
	}
	if cut {
		return string(b) + "..."
	}
	return string(b)
}
