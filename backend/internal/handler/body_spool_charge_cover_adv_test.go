//go:build unit

package handler

import (
	"bytes"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Adversarial corpus for the charge-covers-allocation test (rr10 item 2):
// string and number content chosen to make a decode, a canonical form or
// a number parse cost the most per body byte. Shapes are named adv_*.
//
//	adv_<field>_<esc>           one body-sized string of escape unit esc in
//	                            field: input (string form), instructions,
//	                            developer / system / user content (string
//	                            and input_text part), a tool description
//	adv_items_<role>_<size>_<esc>  many items of role, each one string of
//	                            esc of 4 KiB or 32 KiB, filling the body
//	adv_num_<field>_<form>      one body-sized number literal in a root
//	                            number field (int: 1000..., frac: 0.000...1)
//	adv_leaf_<gen shape>_<path> every string or number leaf of every
//	                            schema-driven shape (coverGenMods) made
//	                            body-sized: strings of coverAdvMixUnit,
//	                            numbers as an int literal
//
// Escape units (JSON text): lt '<' (json.Marshal writes 6 bytes for it),
// quote \", uesc \u00e9, surr two invalid surrogates (\udc00\ud800, each
// decodes to U+FFFD).
//
// coverAdvAlwaysOn run with SUB2API_CHARGE_COVER=1; the rest also need
// SUB2API_CHARGE_COVER_GEN=1. The adv_* shapes run on every Responses
// route × account kind, with the guard on and off and the session block
// on and off (coverAdvCombo), at 2, 8 and 16 MiB; adv_leaf_* on the plain
// combos (coverAdvShapeOn).
var coverAdvEscapes = []struct{ name, unit string }{
	{"lt", "<"},
	{"quote", `\"`},
	{"uesc", `\u00e9`},
	{"surr", `\udc00\ud800`},
}

// coverAdvMixUnit: every escape kind in one unit (adv_leaf_*).
const coverAdvMixUnit = `<\"\u00e9\udc00\ud800 `

// coverAdvAlwaysOn: the reviewer's rr9 findings and one shape per field
// kind; they run without SUB2API_CHARGE_COVER_GEN.
var coverAdvAlwaysOn = map[string]bool{
	"adv_items_developer_32k_lt":    true, // rr9 blocker 1
	"adv_items_developer_4k_lt":     true,
	"adv_items_user_4k_quote":       true, // transcript: every item
	"adv_input_string_uesc":         true, // rr9: the seed's |input= path
	"adv_instructions_quote":        true, // rr9: escaped instructions
	"adv_developer_lt":              true,
	"adv_tool_description_lt":       true,
	"adv_num_max_output_tokens_int": true, // rr9: strconv copies the literal
	"adv_num_temperature_frac":      true,
}

// coverAdvShape is one adversarial body generator.
type coverAdvShape struct {
	name string
	body func(model string, n int) []byte
}

// coverAdvRepeat is unit repeated to about n bytes (whole units).
func coverAdvRepeat(unit string, n int) string {
	return strings.Repeat(unit, max(1, n/len(unit)))
}

// coverAdvWrap renders a Responses body around pre + big + post, with big
// sized so the body is about n bytes.
func coverAdvWrap(model, pre, post string, n int, big func(n int) string) []byte {
	head := `{"model":"` + model + `","stream":false,"store":false,`
	var b bytes.Buffer
	b.Grow(n + 1024)
	b.WriteString(head)
	b.WriteString(pre)
	b.WriteString(big(n - len(head) - len(pre) - len(post) - 64))
	b.WriteString(post)
	return b.Bytes()
}

// coverAdvItems fills the input array with item(i) up to about n bytes,
// then a short last user message.
func coverAdvItems(model string, n int, item string) []byte {
	head := `{"model":"` + model + `","stream":false,"store":false,"input":[`
	tail := `{"role":"user","content":"last"}]}`
	var b bytes.Buffer
	b.Grow(n + len(item))
	b.WriteString(head)
	for b.Len()+len(item)+1+len(tail) <= n {
		b.WriteString(item)
		b.WriteByte(',')
	}
	b.WriteString(tail)
	return b.Bytes()
}

func coverAdvShapes() []coverAdvShape {
	var out []coverAdvShape
	q := `"input":[{"role":"user","content":"q"}]}`
	for _, e := range coverAdvEscapes {
		str := func(n int) string { return coverAdvRepeat(e.unit, n) }
		field := func(name, pre, post string) {
			out = append(out, coverAdvShape{name: "adv_" + name + "_" + e.name, body: func(model string, n int) []byte {
				return coverAdvWrap(model, pre, post, n, str)
			}})
		}
		field("input_string", `"input":"`, `"}`)
		field("instructions", `"instructions":"`, `",`+q)
		for _, role := range []string{"developer", "system", "user"} {
			field(role, `"input":[{"role":"`+role+`","content":"`, `"},{"role":"user","content":"q"}]}`)
			field(role+"_part", `"input":[{"role":"`+role+`","content":[{"type":"input_text","text":"`, `"}]},{"role":"user","content":"q"}]}`)
		}
		field("tool_description", `"tools":[{"type":"function","name":"f","description":"`,
			`","parameters":{"type":"object","properties":{}}}],`+q)
		for _, role := range []string{"system", "developer", "user"} {
			for _, sz := range []struct {
				name string
				n    int
			}{{"4k", 4 << 10}, {"32k", 32 << 10}} {
				item := `{"role":"` + role + `","content":"` + coverAdvRepeat(e.unit, sz.n-40) + `"}`
				out = append(out, coverAdvShape{name: "adv_items_" + role + "_" + sz.name + "_" + e.name, body: func(model string, n int) []byte {
					return coverAdvItems(model, n, item)
				}})
			}
		}
	}
	nums := map[string]func(n int) string{
		"int":  func(n int) string { return "1" + strings.Repeat("0", max(1, n-1)) },
		"frac": func(n int) string { return "0." + strings.Repeat("0", max(1, n-3)) + "1" },
	}
	for _, f := range []string{"max_output_tokens", "max_tokens", "temperature", "top_p", "top_logprobs", "max_tool_calls"} {
		for form, digits := range nums {
			out = append(out, coverAdvShape{name: "adv_num_" + f + "_" + form, body: func(model string, n int) []byte {
				return coverAdvWrap(model, `"`+f+`":`, ","+q, n, digits)
			}})
		}
	}
	out = append(out, coverAdvLeafShapes()...)
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

// coverAdvLeafShapes: for each schema-driven shape (coverGenMods) and each
// string or number leaf of its item or root members, the shape with that
// leaf body-sized. "type" and "role" leaves are kept: they select the
// shape.
func coverAdvLeafShapes() []coverAdvShape {
	const placeholder = `"@@cover-adv-leaf@@"`
	var out []coverAdvShape
	for _, m := range coverGenMods() {
		doc, isRoot := m.item, false
		if doc == "" {
			doc, isRoot = "{"+strings.TrimSuffix(m.root, ",")+"}", true
		}
		var leaves []struct {
			path string
			num  bool
		}
		var walk func(prefix string, v gjson.Result)
		walk = func(prefix string, v gjson.Result) {
			v.ForEach(func(k, child gjson.Result) bool {
				key := k.String()
				if k.Type == gjson.Number {
					key = k.Raw
				}
				path := key
				if prefix != "" {
					path = prefix + "." + key
				}
				switch child.Type {
				case gjson.String:
					if key != "type" && key != "role" {
						leaves = append(leaves, struct {
							path string
							num  bool
						}{path, false})
					}
				case gjson.Number:
					leaves = append(leaves, struct {
						path string
						num  bool
					}{path, true})
				case gjson.JSON:
					walk(path, child)
				}
				return true
			})
		}
		walk("", gjson.Parse(doc))
		for _, lf := range leaves {
			tmpl, err := sjson.SetRaw(doc, lf.path, placeholder)
			if err != nil {
				continue
			}
			pre, post, _ := strings.Cut(tmpl, placeholder)
			big := func(n int) string { return `"` + coverAdvRepeat(coverAdvMixUnit, n) + `"` }
			if lf.num {
				big = func(n int) string { return "1" + strings.Repeat("0", max(1, n-1)) }
			}
			name := "adv_leaf_" + strings.TrimPrefix(m.name, "gen_") + "_" + strings.ReplaceAll(lf.path, ".", "_")
			if isRoot {
				// pre/post are inside {...}: drop the braces and splice into the root.
				pre, post = pre[1:], post[:len(post)-1]
				out = append(out, coverAdvShape{name: name, body: func(model string, n int) []byte {
					return coverAdvWrap(model, pre, post+`,"input":[{"role":"user","content":"q"}]}`, n, big)
				}})
				continue
			}
			out = append(out, coverAdvShape{name: name, body: func(model string, n int) []byte {
				return coverAdvWrap(model, `"input":[`+pre, post+`,{"role":"user","content":"q"}]}`, n, big)
			}})
		}
	}
	return out
}

// coverAdvShapeOn: whether shape name runs on combo cb (coverAdvCombo).
// The always-on shapes run everywhere; with gen the explicit shapes too,
// and the per-leaf sweep (adv_leaf_*, about 220 shapes) on the plain
// combos only: the guard and the session block read message text and
// items, which the explicit shapes cover in every field.
func coverAdvShapeOn(cb coverCombo, name string, gen bool) bool {
	switch {
	case coverAdvAlwaysOn[name]:
		return true
	case !gen:
		return false
	case strings.HasPrefix(name, "adv_leaf_"):
		return !cb.sessionBlock && !strings.HasPrefix(cb.account, coverGuardPrefix)
	}
	return true
}

// coverAdvCombo: the adv_* shapes run on the four Responses routes × the
// four account kinds, each plain, with the guard, with the session block,
// and with both.
func coverAdvCombo(cb coverCombo) bool {
	switch cb.route {
	case "v1/responses", "responses", "codex/responses", "v1/responses/compact":
	default:
		return false
	}
	acct := strings.TrimPrefix(strings.TrimPrefix(cb.account, coverSessionBlockPrefix), coverGuardPrefix)
	for _, a := range coverOpenAIAccounts {
		if acct == a.name {
			return true
		}
	}
	return false
}

// TestCoverAdvShapesAreValid: every adv_* body is valid JSON of about the
// asked size, so a case measures its content and not a parse error.
func TestCoverAdvShapesAreValid(t *testing.T) {
	shapes := coverAdvShapes()
	names := map[string]bool{}
	for _, a := range shapes {
		require.False(t, names[a.name], "duplicate %s", a.name)
		names[a.name] = true
		for _, n := range []int{256 << 10, 2 << 20} {
			body := a.body("gpt-5.5", n)
			require.True(t, gjson.ValidBytes(body), "%s at %d", a.name, n)
			require.InDelta(t, n, len(body), float64(40<<10), "%s at %d", a.name, n)
		}
	}
	for name := range coverAdvAlwaysOn {
		require.True(t, names[name], "always-on %s is not generated", name)
	}
	t.Logf("adv shapes %d (always on %d)", len(shapes), len(coverAdvAlwaysOn))
}
