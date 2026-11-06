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

// RequestConversionCharger is ChargeRequestConversion for c, as a func for
// helpers that have no *gin.Context.
func RequestConversionCharger(c *gin.Context) func() error {
	return func() error { return ChargeRequestConversion(c) }
}

// NoConversionCharge charges nothing: the body has no request charge (a
// WebSocket message).
func NoConversionCharge() error { return nil }

// DecodeFullRequestBody is the one door for a full decode of a client
// request body into Go values (encoding/json v1 semantics, numbers as
// json.Number): such a decode costs about k_conv x the body, so it takes
// charge, the k_conv top-up, before it allocates anything. A refused charge
// returns its error (IsRequestConversionRefused: the 413/503 is written)
// and leaves dst untouched. charge is RequestConversionCharger(c), a
// charger a caller passed down, or NoConversionCharge for a body without a
// request charge (a WebSocket message). Not every full decode goes through
// here (review rr6 found three that did not): the safety net is the
// fail-safe charge in request_raw_list.go, which takes the k_conv top-up up
// front for any request or account not on the verified-raw list. The
// static check (TestRequestConvertersChargeFirst) knows only the listed
// converters and this door; the charge-cover property test is the gate.
func DecodeFullRequestBody(charge func() error, body []byte, dst any) error {
	if err := charge(); err != nil {
		return err
	}
	return decodeOpenAIJSONUseNumber(body, dst)
}

// IsRequestConversionRefused reports whether err is a written, final
// converter refusal.
func IsRequestConversionRefused(err error) bool {
	return errors.Is(err, ErrRequestConversionRefused)
}
