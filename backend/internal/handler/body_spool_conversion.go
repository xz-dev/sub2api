package handler

import (
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

// conversionLimit is S_conv for a covered request (the inline limit, never
// above what k_conv × the inline bytes leaves room for beside the idle
// working set and the margin under memory.max), or L_conv for a fallback
// request (fallbackLimit with k_conv in place of k_path).
func conversionLimit(cfg config.GatewayBodySpoolConfig, memoryMax int64, covered, ticketed bool) int64 {
	if !covered {
		return fallbackLimitFor(cfg, memoryMax, cfg.KConv)
	}
	limit := cfg.InlineLimit
	if ticketed {
		limit = cfg.TicketedInlineLimit
	}
	room := memoryMax - bodyspool.ChargeMargin - cfg.IdleBaseline - bodyspool.ReadBufferBytes
	derived := int64(float64(room)/cfg.KConv) - 24<<10
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
		limit := conversionLimit(rt.cfg, st.charge.Limit(), st.covered, st.lease != nil)
		if inline > limit {
			return refuse(tooLarge(limit, "%s is converted to another protocol for this account; a converted request's limit is %d bytes and this one has %d", c.Request.URL.Path, limit, inline))
		}
		if extra := chargeBytes(rt.cfg.KConv-k, roundUpGranule(inline)); extra > 0 {
			if err := st.charge.Add(extra); err != nil {
				return refuse(refusalFor(err, 0))
			}
		}
		return nil
	})
}
