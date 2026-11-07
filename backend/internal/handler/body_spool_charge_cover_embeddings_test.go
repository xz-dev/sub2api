//go:build unit

package handler

import (
	"bytes"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
)

// Charge-covers-allocation for /embeddings and /rerank (M3, k_conv rows
// "embeddings" and "rerank"). Only an OpenAI group serves them (routes
// isOpenAIOnlyEndpointGatewayPlatform) and only an API-key account
// (Account.IsOpenAIEndpointCapabilitySupported); Gemini and Antigravity
// have no such route. The handlers read the body whole, validate it with
// gjson, read the model, and forward it as is, rewritten with sjson when
// the model is mapped and into the nested shape for a Bailian text-rerank
// model.

// coverEmbedShape is one body shape; body builds about n bytes.
type coverEmbedShape struct {
	name    string
	chunked bool
	body    func(model string, n int) []byte
}

// coverEmbedFill writes items until the body reaches about n bytes:
// prefix, then item(i) joined by commas, then suffix.
func coverEmbedFill(prefix, suffix string, n int, item func(i int) string) []byte {
	var b bytes.Buffer
	b.Grow(n + 256)
	b.WriteString(prefix)
	for i := 0; b.Len()+len(suffix) < n; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(item(i))
	}
	b.WriteString(suffix)
	return b.Bytes()
}

// coverEmbedText is plain text of about n bytes.
func coverEmbedText(n int) string {
	const word = "lorem ipsum dolor sit amet "
	return strings.Repeat(word, max(1, n/len(word)))
}

// coverEmbedEscaped is JSON-escaped text of about n bytes (escapes only).
func coverEmbedEscaped(n int) string {
	const unit = `\u00e9\n\"\\`
	return strings.Repeat(unit, max(1, n/len(unit)))
}

func coverEmbeddingShapes() []coverEmbedShape {
	head := func(model string) string { return `{"model":"` + model + `","input":` }
	return []coverEmbedShape{
		{name: "emb_string", body: func(m string, n int) []byte {
			return []byte(head(m) + `"` + coverEmbedText(n) + `"}`)
		}},
		{name: "emb_string_chunked", chunked: true, body: func(m string, n int) []byte {
			return []byte(head(m) + `"` + coverEmbedText(n) + `"}`)
		}},
		{name: "emb_escaped", body: func(m string, n int) []byte {
			return []byte(head(m) + `"` + coverEmbedEscaped(n) + `"}`)
		}},
		{name: "emb_strings", body: func(m string, n int) []byte {
			s := `"` + coverEmbedText(512) + `"`
			return coverEmbedFill(head(m)+`[`, `]}`, n, func(int) string { return s })
		}},
		{name: "emb_tokens", body: func(m string, n int) []byte {
			var tok bytes.Buffer
			tok.WriteByte('[')
			for i := 0; i < 256; i++ {
				if i > 0 {
					tok.WriteByte(',')
				}
				tok.WriteString(strconv.Itoa(10000 + i*37))
			}
			tok.WriteByte(']')
			s := tok.String()
			return coverEmbedFill(head(m)+`[`, `]}`, n, func(int) string { return s })
		}},
		{name: "emb_many_small", body: func(m string, n int) []byte {
			return coverEmbedFill(head(m)+`[`, `]}`, n, func(int) string { return `"ab"` })
		}},
		{name: "emb_many_small_tokens", body: func(m string, n int) []byte {
			return coverEmbedFill(head(m)+`[`, `]}`, n, func(int) string { return `[1]` })
		}},
		{name: "emb_one_big", body: func(m string, n int) []byte {
			return []byte(head(m) + `["` + coverEmbedText(n) + `"],"encoding_format":"float","dimensions":256}`)
		}},
	}
}

func coverRerankShapes() []coverEmbedShape {
	head := func(model string) string {
		return `{"model":"` + model + `","query":"what is lorem","top_n":3,"documents":`
	}
	return []coverEmbedShape{
		{name: "rr_many_docs", body: func(m string, n int) []byte {
			s := `"` + coverEmbedText(256) + `"`
			return coverEmbedFill(head(m)+`[`, `],"return_documents":false}`, n, func(int) string { return s })
		}},
		{name: "rr_many_docs_chunked", chunked: true, body: func(m string, n int) []byte {
			s := `"` + coverEmbedText(256) + `"`
			return coverEmbedFill(head(m)+`[`, `]}`, n, func(int) string { return s })
		}},
		{name: "rr_many_small_docs", body: func(m string, n int) []byte {
			return coverEmbedFill(head(m)+`[`, `]}`, n, func(int) string { return `"ab"` })
		}},
		{name: "rr_object_docs", body: func(m string, n int) []byte {
			s := `{"text":"` + coverEmbedText(128) + `"}`
			return coverEmbedFill(head(m)+`[`, `]}`, n, func(int) string { return s })
		}},
		{name: "rr_one_big_doc", body: func(m string, n int) []byte {
			return []byte(head(m) + `["` + coverEmbedText(n) + `"]}`)
		}},
		{name: "rr_escaped_docs", body: func(m string, n int) []byte {
			s := `"` + coverEmbedEscaped(256) + `"`
			return coverEmbedFill(head(m)+`[`, `]}`, n, func(int) string { return s })
		}},
		{name: "rr_big_query", body: func(m string, n int) []byte {
			return []byte(`{"model":"` + m + `","query":"` + coverEmbedText(n) + `","documents":["a","b"]}`)
		}},
	}
}

// coverEmbedCombo is one route on one account variant.
type coverEmbedCombo struct {
	route, account, model, path string
	rerank                      bool
	setup                       func(t *testing.T) (http.Handler, *config.Config, *coverUpstream)
}

func coverEmbedCombos() []coverEmbedCombo {
	type acct struct {
		name, kind, model string
		mapping           map[string]any // credentials model_mapping
		guard             bool
	}
	embAccounts := []acct{
		{name: "apikey", kind: "apikey_plain", model: "text-embedding-3-large"},
		{name: "apikey-passthrough", kind: "apikey_passthrough", model: "text-embedding-3-large"},
		{name: "apikey-mapped", kind: "apikey_plain", model: "emb-alias", mapping: map[string]any{"emb-alias": "text-embedding-3-large"}},
		{name: "guard-apikey", kind: "apikey_plain", model: "text-embedding-3-large", guard: true},
	}
	rrAccounts := []acct{
		{name: "apikey", kind: "apikey_plain", model: "bge-reranker-v2-m3"},
		{name: "apikey-passthrough", kind: "apikey_passthrough", model: "bge-reranker-v2-m3"},
		{name: "apikey-mapped", kind: "apikey_plain", model: "rr-alias", mapping: map[string]any{"rr-alias": "bge-reranker-v2-m3"}},
		// Bailian text-rerank: the body is rebuilt in the nested shape.
		{name: "apikey-bailian", kind: "apikey_plain", model: "qwen3.7-text-rerank"},
		{name: "apikey-bailian-mapped", kind: "apikey_plain", model: "rr-alias", mapping: map[string]any{"rr-alias": "qwen3.7-text-rerank"}},
		{name: "guard-apikey", kind: "apikey_plain", model: "bge-reranker-v2-m3", guard: true},
	}
	build := func(route, path string, rerank bool, a acct) coverEmbedCombo {
		return coverEmbedCombo{route: route, account: a.name, model: a.model, path: path, rerank: rerank,
			setup: func(t *testing.T) (http.Handler, *config.Config, *coverUpstream) {
				up := &coverUpstream{}
				accounts := largeBodyTestAccounts(a.kind)
				for i := range accounts {
					if a.mapping != nil {
						accounts[i].Credentials["model_mapping"] = a.mapping
					}
				}
				h := newCoverHandler(t, up, accounts, false)
				if a.guard {
					coverGuardOn(h)
				}
				enableBodySpoolForTest(t, h.cfg, true, 8<<30, nil)
				serve := h.Embeddings
				if rerank {
					serve = h.Rerank
				}
				r := gin.New()
				r.POST(path, coverAuth(service.PlatformOpenAI), BodySpoolIngest(h.cfg), serve)
				return r, h.cfg, up
			}}
	}
	var out []coverEmbedCombo
	for _, a := range embAccounts {
		out = append(out, build("v1/embeddings", "/v1/embeddings", false, a))
	}
	out = append(out, build("embeddings", "/embeddings", false, embAccounts[0]))
	for _, a := range rrAccounts {
		out = append(out, build("v1/rerank", "/v1/rerank", true, a))
	}
	return out
}

// TestBodySpoolChargeCoversEmbeddings is TestBodySpoolChargeCoversAllocation
// for /embeddings and /rerank: every account variant and shape at 64 KiB
// (fixed overhead, logged only), 1 MiB and 4 MiB; the request is charged
// its class's k_conv and its total allocation stays within the charge.
// Set SUB2API_CHARGE_COVER=1; SUB2API_CHARGE_COVER_ONLY filters by name.
func TestBodySpoolChargeCoversEmbeddings(t *testing.T) {
	if os.Getenv("SUB2API_CHARGE_COVER") == "" {
		t.Skip("set SUB2API_CHARGE_COVER=1 to run the charge-covers-allocation property test")
	}
	gin.SetMode(gin.TestMode)
	only := os.Getenv("SUB2API_CHARGE_COVER_ONLY")
	cases := 0
	var guardRefused []string
	for _, cb := range coverEmbedCombos() {
		shapes := coverEmbeddingShapes()
		wantClass := "embeddings"
		if cb.rerank {
			shapes, wantClass = coverRerankShapes(), "rerank"
		}
		var srv *coverServer
		for _, sh := range shapes {
			for _, size := range []int{64 << 10, 1 << 20, 4 << 20} {
				name := cb.route + "/" + cb.account + "/" + sh.name + "/" + coverSize(size)
				if !coverSelected(name, only) {
					continue
				}
				if srv == nil { // one unmeasured request warms the handler
					h, cfg, up := cb.setup(t)
					srv = &coverServer{h: h, cfg: cfg, up: up}
					runCoverCase(t, *srv, cb.path, shapes[0].body(cb.model, 64<<10), false, nil)
				}
				cases++
				t.Run(name, func(t *testing.T) {
					body := sh.body(cb.model, size)
					r := runCoverCaseBody(t, *srv, cb.path, body, false, nil, sh.chunked)
					logCoverResult(t, name, r)
					if r.reply != "" {
						t.Logf("COVER %s reply %s", name, r.reply)
					}
					// The blocking guard fails closed on an input it has no
					// text for (token arrays): a 503 before the upstream,
					// listed; the charge must still cover what it allocated.
					if r.status == http.StatusServiceUnavailable && strings.Contains(r.reply, "prompt_guard_unavailable") {
						assert.Zero(t, r.calls, "refused after an upstream call")
						guardRefused = append(guardRefused, name)
					} else {
						assert.Equal(t, http.StatusOK, r.status, "route not exercised")
						assert.Positive(t, r.calls, "200 without an upstream call")
					}
					if size < 1<<20 {
						t.Logf("COVER-INFO %s fixed overhead above charge %d B (total alloc)", name, r.TotalAlloc-r.PeakCharge)
						return
					}
					assertChargeCovers(t, r.chargeCoverReading)
					// The ingest charges the class k_conv: peak charge at
					// least k x body.
					assert.GreaterOrEqual(t, float64(r.PeakCharge), kConvClasses[wantClass].k*float64(len(body))*0.99, "class %s", wantClass)
					// Sensitivity: the floor of 65 would also cover a full
					// JSON decode (k 7.30-47.60 at 1 MiB), so the total
					// allocation must also stay within ceil(measured x 1.15)
					// x body plus the allowance.
					measuredK := math.Ceil(kConvClasses[wantClass].measured * 1.15)
					assert.LessOrEqual(t, float64(r.TotalAlloc), measuredK*float64(len(body))+chargeCoverAllowance,
						"class %s: total allocation k %.2f above the measured bound %.0f", wantClass, float64(r.TotalAlloc)/float64(len(body)), measuredK)
				})
			}
		}
	}
	t.Logf("COVER embeddings/rerank cases %d, refused by the guard before the upstream %d", cases, len(guardRefused))
	for _, n := range guardRefused {
		t.Logf("COVER guard-refused %s", n)
	}
}
