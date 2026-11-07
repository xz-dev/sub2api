package handler

import (
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/bodyspool"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// Converted requests (review rr3 blocker 2). The ingest cannot know whether
// the account the handler picks later serves the request natively or
// through a protocol converter, so it charges k_skel (covered) or k_path
// (fallback) as for passthrough. A converter calls
// service.ChargeRequestConversion before it allocates; that takes the rest
// of k_conv × the inline bytes, once, on the request's charge (released
// with it at chain return, panics included). Above S_conv (covered) or
// L_conv (fallback) it writes a 413 with limit_bytes; a short budget gets
// the 503 insufficient_memory with the bytes needed. Either refusal is
// final: the converter returns service.ErrRequestConversionRefused, which
// is not a failover error.

// kConvClass is one row of the per-class k_conv table.
type kConvClass struct {
	k float64
	// measured: the worst total allocation / body bytes of the class over
	// the charge-cover corpus (p1-p7 and the gen parts, 2/8/16 MiB, at
	// e69ae929aa); k = ceil(measured x 1.15), at least config k_conv (65).
	measured float64
}

// kConvClasses (review rr10): k_conv per charge class. Total allocation is
// the bound (GC can be starved in production), so each class is charged
// its own worst total allocation plus 15 %. The class follows how the
// ingest charges: a covered Responses request and a Responses fallback
// request (compact, Content-Encoding) take the class's top-up when they go
// off the verified-raw list; every other route is charged its class's
// k_conv from the first byte. The converter rework (M3) is how these come
// back down.
var kConvClasses = map[string]kConvClass{
	// Covered Responses route, OpenAI platform, off-list top-up (S_conv):
	// Responses->Chat on force_chat, codex_tui shape, 16 MiB (98.38); the
	// Codex OAuth transform 76.61.
	"responses": {k: 114, measured: 98.38},
	// Responses fallback, off-list top-up (L_conv): compact on the Codex
	// OAuth account, codex_tui, 16 MiB (92.86).
	"responses_fallback": {k: 107, measured: 92.86},
	// A Responses route on a non-OpenAI group (Responses->Anthropic or
	// Gemini): rr6_custom_tools, 16 MiB (124.30).
	"responses_other_platform": {k: 143, measured: 124.30},
	// /chat/completions: Chat->Gemini, rr6_tinyinvalid, 8 MiB (225.73);
	// Chat->Anthropic 176.82, the Codex OAuth account 130.17.
	"chat": {k: 260, measured: 225.73},
	// /messages: Anthropic->Responses on the Codex OAuth account, tiny,
	// 8 MiB (138.24); API-key accounts 98.26.
	"messages": {k: 159, measured: 138.24},
	// /count_tokens: the Grok local estimate, rr6_tinyinvalid, 8 MiB
	// (134.94); OpenAI accounts 74.58.
	"count_tokens": {k: 156, measured: 134.94},
	// Gemini-native /v1beta/ (also under /antigravity/): Antigravity
	// generateContent, rr6_tinyinvalid, 8 MiB (127.13); AI Studio 38.69.
	"gemini": {k: 147, measured: 127.13},
	// /web_search (Grok): rr6_tinyinvalid, 16 MiB (24.56).
	"web_search": {k: 65, measured: 24.56},
	// /embeddings (OpenAI API-key accounts only; M3, measured at 1 and
	// 4 MiB, f3db26d98a): the blocking guard on, an array of strings,
	// 1 MiB (5.91); model mapping 4.06, plain 3.05. At 64 KiB the fixed
	// per-request cost dominates: 10.87.
	"embeddings": {k: 65, measured: 5.91},
	// /rerank (OpenAI API-key accounts only; M3, 1 and 4 MiB): a mapped
	// Bailian text-rerank model (body rebuilt nested), many documents,
	// chunked, 1 MiB (8.09); the guard 4.86, plain 3.05. At 64 KiB: 9.38.
	"rerank": {k: 65, measured: 8.09},
	// Any other route: not measured, so the worst class.
	"unmeasured": {k: 260},
}

// kConvRouteClasses maps each registered route (routes/gateway.go) whose
// protocol a class row measured to that class. The key is the matched route,
// never a path suffix: a Responses subpath such as
// /v1/responses/*subpath ending in /web_search is served by the Responses
// handler and is not measured by the web_search row.
var kConvRouteClasses = map[string]string{
	"/v1/chat/completions":                    "chat",
	"/chat/completions":                       "chat",
	"/v1/messages/count_tokens":               "count_tokens",
	"/messages/count_tokens":                  "count_tokens",
	"/antigravity/v1/messages/count_tokens":   "count_tokens",
	"/v1/messages":                            "messages",
	"/antigravity/v1/messages":                "messages",
	"/v1beta/models/*modelAction":             "gemini",
	"/antigravity/v1beta/models/*modelAction": "gemini",
	"/v1/web_search":                          "web_search",
	"/web_search":                             "web_search",
	"/v1/embeddings":                          "embeddings",
	"/embeddings":                             "embeddings",
	"/v1/rerank":                              "rerank",
}

// kConvClassFor names the charge class of c's request (kConvClasses). The
// class comes from the matched route, c.FullPath(), or, when c has no matched
// route, from the request path compared for equality; anything else is
// unmeasured.
func kConvClassFor(c *gin.Context, covered, responsesFallback bool) string {
	switch {
	case covered:
		return "responses"
	case responsesFallback:
		return "responses_fallback"
	case isBareOpenAIResponsesPath(c):
		return "responses_other_platform"
	}
	route := c.FullPath()
	if route == "" {
		route = strings.TrimRight(strings.TrimSpace(c.Request.URL.Path), "/")
	}
	if class, ok := kConvRouteClasses[route]; ok {
		return class
	}
	return "unmeasured"
}

// kConvFor is the k_conv of class: its table row, at least config k_conv.
func kConvFor(cfg config.GatewayBodySpoolConfig, class string) float64 {
	k, ok := kConvClasses[class]
	if !ok {
		k = kConvClasses["unmeasured"]
	}
	return max(cfg.KConv, k.k)
}

// conversionLimit is S_conv for a covered request (the inline limit, never
// above what kConv × the inline bytes leaves room for beside the idle
// working set and the margin under memory.max), or L_conv for a fallback
// request (fallbackLimit with kConv in place of k_path). kConv is the
// request's class k_conv (kConvFor).
func conversionLimit(cfg config.GatewayBodySpoolConfig, memoryMax int64, covered, ticketed bool, kConv float64) int64 {
	if !covered {
		return fallbackLimitFor(cfg, memoryMax, kConv)
	}
	limit := cfg.InlineLimit
	if ticketed {
		limit = cfg.TicketedInlineLimit
	}
	room := memoryMax - bodyspool.ChargeMargin - cfg.IdleBaseline - bodyspool.ReadBufferBytes
	derived := int64(float64(room)/kConv) - 24<<10
	derived -= derived % bodyspool.DefaultChargeStep
	return max(0, min(limit, derived))
}

// setConversionCharge installs the converter top-up for st's request.
func setConversionCharge(c *gin.Context, rt *bodySpoolRuntime, st *bodySpoolState, k float64, inline int64) {
	done := false
	var result error
	c.Set(service.RequestConversionChargeKey, func() error {
		if done {
			return result
		}
		done = true
		if st.released {
			return nil
		}
		refuse := func(r *capacityRefusal) error {
			writeCapacityRefusal(c, r)
			result = service.ErrRequestConversionRefused
			return result
		}
		limit := conversionLimit(rt.cfg, st.charge.Limit(), st.covered, st.lease != nil, st.kConv)
		if inline > limit {
			return refuse(tooLarge(limit, "%s is converted to another protocol for this account; a converted request's limit is %d bytes and this one has %d", c.Request.URL.Path, limit, inline))
		}
		if extra := chargeBytes(st.kConv-k, roundUpGranule(inline)); extra > 0 {
			if err := st.charge.Add(extra); err != nil {
				return refuse(refusalFor(err, 0))
			}
		}
		return nil
	})
}
