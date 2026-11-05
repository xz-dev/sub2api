package service

import (
	"errors"

	"github.com/gin-gonic/gin"
)

// RequestConversionChargeKey holds the large-request ingest's top-up for a
// request that a protocol converter rewrites (a func() error, set by the
// body-spool ingest when it charged the request). A converted request costs
// k_conv × its inline bytes, more than the k_skel or k_path the ingest
// charged, so every converter calls ChargeRequestConversion before it
// allocates anything (review rr3 blocker 2; TestRequestConvertersChargeFirst
// enforces it).
const RequestConversionChargeKey = "large_request_conversion_charge"

// ErrRequestConversionRefused: the top-up was refused (413 above S_conv or
// L_conv, or 503 insufficient_memory) and the refusal is already written.
// It is final for the request: no failover, no retry.
var ErrRequestConversionRefused = errors.New("converted request refused for memory capacity; the refusal is written")

// ChargeRequestConversion takes the converter top-up for c's request, once.
// nil when the request has no charge (the body spool is off, or a test).
func ChargeRequestConversion(c *gin.Context) error {
	if c == nil {
		return nil
	}
	v, ok := c.Get(RequestConversionChargeKey)
	if !ok {
		return nil
	}
	charge, ok := v.(func() error)
	if !ok || charge == nil {
		return nil
	}
	return charge()
}

// requestConversionCharger is ChargeRequestConversion for c, as a func for
// helpers that have no *gin.Context.
func requestConversionCharger(c *gin.Context) func() error {
	return func() error { return ChargeRequestConversion(c) }
}

// noConversionCharge charges nothing: the body has no request charge (a
// WebSocket message).
func noConversionCharge() error { return nil }

// IsRequestConversionRefused reports whether err is a written, final
// converter refusal.
func IsRequestConversionRefused(err error) bool {
	return errors.Is(err, ErrRequestConversionRefused)
}
