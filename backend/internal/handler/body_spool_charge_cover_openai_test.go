//go:build unit

package handler

import (
	"context"
	"net/http"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// coverCombo is one route on one account kind.
type coverCombo struct {
	route, account string
	proto, model   string // body protocol and model
	pattern, path  string // gin route pattern and request path
	local          bool   // answered without an upstream call
	sessionBlock   bool   // cyber session block on; runs only the rr9 probe
	setup          func(t *testing.T) (http.Handler, *config.Config, *coverUpstream)
}

// coverAuth stands in for API-key auth: a key of a group on platform that
// allows images and /v1/messages dispatch; like the auth middleware it
// also puts the group in the request context.
func coverAuth(platform string) gin.HandlerFunc {
	groupID := int64(3131)
	return func(c *gin.Context) {
		group := &service.Group{ID: groupID, Platform: platform, Status: service.StatusActive, Hydrated: true,
			RateMultiplier: 1, AllowImageGeneration: true, AllowMessagesDispatch: true}
		c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{
			ID: 99, GroupID: &groupID, Group: group, User: &service.User{ID: 100, Balance: 100},
		})
		c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), ctxkey.Group, group))
		c.Set(string(middleware2.ContextKeyUser), middleware2.AuthSubject{UserID: 100, Concurrency: 0})
	}
}

// coverOpenAIAccounts: API-key force_responses (passthrough), API-key
// force_chat_completions (Responses->Chat conversion), OAuth (Codex OAuth
// transform).
var coverOpenAIAccounts = []struct{ name, kind, mode string }{
	{"apikey-force_responses", "apikey", "force_responses"},
	{"apikey-force_chat", "apikey", "force_chat_completions"},
	{"oauth", "oauth", ""},
	// rr6: API-key account with extra.openai_passthrough.
	{"apikey-passthrough", "apikey_passthrough", ""},
}

type coverOpenAIRoute struct {
	route, proto, pattern string
	serve                 func(h *OpenAIGatewayHandler) gin.HandlerFunc
}

func coverOpenAICombos() []coverCombo {
	routes := []coverOpenAIRoute{
		{"v1/responses", "responses", "/v1/responses", func(h *OpenAIGatewayHandler) gin.HandlerFunc { return h.Responses }},
		{"responses", "responses", "/responses", func(h *OpenAIGatewayHandler) gin.HandlerFunc { return h.Responses }},
		{"codex/responses", "responses", "/backend-api/codex/responses", func(h *OpenAIGatewayHandler) gin.HandlerFunc { return h.Responses }},
		{"chat/completions", "chat", "/v1/chat/completions", func(h *OpenAIGatewayHandler) gin.HandlerFunc { return h.ChatCompletions }},
		{"v1/messages", "claude", "/v1/messages", func(h *OpenAIGatewayHandler) gin.HandlerFunc { return h.Messages }},
		{"count_tokens", "claude", "/v1/messages/count_tokens", func(h *OpenAIGatewayHandler) gin.HandlerFunc { return h.CountTokens }},
		// rr6: the compact subpath.
		{"v1/responses/compact", "responses", "/v1/responses/compact", func(h *OpenAIGatewayHandler) gin.HandlerFunc { return h.Responses }},
	}
	var out []coverCombo
	for _, rt := range routes {
		for _, acct := range coverOpenAIAccounts {
			out = append(out, coverOpenAICombo(rt, acct.name, acct.kind, acct.mode, false))
		}
	}
	out = append(out, rr6GuardCombos(false)...)
	// rr9: the Responses routes again with the cyber session block on;
	// these run only the escaped first-user probe.
	for _, rt := range routes {
		if rt.proto != "responses" {
			continue
		}
		for _, acct := range coverOpenAIAccounts {
			out = append(out, coverOpenAICombo(rt, coverSessionBlockPrefix+acct.name, acct.kind, acct.mode, true))
		}
	}
	out = append(out, rr6GuardCombos(true)...)
	// rr6: failover from a passthrough account (502) to a plain API-key
	// force_responses account.
	out = append(out, coverCombo{
		route: "v1/responses", account: "failover-passthrough-to-plain", proto: "responses", model: "gpt-5.5",
		pattern: "/v1/responses", path: "/v1/responses",
		setup: func(t *testing.T) (http.Handler, *config.Config, *coverUpstream) {
			up := &coverUpstream{failAuth: "sk-pass"}
			accounts := largeBodyTestAccounts("apikey")
			accounts[0].Extra["openai_passthrough"] = true
			accounts[0].Credentials = map[string]any{"api_key": "sk-pass", "base_url": "https://api.openai.com"}
			accounts[1].Extra["openai_responses_mode"] = "force_responses"
			h := newLargeBodyTestHandlerWithAccounts(t, up, accounts)
			enableBodySpoolForTest(t, h.cfg, true, 8<<30, nil)
			r := gin.New()
			r.POST("/v1/responses", coverAuth(service.PlatformOpenAI), BodySpoolIngest(h.cfg), h.Responses)
			return r, h.cfg, up
		},
	})
	// Grok count_tokens: a local estimate (Anthropic -> Responses), no
	// upstream call (review rr5: found by the static check).
	out = append(out, coverCombo{
		route: "grok/count_tokens", account: "local", proto: "claude", model: "grok-4",
		pattern: "/v1/messages/count_tokens", path: "/v1/messages/count_tokens", local: true,
		setup: func(t *testing.T) (http.Handler, *config.Config, *coverUpstream) {
			up := &coverUpstream{}
			h := newLargeBodyTestHandlerWithAccounts(t, up, largeBodyTestAccounts("apikey"))
			enableBodySpoolForTest(t, h.cfg, true, 8<<30, nil)
			r := gin.New()
			r.POST("/v1/messages/count_tokens", coverAuth(service.PlatformGrok), BodySpoolIngest(h.cfg), h.GrokCountTokens)
			return r, h.cfg, up
		},
	})
	return out
}

// coverOpenAICombo is route rt on an account of kind and Responses mode;
// sessionBlock turns the cyber session block on (coverSessionBlockDeps).
func coverOpenAICombo(rt coverOpenAIRoute, name, kind, mode string, sessionBlock bool) coverCombo {
	return coverCombo{
		route: rt.route, account: name, proto: rt.proto, model: "gpt-5.5",
		pattern: rt.pattern, path: rt.pattern, sessionBlock: sessionBlock,
		setup: func(t *testing.T) (http.Handler, *config.Config, *coverUpstream) {
			up := &coverUpstream{}
			accounts := largeBodyTestAccounts(kind)
			for i := range accounts {
				if mode != "" {
					accounts[i].Extra["openai_responses_mode"] = mode
				}
			}
			h := newCoverHandler(t, up, accounts, sessionBlock)
			enableBodySpoolForTest(t, h.cfg, true, 8<<30, nil)
			r := gin.New()
			r.POST(rt.pattern, coverAuth(service.PlatformOpenAI), BodySpoolIngest(h.cfg), rt.serve(h))
			return r, h.cfg, up
		},
	}
}
