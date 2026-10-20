package service

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai_compat"
	"github.com/gin-gonic/gin"
)

// resolveUpstreamProtocol 决定 OpenAI 网关本次请求发往上游的协议
// （chat_completions / responses / anthropic），是 /v1/responses、
// /v1/chat/completions、/v1/messages 三个入站入口共用的唯一判定。
// 只做判断，不改写请求体、不发请求；各入口再按「入站 × 上游」选择转换路径。
//
// inbound 为入站协议；/v1/chat/completions 收到 Responses 形状请求体时按
// responses 计。model 仅按模型分流的账号使用，取 upstreamRoutingModel 的结果。
// catalog 为上游模型目录给出的该模型支持的协议（见 model_protocol_catalog.go），未知时为 nil。
//
// 优先级：
//  1. 按模型分流的供应商（多模型聚合平台，如 OpenCode、Command Code）：显式
//     api_protocol 直接生效；否则先得到该模型支持的协议集合（见 modelProtocolSet），
//     入站协议在集合中时同协议直通，不在时走集合首项（首选协议）。
//  2. adaptive（按入站协议分流）：供应商提供入站协议的原生端点则走同协议，
//     否则回落 Chat Completions。
//  3. 显式 anthropic 协议 → Anthropic。
//  4. shouldForwardOpenAIResponsesViaRawChatCompletions（显式 chat_completions、
//     探针确认不支持 Responses 等）→ Chat Completions。
//  5. 其余 → Responses。
//
// inbound_native 账号（openai_responses_mode=inbound_native）在第 3 步之后判定：Chat
// Completions 与 Messages 入站永不转为 Responses，Responses 入站仅在上游支持时直通。
//
// Grok 在 Responses / Chat Completions 入口有专属链路，须在调用本函数前分流。
func resolveUpstreamProtocol(account *Account, inbound, model string, catalog []string) string {
	if account.routesByModel() {
		return account.resolveModelRoutedProtocolFor(model, inbound, catalog)
	}
	if account.IsAdaptiveAPIProtocol() {
		if account.providerSupportsProtocol(inbound) {
			return inbound
		}
		return APIProtocolChatCompletions
	}
	if account.IsAnthropicProtocol() {
		return APIProtocolAnthropic
	}
	if account.inboundNativeRouting() {
		return resolveInboundNativeProtocol(account, inbound, model)
	}
	if shouldForwardOpenAIResponsesViaRawChatCompletions(account) {
		return APIProtocolChatCompletions
	}
	return APIProtocolResponses
}

// inboundNativeRouting 报告 OpenAI API Key 账号是否启用 inbound_native 按入站协议直通。
func (a *Account) inboundNativeRouting() bool {
	return a != nil && a.Type == AccountTypeAPIKey && a.Platform == PlatformOpenAI &&
		openai_compat.InboundNativeRoutingEnabled(a.Extra)
}

// resolveInboundNativeProtocol 决定 inbound_native 账号的上游协议，全部为原生端点，不做任何
// 协议转换：Chat Completions 入站走 /v1/chat/completions；Messages 入站仅在配置了
// api_base_urls.anthropic 时走该基址的 /v1/messages；Responses 入站仅在上游支持
// /v1/responses 时走原生 Responses。无原生端点时返回空串，由入口报错（见
// nativeInboundUnavailable），绝不回落到转换路径。空入站（连接测试）优先原生 Responses，
// 上游不支持时用原生 Chat Completions 探测。
func resolveInboundNativeProtocol(account *Account, inbound, model string) string {
	switch inbound {
	case APIProtocolChatCompletions:
		if account.inboundNativeProtocolAllowed(model, APIProtocolChatCompletions) {
			return APIProtocolChatCompletions
		}
		return ""
	case APIProtocolAnthropic:
		if account.nativeAnthropicBaseURL() != "" && account.inboundNativeProtocolAllowed(model, APIProtocolAnthropic) {
			return APIProtocolAnthropic
		}
		return ""
	case APIProtocolResponses:
		if openai_compat.ShouldUseResponsesAPI(account.Extra) && account.inboundNativeProtocolAllowed(model, APIProtocolResponses) {
			return APIProtocolResponses
		}
		return ""
	}
	if openai_compat.ShouldUseResponsesAPI(account.Extra) && account.inboundNativeProtocolAllowed(model, APIProtocolResponses) {
		return APIProtocolResponses
	}
	return APIProtocolChatCompletions
}

// nativeInboundUnavailable 报告 inbound_native 账号上该入站协议对该上游模型是否没有原生端点。
// 为真时入口必须报错，不能继续走任何转换路径。
func nativeInboundUnavailable(account *Account, inbound, model string) bool {
	return account.inboundNativeRouting() && resolveInboundNativeProtocol(account, inbound, model) == ""
}

// inboundNativeProtocolAllowed 报告上游模型 model 是否允许把 protocol 当作原生端点。未列出的
// 模型不受限制（沿用账号级规则）；列出的模型只允许列出的协议；配置格式错误或歧义时拒绝全部。
func (a *Account) inboundNativeProtocolAllowed(model, protocol string) bool {
	protocols, restricted := a.inboundNativeModelProtocols(model)
	if !restricted {
		return true
	}
	return slices.Contains(protocols, protocol)
}

// inboundNativeModelProtocols 读取 inbound_native_protocols 中该模型的协议列表（模型名大小写不敏感）。
// 第二个返回值为 false 表示该模型没有限制；为 true 且列表为空表示全部拒绝（失败关闭）。
func (a *Account) inboundNativeModelProtocols(model string) ([]string, bool) {
	if a == nil {
		return nil, false
	}
	model = strings.TrimSpace(model)
	raw, present := a.Extra[openai_compat.ExtraKeyInboundNativeProtocols]
	if !present || model == "" {
		return nil, false
	}
	byModel, ok := raw.(map[string]any)
	if !ok {
		return nil, true
	}
	var matched []any
	matches := 0
	for name, value := range byModel {
		if strings.EqualFold(strings.TrimSpace(name), model) {
			matches++
			matched = []any{value}
		}
	}
	switch {
	case matches == 0:
		return nil, false
	case matches > 1:
		return nil, true
	}
	return anyToProtocolList(matched[0]), true
}

// anyToProtocolList 把配置里的协议数组转成字符串列表；格式不对时返回 nil（失败关闭）。
func anyToProtocolList(value any) []string {
	switch list := value.(type) {
	case []string:
		return list
	case []any:
		out := make([]string, 0, len(list))
		for _, item := range list {
			s, ok := item.(string)
			if !ok {
				return nil
			}
			out = append(out, strings.TrimSpace(s))
		}
		return out
	}
	return nil
}

// writeInboundNativeUnavailableError 写出无原生端点时的协议错误，并返回对应的 Go 错误。
func writeInboundNativeUnavailableError(c *gin.Context, inbound string) error {
	msg := fmt.Sprintf("this account has no native %s endpoint; protocol conversion is disabled for it", inbound)
	if inbound == APIProtocolAnthropic {
		writeAnthropicError(c, http.StatusBadRequest, "invalid_request_error", msg)
	} else {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{
			"type":    "invalid_request_error",
			"code":    "native_endpoint_unavailable",
			"message": msg,
		}})
	}
	return fmt.Errorf("inbound native endpoint unavailable: %s", inbound)
}

// nativeAnthropicBaseURL 返回 inbound_native 账号 api_base_urls.anthropic 配置的原生
// Anthropic 基址；非该模式的账号返回空串。
func (a *Account) nativeAnthropicBaseURL() string {
	if !a.inboundNativeRouting() {
		return ""
	}
	baseURLs, ok := a.Credentials["api_base_urls"].(map[string]any)
	if !ok {
		return ""
	}
	baseURL, _ := baseURLs[APIProtocolAnthropic].(string)
	return strings.TrimSpace(baseURL)
}

// resolveModelRoutedProtocol 返回按模型分流账号对该模型的首选上游协议（不考虑入站
// 协议与上游模型目录），供连接测试等没有入站协议的场景使用。
func (a *Account) resolveModelRoutedProtocol(model string) string {
	return a.resolveModelRoutedProtocolFor(model, "", nil)
}

// resolveModelRoutedProtocolFor 按模型名解析按模型分流账号的上游协议：显式 pinned
// 协议直接生效；否则取该模型支持的协议集合，入站协议在其中时同协议直通（省去一次
// 协议转换），不在时走首选协议。
func (a *Account) resolveModelRoutedProtocolFor(model, inbound string, catalog []string) string {
	switch proto := a.GetAPIProtocol(); proto {
	case APIProtocolChatCompletions, APIProtocolAnthropic, APIProtocolResponses:
		return proto
	}
	set := a.modelProtocolSet(model, catalog)
	if slices.Contains(set, inbound) {
		return inbound
	}
	return set[0]
}

// modelProtocolSet 返回模型支持的上游协议集合（首项为首选），来源依次为：
//  1. 账号 protocol_rules 中命中的规则（管理员显式配置）；
//  2. 上游模型目录（catalog）：首选沿用内置规则对该模型的首选（在集合内时），否则取目录首项；
//  3. profile 内置规则——仅在账号未配置 protocol_rules 时，已配置但未命中视为有意不走内置规则；
//  4. Chat Completions，避免落入 Responses 转换链。
func (a *Account) modelProtocolSet(model string, catalog []string) []string {
	rules, configured := a.configuredProtocolRules()
	if configured {
		if set, ok := matchProtocolRuleSet(model, rules); ok {
			return set
		}
	}
	profileRules := a.providerProfile().Endpoints(a.GetCredential("account_mode")).ProtocolRules
	if set := a.supportedCatalogProtocols(catalog); len(set) > 0 {
		preferred := matchProtocolRules(model, profileRules)
		if slices.Contains(set, preferred) {
			return normalizeProtocolSet(preferred, set)
		}
		return set
	}
	if !configured {
		if set, ok := matchProtocolRuleSet(model, profileRules); ok {
			return set
		}
	}
	return []string{APIProtocolChatCompletions}
}

// supportedCatalogProtocols 过滤出账号接入模式确有原生端点的目录协议。
func (a *Account) supportedCatalogProtocols(catalog []string) []string {
	out := make([]string, 0, len(catalog))
	for _, protocol := range catalog {
		if isNativeUpstreamProtocol(protocol) && a.providerSupportsProtocol(protocol) && !slices.Contains(out, protocol) {
			out = append(out, protocol)
		}
	}
	return out
}

// upstreamRoutingModel 返回参与协议分流的模型名：仅按模型分流的账号需要，按账号
// 模型映射与上游归一后的名字匹配规则；其余账号的分流与模型无关，返回空串。
func upstreamRoutingModel(account *Account, body []byte, defaultMappedModel string) string {
	// inbound_native 账号按上游模型名做逐模型的协议限制（见 inbound_native_protocols）。
	if !account.routesByModel() && !account.inboundNativeRouting() {
		return ""
	}
	return resolveMappedUpstreamModel(account, body, defaultMappedModel)
}

// convertResponsesShapedChatBody 把发到 /v1/chat/completions 的 Responses 形状
// 请求体（Cursor 兼容）转换为 Chat Completions 请求体。
func (s *OpenAIGatewayService) convertResponsesShapedChatBody(body []byte) ([]byte, error) {
	var responsesReq apicompat.ResponsesRequest
	if err := json.Unmarshal(body, &responsesReq); err != nil {
		return nil, fmt.Errorf("parse responses-shaped chat completions request: %w", err)
	}
	chatReq, err := apicompat.ResponsesToChatCompletionsRequestWithOptions(
		&responsesReq,
		&apicompat.ResponsesToChatOptions{ReasoningContentByID: s.reasoningContentByID},
	)
	if err != nil {
		return nil, fmt.Errorf("convert responses-shaped chat completions request: %w", err)
	}
	chatBody, err := json.Marshal(chatReq)
	if err != nil {
		return nil, fmt.Errorf("marshal converted chat completions request: %w", err)
	}
	return chatBody, nil
}
