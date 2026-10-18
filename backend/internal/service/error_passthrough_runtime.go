package service

import (
	"github.com/Wei-Shaw/sub2api/internal/model"
	"github.com/gin-gonic/gin"
)

const errorPassthroughServiceContextKey = "error_passthrough_service"

// BindErrorPassthroughService 将错误透传服务绑定到请求上下文，供 service 层在非 failover 场景下复用规则。
func BindErrorPassthroughService(c *gin.Context, svc *ErrorPassthroughService) {
	if c == nil || svc == nil {
		return
	}
	c.Set(errorPassthroughServiceContextKey, svc)
}

func getBoundErrorPassthroughService(c *gin.Context) *ErrorPassthroughService {
	if c == nil {
		return nil
	}
	v, ok := c.Get(errorPassthroughServiceContextKey)
	if !ok {
		return nil
	}
	svc, ok := v.(*ErrorPassthroughService)
	if !ok {
		return nil
	}
	return svc
}

// applyErrorPassthroughRule 按规则改写错误响应；未命中时返回默认响应参数。
func applyErrorPassthroughRule(
	c *gin.Context,
	platform string,
	upstreamStatus int,
	responseBody []byte,
	defaultStatus int,
	defaultErrType string,
	defaultErrMsg string,
) (status int, errType string, errMsg string, matched bool) {
	return applyErrorPassthroughRuleWithMessage(
		c, platform, upstreamStatus, responseBody, defaultStatus, defaultErrType, defaultErrMsg, ExtractUpstreamErrorMessage,
	)
}

// applyErrorPassthroughRuleWithMessage 同 applyErrorPassthroughRule，但由调用方提供
// 上游消息提取器（如 Grok 媒体路径需识别 xAI 扁平错误体）。
func applyErrorPassthroughRuleWithMessage(
	c *gin.Context,
	platform string,
	upstreamStatus int,
	responseBody []byte,
	defaultStatus int,
	defaultErrType string,
	defaultErrMsg string,
	extractMessage func([]byte) string,
) (status int, errType string, errMsg string, matched bool) {
	status, errType, errMsg, rule := applyErrorPassthroughRuleWithRuleAndMessage(
		c, platform, upstreamStatus, responseBody, defaultStatus, defaultErrType, defaultErrMsg, extractMessage,
	)
	return status, errType, errMsg, rule != nil
}

// applyErrorPassthroughRuleWithRule 与 applyErrorPassthroughRule 相同，额外返回命中的规则（未命中为 nil），
// 供需要按规则选项（如 passthrough_body）细化响应体的调用方使用。
func applyErrorPassthroughRuleWithRule(
	c *gin.Context,
	platform string,
	upstreamStatus int,
	responseBody []byte,
	defaultStatus int,
	defaultErrType string,
	defaultErrMsg string,
) (status int, errType string, errMsg string, rule *model.ErrorPassthroughRule) {
	return applyErrorPassthroughRuleWithRuleAndMessage(
		c, platform, upstreamStatus, responseBody, defaultStatus, defaultErrType, defaultErrMsg, ExtractUpstreamErrorMessage,
	)
}

// applyErrorPassthroughRuleWithRuleAndMessage 是规则匹配与改写的公共实现：
// 由调用方提供上游消息提取器，并返回命中的规则（未命中为 nil）。
func applyErrorPassthroughRuleWithRuleAndMessage(
	c *gin.Context,
	platform string,
	upstreamStatus int,
	responseBody []byte,
	defaultStatus int,
	defaultErrType string,
	defaultErrMsg string,
	extractMessage func([]byte) string,
) (status int, errType string, errMsg string, rule *model.ErrorPassthroughRule) {
	status = defaultStatus
	errType = defaultErrType
	errMsg = defaultErrMsg

	svc := getBoundErrorPassthroughService(c)
	if svc == nil {
		return status, errType, errMsg, nil
	}

	rule = svc.MatchRule(platform, upstreamStatus, responseBody)
	if rule == nil {
		return status, errType, errMsg, nil
	}

	status = upstreamStatus
	if !rule.PassthroughCode && rule.ResponseCode != nil {
		status = *rule.ResponseCode
	}

	errMsg = extractMessage(responseBody)
	if !rule.PassthroughBody && rule.CustomMessage != nil {
		errMsg = *rule.CustomMessage
	}

	// 命中 skip_monitoring 时在 context 中标记，供 ops_error_logger 跳过记录。
	if rule.SkipMonitoring {
		c.Set(OpsSkipPassthroughKey, true)
	}

	// 与现有 failover 场景保持一致：命中规则时默认返回 upstream_error；
	// OpenAI 路径在 passthrough_body=true 时会改用上游 type/code/param（见 writeOpenAIPassthroughRuleBodyError）。
	errType = "upstream_error"
	return status, errType, errMsg, rule
}
