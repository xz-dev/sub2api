package service

import (
	"bytes"
	"encoding/json"
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"errors"
	"io"
	"reflect"
)

// useNumberOptions is v1 Unmarshal plus Decoder.UseNumber: every number
// decoded into an interface value becomes a json.Number holding its literal.
var useNumberOptions = jsonv2.JoinOptions(json.DefaultOptionsV1(), jsonv2.WithUnmarshalers(
	jsonv2.UnmarshalFromFunc(func(dec *jsontext.Decoder, v *any) error {
		if dec.PeekKind() != '0' {
			return errors.ErrUnsupported
		}
		raw, err := dec.ReadValue()
		if err != nil {
			return err
		}
		*v = json.Number(raw)
		return nil
	}),
))

func decodeOpenAIJSONUseNumber(data []byte, dst any) error {
	// Fast path: decode in place. A json.Decoder copies the whole input into
	// a growing buffer (about 4x a body made of one large string).
	rv := reflect.ValueOf(dst)
	wasZero := rv.Kind() == reflect.Pointer && !rv.IsNil() && rv.Elem().IsZero()
	if jsonv2.Unmarshal(data, dst, useNumberOptions) == nil {
		return nil
	}
	// Any failure takes the legacy path, which yields the legacy error, from
	// the caller's (zero) dst again: v1 merges into what the fast path left.
	if wasZero {
		rv.Elem().SetZero()
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(dst); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values are not allowed")
		}
		return err
	}
	return nil
}
