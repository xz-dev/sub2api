//go:build unit

package handler

import (
	"bytes"
	"strconv"
	"strings"
)

// rr10 review additions (128 shapes, adopted in rr11): the generator cells
// the adv_* corpus lacked: cap boundaries (4 KiB - 1, 4 KiB, 4 KiB + 1),
// many items with a capped identifier, escaped and body-sized member
// names, single-escape strings (adv_rv1_*), exponent-form numbers and a
// body-sized json_schema. Names adv_rv_* and adv_rv1_*; they run with
// SUB2API_CHARGE_COVER_GEN=1 on every adv combo.

// rvIdent: a JSON string body (no quotes) of exactly l raw bytes, unique
// for i, made of coverAdvMixUnit (every escape kind) padded with 'a'.
func rvIdent(l, i int) string {
	p := "x" + strconv.Itoa(i) + "_"
	r := l - len(p)
	units := r / len(coverAdvMixUnit)
	return p + strings.Repeat(coverAdvMixUnit, units) + strings.Repeat("a", r-units*len(coverAdvMixUnit))
}

// rvPlain: exactly l raw bytes of plain letters, unique for i.
func rvPlain(l, i int) string {
	p := "x" + strconv.Itoa(i) + "_"
	return p + strings.Repeat("a", l-len(p))
}

// rvItems fills input with item(i) up to about n bytes, then a short
// user message.
func rvItems(model string, n int, item func(i int) string) []byte {
	return rvArray(`{"model":"`+model+`","stream":false,"store":false,"input":[`, `{"role":"user","content":"last"}]}`, n, item)
}

// rvArray writes head, item(0), item(1), ... while they fit in n, then tail.
func rvArray(head, tail string, n int, item func(i int) string) []byte {
	var b bytes.Buffer
	b.Grow(n + 64<<10)
	b.WriteString(head)
	for i := 0; ; i++ {
		it := item(i)
		if b.Len()+len(it)+1+len(tail) > n {
			break
		}
		b.WriteString(it)
		b.WriteByte(',')
	}
	if tail[0] == ']' || tail[0] == '}' { // no element after the last comma
		b.Truncate(b.Len() - 1)
	}
	b.WriteString(tail)
	return b.Bytes()
}

func coverAdvRVShapes() []coverAdvShape {
	var out []coverAdvShape
	add := func(name string, body func(model string, n int) []byte) {
		out = append(out, coverAdvShape{name: "adv_rv_" + name, body: body})
	}
	mix := func(n int) string { return coverAdvRepeat(coverAdvMixUnit, n) }
	plainA := func(n int) string { return strings.Repeat("a", max(1, n)) }
	q := `"input":[{"role":"user","content":"q"}]}`

	// 1. Cap boundary (4 KiB - 1, 4 KiB, 4 KiB + 1) on every capped field;
	// item identifiers also at 255, 256 and 257 bytes (rr11): escaped ids
	// within 6 x 64 raw bytes, which the input-ID checks decode in place.
	for _, l := range []int{255, 256, 257, 4095, 4096, 4097} {
		sfx := "_cap" + strconv.Itoa(l)
		// 1a. Many items, each with one capped identifier at the cap.
		add("many_fc_pair_callid"+sfx, func(model string, n int) []byte {
			return rvItems(model, n, func(i int) string {
				id := rvIdent(l, i)
				return `{"type":"function_call","call_id":"` + id + `","name":"f","arguments":"{}"},{"type":"function_call_output","call_id":"` + id + `","output":"ok"}`
			})
		})
		add("many_fc_name"+sfx, func(model string, n int) []byte {
			return rvItems(model, n, func(i int) string {
				c := "call_" + strconv.Itoa(i)
				return `{"type":"function_call","call_id":"` + c + `","name":"` + rvIdent(l, i) + `","arguments":"{}"},{"type":"function_call_output","call_id":"` + c + `","output":"ok"}`
			})
		})
		add("many_msg_id"+sfx, func(model string, n int) []byte {
			return rvItems(model, n, func(i int) string {
				return `{"type":"message","role":"assistant","id":"` + rvIdent(l, i) + `","content":[{"type":"output_text","text":"a"}]}`
			})
		})
		add("many_msg_status"+sfx, func(model string, n int) []byte {
			return rvItems(model, n, func(i int) string {
				return `{"type":"message","role":"assistant","status":"` + rvIdent(l, i) + `","content":[{"type":"output_text","text":"a"}]}`
			})
		})
		add("many_reasoning_id"+sfx, func(model string, n int) []byte {
			return rvItems(model, n, func(i int) string {
				return `{"type":"reasoning","id":"` + rvIdent(l, i) + `","summary":[],"encrypted_content":"e"}`
			})
		})
		add("many_itemref_id"+sfx, func(model string, n int) []byte {
			return rvItems(model, n, func(i int) string {
				return `{"type":"item_reference","id":"` + rvIdent(l, i) + `"}`
			})
		})
		add("many_fc_namespace"+sfx, func(model string, n int) []byte {
			return rvItems(model, n, func(i int) string {
				c := "call_" + strconv.Itoa(i)
				return `{"type":"function_call","namespace":"` + rvPlain(l, i) + `","call_id":"` + c + `","name":"f","arguments":"{}"},{"type":"function_call_output","call_id":"` + c + `","output":"ok"}`
			})
		})
		if l < 4095 { // the root and metadata cells: at the cap only
			continue
		}
		// 1b. Many metadata entries at the cap (value; key).
		add("metadata_many_val"+sfx, func(model string, n int) []byte {
			return rvArray(`{"model":"`+model+`","stream":false,"store":false,"metadata":{`, `},`+q, n, func(i int) string {
				return `"k` + strconv.Itoa(i) + `":"` + rvIdent(l, i) + `"`
			})
		})
		add("metadata_many_key"+sfx, func(model string, n int) []byte {
			return rvArray(`{"model":"`+model+`","stream":false,"store":false,"metadata":{`, `},`+q, n, func(i int) string {
				return `"` + rvIdent(l, i) + `":"v"`
			})
		})
		// 1c. One root identifier at the cap, the body filled by a plain
		// input string.
		for _, f := range []struct{ name, pre string }{
			{"prompt_cache_key", `"prompt_cache_key":"%s"`},
			{"user", `"user":"%s"`},
			{"safety_identifier", `"safety_identifier":"%s"`},
			{"service_tier", `"service_tier":"%s"`},
			{"truncation", `"truncation":"%s"`},
			{"previous_response_id", `"previous_response_id":"%s"`},
			{"conversation", `"conversation":"%s"`},
			{"conversation_obj", `"conversation":{"id":"%s"}`},
			{"reasoning_effort", `"reasoning":{"effort":"%s"}`},
			{"reasoning_summary", `"reasoning":{"effort":"low","summary":"%s"}`},
			{"metadata_val", `"metadata":{"k":"%s"}`},
		} {
			add("root_"+f.name+sfx, func(model string, n int) []byte {
				pre := strings.Replace(f.pre, "%s", rvIdent(l, 0), 1) + `,"input":"`
				return coverAdvWrap(model, pre, `"}`, n, plainA)
			})
		}
		add("root_model"+sfx, func(model string, n int) []byte {
			head := `{"model":"` + model + rvIdent(l-len(model), 0) + `","stream":false,"store":false,"input":"`
			var b bytes.Buffer
			b.WriteString(head)
			b.WriteString(plainA(n - len(head) - 64))
			b.WriteString(`"}`)
			return b.Bytes()
		})
	}

	// 2. Many-items cells of content classes the adv_items_* shapes lack.
	add("many_parts_4k_mix", func(model string, n int) []byte {
		return rvArray(`{"model":"`+model+`","stream":false,"store":false,"input":[{"role":"user","content":[`, `]}]}`, n, func(i int) string {
			return `{"type":"input_text","text":"` + mix(4<<10) + `"}`
		})
	})
	add("many_summary_parts_4k_mix", func(model string, n int) []byte {
		return rvArray(`{"model":"`+model+`","stream":false,"store":false,"input":[{"type":"reasoning","encrypted_content":"e","summary":[`, `]},{"role":"user","content":"q"}]}`, n, func(i int) string {
			return `{"type":"summary_text","text":"` + mix(4<<10) + `"}`
		})
	})
	add("many_reasoning_enc_4k_mix", func(model string, n int) []byte {
		return rvItems(model, n, func(i int) string {
			return `{"type":"reasoning","summary":[{"type":"summary_text","text":"s"}],"encrypted_content":"` + mix(4<<10) + `"}`
		})
	})
	add("many_fc_args_4k_mix", func(model string, n int) []byte {
		return rvItems(model, n, func(i int) string {
			c := "call_" + strconv.Itoa(i)
			return `{"type":"function_call","call_id":"` + c + `","name":"f","arguments":"` + mix(4<<10) + `"},{"type":"function_call_output","call_id":"` + c + `","output":"` + mix(4<<10) + `"}`
		})
	})
	add("many_fco_output_parts_4k_mix", func(model string, n int) []byte {
		return rvArray(`{"model":"`+model+`","stream":false,"store":false,"input":[{"type":"function_call","call_id":"c1","name":"f","arguments":"{}"},{"type":"function_call_output","call_id":"c1","output":[`, `]},{"role":"user","content":"q"}]}`, n, func(i int) string {
			return `{"type":"input_text","text":"` + mix(4<<10) + `"}`
		})
	})
	add("many_tools_name_4k_mix", func(model string, n int) []byte {
		return rvArray(`{"model":"`+model+`","stream":false,"store":false,"tools":[`, `],`+q, n, func(i int) string {
			return `{"type":"function","name":"` + rvIdent(4<<10, i) + `","parameters":{"type":"object","properties":{}}}`
		})
	})
	add("many_tools_desc_4k_mix", func(model string, n int) []byte {
		return rvArray(`{"model":"`+model+`","stream":false,"store":false,"tools":[`, `],`+q, n, func(i int) string {
			return `{"type":"function","name":"f` + strconv.Itoa(i) + `","description":"` + mix(4<<10) + `","parameters":{"type":"object","properties":{}}}`
		})
	})
	add("many_include_4k_mix", func(model string, n int) []byte {
		return rvArray(`{"model":"`+model+`","stream":false,"store":false,"include":[`, `],`+q, n, func(i int) string {
			return `"` + rvIdent(4<<10, i) + `"`
		})
	})
	add("many_part_keys_4k_mix", func(model string, n int) []byte {
		return rvArray(`{"model":"`+model+`","stream":false,"store":false,"input":[{"role":"user","content":[{"type":"input_text","text":"q",`, `}]}]}`, n, func(i int) string {
			return `"` + rvIdent(4<<10, i) + `":1`
		})
	})
	add("many_tool_keys_4k_mix", func(model string, n int) []byte {
		return rvArray(`{"model":"`+model+`","stream":false,"store":false,"tools":[{"type":"function","name":"f","parameters":{"type":"object","properties":{}},`, `}],`+q, n, func(i int) string {
			return `"` + rvIdent(4<<10, i) + `":1`
		})
	})

	// 3. Body-sized escaped member names (keys) in objects whose names the
	// list does not check, and body-sized uncapped values beside capped ones.
	bigKey := func(n int) string { return mix(n) }
	for _, k := range []struct{ name, pre, post string }{
		{"key_part", `"input":[{"role":"user","content":[{"type":"input_text","text":"q","`, `":1}]}]}`},
		{"key_tool", `"tools":[{"type":"function","name":"f","parameters":{"type":"object","properties":{}},"`, `":1}],` + q},
		{"key_params", `"tools":[{"type":"function","name":"f","parameters":{"type":"object","properties":{"`, `":{"type":"string"}}}}],` + q},
		{"key_reasoning", `"reasoning":{"effort":"low","`, `":1},` + q},
		{"val_reasoning_other", `"reasoning":{"effort":"low","x":"`, `"},` + q},
		{"key_conversation", `"conversation":{"`, `":"c"},` + q},
		{"key_text", `"text":{"verbosity":"low","`, `":1},` + q},
		{"key_client_metadata", `"client_metadata":{"`, `":"v"},` + q},
		{"key_stream_options", `"stream_options":{"`, `":true},` + q},
		{"key_tool_choice", `"tool_choice":{"type":"function","name":"f","`, `":1},` + q},
		{"key_item_part_output", `"input":[{"role":"assistant","content":[{"type":"output_text","text":"a","annotations":[],"`, `":1}]},{"role":"user","content":"q"}]}`},
		{"text_schema_desc", `"text":{"format":{"type":"json_schema","name":"s","schema":{"type":"object","description":"`, `"}}},` + q},
		{"params_desc", `"tools":[{"type":"function","name":"f","parameters":{"type":"object","description":"`, `","properties":{}}}],` + q},
		{"context_management_str", `"context_management":[{"type":"compaction","x":"`, `"}],` + q},
		{"tool_choice_name", `"tool_choice":{"type":"function","name":"`, `"},` + q},
		{"prompt_cache_options_val", `"prompt_cache_options":{"mode":"`, `"},` + q},
	} {
		add(k.name+"_big_mix", func(model string, n int) []byte {
			return coverAdvWrap(model, k.pre, k.post, n, bigKey)
		})
	}
	add("text_schema_instr_esc", func(model string, n int) []byte { // rr9 rv_instr_esc_schema
		return coverAdvWrap(model, `"text":{"format":{"type":"json_schema","name":"s","schema":{"type":"object","properties":{"a":{"type":"string"}}}}},"instructions":"`, `",`+q, n, mix)
	})

	// 4. Huge numeric forms beyond adv_num_* int/frac.
	nums := map[string]func(n int) string{
		"exp":     func(n int) string { return "1e" + strings.Repeat("0", max(1, n-3)) + "1" },
		"negfrac": func(n int) string { return "-0." + strings.Repeat("0", max(1, n-4)) + "1" },
		"mantexp": func(n int) string { return "1." + strings.Repeat("0", max(1, n-6)) + "e-5" },
		"negexp":  func(n int) string { return "1e-" + strings.Repeat("9", max(1, n-3)) },
	}
	for form, digits := range nums {
		add("num_max_output_tokens_"+form, func(model string, n int) []byte {
			return coverAdvWrap(model, `"max_output_tokens":`, ","+q, n, digits)
		})
		add("num_temperature_"+form, func(model string, n int) []byte {
			return coverAdvWrap(model, `"temperature":`, ","+q, n, digits)
		})
	}
	add("num_params_max_int", func(model string, n int) []byte {
		return coverAdvWrap(model, `"tools":[{"type":"function","name":"f","parameters":{"type":"object","properties":{},"maximum":`, `}}],`+q, n,
			func(n int) string { return "1" + strings.Repeat("0", max(1, n-1)) })
	})
	add("num_context_management_int", func(model string, n int) []byte {
		return coverAdvWrap(model, `"context_management":[{"type":"compaction","compact_threshold":`, `}],`+q, n,
			func(n int) string { return "1" + strings.Repeat("0", max(1, n-1)) })
	})
	add("num_many_params_enum", func(model string, n int) []byte {
		return rvArray(`{"model":"`+model+`","stream":false,"store":false,"tools":[{"type":"function","name":"f","parameters":{"type":"object","properties":{"a":{"type":"number","enum":[`, `]}}}}],`+q, n, func(i int) string {
			return "1.7976931348623157e308"
		})
	})
	add("num_many_computer_x_4k", func(model string, n int) []byte { // many 4 KiB numbers in items
		return rvItems(model, n, func(i int) string {
			return `{"type":"computer_call","call_id":"c` + strconv.Itoa(i) + `","action":{"type":"click","x":1` + strings.Repeat("0", 4000) + `,"y":1,"button":"left"},"pending_safety_checks":[],"status":"completed"}`
		})
	})
	out = append(out, coverAdvRV1EscShapes()...)
	return out
}

// rv1esc: one escape then plain letters, about n bytes: gjson's unescape
// allocates the raw length plus a decoded copy of about the same length,
// the most per raw byte (the mix unit decodes to about half).
func rv1esc(n int) string { return `\n` + strings.Repeat("a", max(1, n-2)) }

// rv1escIdent: exactly l raw bytes, unique for i, one escape.
func rv1escIdent(l, i int) string {
	p := "x" + strconv.Itoa(i) + `_\n`
	return p + strings.Repeat("a", l-len(p))
}

func coverAdvRV1EscShapes() []coverAdvShape {
	var out []coverAdvShape
	add := func(name string, body func(model string, n int) []byte) {
		out = append(out, coverAdvShape{name: "adv_rv1_" + name, body: body})
	}
	q := `"input":[{"role":"user","content":"q"}]}`
	for _, k := range []struct{ name, pre, post string }{
		{"input_string", `"input":"`, `"}`},
		{"instructions", `"instructions":"`, `",` + q},
		{"developer", `"input":[{"role":"developer","content":"`, `"},{"role":"user","content":"q"}]}`},
		{"developer_part", `"input":[{"role":"developer","content":[{"type":"input_text","text":"`, `"}]},{"role":"user","content":"q"}]}`},
		{"user_part", `"input":[{"role":"user","content":[{"type":"input_text","text":"`, `"}]}]}`},
		{"tool_description", `"tools":[{"type":"function","name":"f","description":"`, `","parameters":{"type":"object","properties":{}}}],` + q},
		{"tool_name", `"tools":[{"type":"function","name":"`, `","parameters":{"type":"object","properties":{}}}],` + q},
		{"fc_arguments", `"input":[{"type":"function_call","call_id":"c1","name":"f","arguments":"`, `"},{"type":"function_call_output","call_id":"c1","output":"ok"},{"role":"user","content":"q"}]}`},
		{"fco_output", `"input":[{"type":"function_call","call_id":"c1","name":"f","arguments":"{}"},{"type":"function_call_output","call_id":"c1","output":"`, `"},{"role":"user","content":"q"}]}`},
		{"summary_text", `"input":[{"type":"reasoning","encrypted_content":"e","summary":[{"type":"summary_text","text":"`, `"}]},{"role":"user","content":"q"}]}`},
		{"encrypted_content", `"input":[{"type":"reasoning","summary":[],"encrypted_content":"`, `"},{"role":"user","content":"q"}]}`},
		{"key_tool_choice", `"tool_choice":{"type":"function","name":"f","`, `":1},` + q},
		{"key_tool", `"tools":[{"type":"function","name":"f","parameters":{"type":"object","properties":{}},"`, `":1}],` + q},
		{"key_part", `"input":[{"role":"user","content":[{"type":"input_text","text":"q","`, `":1}]}]}`},
		{"key_client_metadata", `"client_metadata":{"`, `":"v"},` + q},
		{"text_schema_desc", `"text":{"format":{"type":"json_schema","name":"s","schema":{"type":"object","description":"`, `"}}},` + q},
	} {
		add(k.name+"_big", func(model string, n int) []byte { return coverAdvWrap(model, k.pre, k.post, n, rv1esc) })
	}
	for _, l := range []int{256, 4096} {
		sfx := "_cap" + strconv.Itoa(l)
		add("many_reasoning_id"+sfx, func(model string, n int) []byte {
			return rvItems(model, n, func(i int) string {
				return `{"type":"reasoning","id":"` + rv1escIdent(l, i) + `","summary":[],"encrypted_content":"e"}`
			})
		})
		add("many_msg_id"+sfx, func(model string, n int) []byte {
			return rvItems(model, n, func(i int) string {
				return `{"type":"message","role":"assistant","id":"` + rv1escIdent(l, i) + `","content":[{"type":"output_text","text":"a"}]}`
			})
		})
		add("many_fc_pair_callid"+sfx, func(model string, n int) []byte {
			return rvItems(model, n, func(i int) string {
				id := rv1escIdent(l, i)
				return `{"type":"function_call","call_id":"` + id + `","name":"f","arguments":"{}"},{"type":"function_call_output","call_id":"` + id + `","output":"ok"}`
			})
		})
		add("many_msg_status"+sfx, func(model string, n int) []byte {
			return rvItems(model, n, func(i int) string {
				return `{"type":"message","role":"assistant","status":"` + rv1escIdent(l, i) + `","content":[{"type":"output_text","text":"a"}]}`
			})
		})
		if l < 4096 { // not item identifiers
			continue
		}
		add("metadata_many_val"+sfx, func(model string, n int) []byte {
			return rvArray(`{"model":"`+model+`","stream":false,"store":false,"metadata":{`, `},`+q, n, func(i int) string {
				return `"k` + strconv.Itoa(i) + `":"` + rv1escIdent(l, i) + `"`
			})
		})
		add("many_tool_keys"+sfx, func(model string, n int) []byte {
			return rvArray(`{"model":"`+model+`","stream":false,"store":false,"tools":[{"type":"function","name":"f","parameters":{"type":"object","properties":{}},`, `}],`+q, n, func(i int) string {
				return `"` + rv1escIdent(l, i) + `":1`
			})
		})
		add("many_tools_name"+sfx, func(model string, n int) []byte {
			return rvArray(`{"model":"`+model+`","stream":false,"store":false,"tools":[`, `],`+q, n, func(i int) string {
				return `{"type":"function","name":"` + rv1escIdent(l, i) + `","parameters":{"type":"object","properties":{}}}`
			})
		})
	}
	for _, f := range []struct{ name, pre string }{
		{"prompt_cache_key", `"prompt_cache_key":"%s"`},
		{"safety_identifier", `"safety_identifier":"%s"`},
		{"reasoning_effort", `"reasoning":{"effort":"%s"}`},
	} {
		add("root_"+f.name+"_cap4096", func(model string, n int) []byte {
			pre := strings.Replace(f.pre, "%s", rv1escIdent(4096, 0), 1) + `,"input":"`
			return coverAdvWrap(model, pre, `"}`, n, func(n int) string { return strings.Repeat("a", max(1, n)) })
		})
	}
	return out
}
