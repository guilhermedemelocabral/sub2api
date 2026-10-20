//go:build unit

package service

import (
	"context"
	"net/http"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/openai_compat"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

const (
	inboundNativeChatOK      = `{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"gpt-x","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`
	inboundNativeResponsesOK = `{"id":"resp_1","object":"response","created_at":1,"model":"gpt-x","status":"completed","output":[{"type":"message","id":"msg_1","role":"assistant","status":"completed","content":[{"type":"output_text","text":"ok","annotations":[]}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`
	inboundNativeAnthropicOK = `{"id":"msg_1","type":"message","role":"assistant","model":"gpt-x","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":1,"output_tokens":1}}`

	// The legacy chat-to-responses path always streams upstream, so its fixture must be SSE
	// with a terminal response.completed event.
	inboundNativeResponsesSSE = "data: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"object\":\"response\",\"model\":\"gpt-x\",\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"id\":\"msg_1\",\"role\":\"assistant\",\"status\":\"completed\",\"content\":[{\"type\":\"output_text\",\"text\":\"ok\",\"annotations\":[]}]}],\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n" +
		"data: [DONE]\n\n"

	inboundNativeAnthropicBase = "https://anthropic.compat.example"
)

// inboundNativeAccount 构造 OpenAI API Key 账号。mode 为空表示未配置 openai_responses_mode；
// anthropicBase 为空表示未配置 api_base_urls.anthropic。
func inboundNativeAccount(responsesSupported bool, mode, anthropicBase string) *Account {
	extra := map[string]any{openai_compat.ExtraKeyResponsesSupported: responsesSupported}
	if mode != "" {
		extra[openai_compat.ExtraKeyResponsesMode] = mode
	}
	credentials := map[string]any{"api_key": "sk-test", "base_url": "https://compat.example"}
	if anthropicBase != "" {
		credentials["api_base_urls"] = map[string]any{APIProtocolAnthropic: anthropicBase}
	}
	return &Account{
		ID:          5201,
		Name:        "inbound-native-test",
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Concurrency: 1,
		Credentials: credentials,
		Extra:       extra,
		Status:      StatusActive,
		Schedulable: true,
	}
}

func TestResolveUpstreamProtocol_InboundNativeMode(t *testing.T) {
	t.Parallel()

	const (
		cc          = APIProtocolChatCompletions
		responses   = APIProtocolResponses
		anthropic   = APIProtocolAnthropic
		native      = "inbound_native"
		unavailable = "" // no native endpoint: the entry point reports an error, nothing is converted
	)
	oauth := routingTestAccount(PlatformOpenAI, AccountTypeOAuth, nil, map[string]any{
		openai_compat.ExtraKeyResponsesMode: native,
	})
	cases := []struct {
		name    string
		account *Account
		inbound string
		want    string
	}{
		{"native: chat stays on chat completions", inboundNativeAccount(true, native, inboundNativeAnthropicBase), APIProtocolChatCompletions, cc},
		{"native: chat stays on chat completions without anthropic base", inboundNativeAccount(true, native, ""), APIProtocolChatCompletions, cc},
		{"native: messages go to the anthropic native endpoint when configured", inboundNativeAccount(true, native, inboundNativeAnthropicBase), anthropic, anthropic},
		{"native: messages without anthropic base are unavailable", inboundNativeAccount(true, native, ""), anthropic, unavailable},
		{"native: responses stay native when supported", inboundNativeAccount(true, native, ""), responses, responses},
		{"native: responses are unavailable when unsupported", inboundNativeAccount(false, native, ""), responses, unavailable},
		{"native: empty inbound (connection test) prefers native responses", inboundNativeAccount(true, native, ""), "", responses},
		{"native: empty inbound falls back to native chat when responses unsupported", inboundNativeAccount(false, native, ""), "", cc},
		{"legacy (no mode): chat still converts to responses", inboundNativeAccount(true, "", ""), APIProtocolChatCompletions, responses},
		{"legacy (no mode): messages still converts to responses", inboundNativeAccount(true, "", ""), anthropic, responses},
		{"legacy (no mode, probe says no): chat direct", inboundNativeAccount(false, "", ""), APIProtocolChatCompletions, cc},
		{"force_chat_completions is unchanged", inboundNativeAccount(true, "force_chat_completions", ""), APIProtocolChatCompletions, cc},
		{"oauth account ignores the mode", oauth, APIProtocolChatCompletions, responses},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tc.want, resolveUpstreamProtocol(tc.account, tc.inbound, "", nil))
		})
	}
}

func TestInboundNativeProtocols_PerModelRestriction(t *testing.T) {
	t.Parallel()

	const (
		cc          = APIProtocolChatCompletions
		responses   = APIProtocolResponses
		anthropic   = APIProtocolAnthropic
		unavailable = ""
	)
	account := inboundNativeAccount(true, "inbound_native", inboundNativeAnthropicBase)
	account.Extra[openai_compat.ExtraKeyInboundNativeProtocols] = map[string]any{
		"step-3.7-flash": []any{cc, responses},
		"MiniMax-M3":     []any{cc, anthropic},
	}
	cases := []struct {
		name    string
		model   string
		inbound string
		want    string
	}{
		{"unlisted model keeps the account-level anthropic rule", "step-5-preview", anthropic, anthropic},
		{"unlisted model keeps the account-level responses rule", "step-5-preview", responses, responses},
		{"listed model allows its responses protocol", "step-3.7-flash", responses, responses},
		{"listed model blocks messages it does not list", "step-3.7-flash", anthropic, unavailable},
		{"listed model keeps chat when listed", "step-3.7-flash", cc, cc},
		{"model names match case-insensitively", "STEP-3.7-FLASH", anthropic, unavailable},
		{"listed model blocks responses it does not list", "minimax-m3", responses, unavailable},
		{"listed model allows messages (case-insensitive key)", "minimax-m3", anthropic, anthropic},
		{"empty model applies no restriction", "", anthropic, anthropic},
		{"connection test on an unlisted model prefers native responses", "step-5-preview", "", responses},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tc.want, resolveInboundNativeProtocol(account, tc.inbound, tc.model))
		})
	}
}

// Malformed or ambiguous restriction config must fail closed, never widen access.
func TestInboundNativeProtocols_MalformedConfigFailsClosed(t *testing.T) {
	t.Parallel()

	account := inboundNativeAccount(true, "inbound_native", inboundNativeAnthropicBase)

	account.Extra[openai_compat.ExtraKeyInboundNativeProtocols] = "responses" // not an object
	require.Empty(t, resolveInboundNativeProtocol(account, APIProtocolChatCompletions, "gpt-x"))
	require.Empty(t, resolveInboundNativeProtocol(account, APIProtocolResponses, "gpt-x"))

	account.Extra[openai_compat.ExtraKeyInboundNativeProtocols] = map[string]any{"gpt-x": "responses"} // list expected
	require.Empty(t, resolveInboundNativeProtocol(account, APIProtocolResponses, "gpt-x"))

	account.Extra[openai_compat.ExtraKeyInboundNativeProtocols] = map[string]any{
		"gpt-x": []any{APIProtocolChatCompletions, APIProtocolResponses},
		"GPT-X": []any{APIProtocolChatCompletions},
	} // two keys match the same model: ambiguous
	require.Empty(t, resolveInboundNativeProtocol(account, APIProtocolResponses, "gpt-x"))
}

func TestUpstreamRoutingModel_InboundNativeUsesUpstreamModel(t *testing.T) {
	t.Parallel()

	body := []byte(`{"model":"client-alias"}`)
	native := inboundNativeAccount(true, "inbound_native", "")
	native.Credentials["model_mapping"] = map[string]any{"client-alias": "upstream-model"}
	require.Equal(t, "upstream-model", upstreamRoutingModel(native, body, ""))

	legacy := inboundNativeAccount(true, "", "")
	legacy.Credentials["model_mapping"] = map[string]any{"client-alias": "upstream-model"}
	require.Empty(t, upstreamRoutingModel(legacy, body, ""))
}

func TestInboundNativeResponsesMode_Helpers(t *testing.T) {
	t.Parallel()

	require.Equal(t, openai_compat.ResponsesSupportModeInboundNative,
		openai_compat.NormalizeResponsesSupportMode("inbound_native"))
	require.True(t, openai_compat.InboundNativeRoutingEnabled(map[string]any{
		openai_compat.ExtraKeyResponsesMode: "inbound_native",
	}))
	require.False(t, openai_compat.InboundNativeRoutingEnabled(map[string]any{
		openai_compat.ExtraKeyResponsesMode: "force_chat_completions",
	}))
	require.False(t, openai_compat.InboundNativeRoutingEnabled(nil))

	// The mode leaves the probe flag alone: legacy callers still read it as before.
	require.True(t, openai_compat.ShouldUseResponsesAPI(map[string]any{
		openai_compat.ExtraKeyResponsesMode:      "inbound_native",
		openai_compat.ExtraKeyResponsesSupported: true,
	}))
	require.False(t, openai_compat.ShouldUseResponsesAPI(map[string]any{
		openai_compat.ExtraKeyResponsesMode:      "inbound_native",
		openai_compat.ExtraKeyResponsesSupported: false,
	}))
}

func TestInboundNativeAnthropicBase_OnlyForInboundNativeAccounts(t *testing.T) {
	t.Parallel()

	require.Equal(t, inboundNativeAnthropicBase,
		inboundNativeAccount(true, "inbound_native", inboundNativeAnthropicBase).GetAnthropicProtocolBaseURL())
	require.Empty(t, inboundNativeAccount(true, "inbound_native", "").GetAnthropicProtocolBaseURL())
	// Legacy accounts keep their current behavior even when api_base_urls.anthropic is present.
	require.Empty(t, inboundNativeAccount(true, "", inboundNativeAnthropicBase).GetAnthropicProtocolBaseURL())
}

func TestInboundNativeForward_ChatInboundUsesNativeChatCompletions(t *testing.T) {
	t.Parallel()

	body := []byte(`{"model":"gpt-x","stream":false,"messages":[{"role":"user","content":"hi"}]}`)
	upstream := &httpUpstreamRecorder{responses: []*http.Response{
		newOpenAIRejectedFieldTestResponse(http.StatusOK, inboundNativeChatOK),
	}}
	c := newOpenAIRejectedFieldTestContext(body)

	_, err := newOpenAIRejectedFieldTestService(upstream).ForwardAsChatCompletions(
		context.Background(), c, inboundNativeAccount(true, "inbound_native", ""), body, "", "",
	)

	require.NoError(t, err)
	require.Len(t, upstream.requests, 1)
	require.Equal(t, "/v1/chat/completions", upstream.requests[0].URL.Path)
	require.True(t, gjson.GetBytes(upstream.bodies[0], "messages").Exists(), "chat body must reach upstream unchanged")
	require.False(t, gjson.GetBytes(upstream.bodies[0], "input").Exists(), "chat must not become a responses body")
}

func TestInboundNativeForward_MessagesInboundUsesNativeAnthropicEndpoint(t *testing.T) {
	t.Parallel()

	body := []byte(`{"model":"gpt-x","max_tokens":16,"stream":false,"messages":[{"role":"user","content":"hi"}]}`)
	upstream := &httpUpstreamRecorder{responses: []*http.Response{
		newOpenAIRejectedFieldTestResponse(http.StatusOK, inboundNativeAnthropicOK),
	}}
	c := newOpenAIRejectedFieldTestContext(body)

	_, err := newOpenAIRejectedFieldTestService(upstream).ForwardAsAnthropic(
		context.Background(), c, inboundNativeAccount(true, "inbound_native", inboundNativeAnthropicBase), body, "", "",
	)

	require.NoError(t, err)
	require.Len(t, upstream.requests, 1)
	require.Equal(t, "anthropic.compat.example", upstream.requests[0].URL.Host)
	require.Equal(t, "/v1/messages", upstream.requests[0].URL.Path)
	require.Equal(t, "sk-test", upstream.requests[0].Header.Get("x-api-key"), "native Anthropic auth")
	require.True(t, gjson.GetBytes(upstream.bodies[0], "messages").Exists(), "messages body must pass through")
	require.False(t, gjson.GetBytes(upstream.bodies[0], "input").Exists(), "messages must not become a responses body")
}

func TestInboundNativeForward_MessagesWithoutAnthropicEndpointFailsWithoutConverting(t *testing.T) {
	t.Parallel()

	body := []byte(`{"model":"gpt-x","max_tokens":16,"stream":false,"messages":[{"role":"user","content":"hi"}]}`)
	upstream := &httpUpstreamRecorder{}
	c := newOpenAIRejectedFieldTestContext(body)

	_, err := newOpenAIRejectedFieldTestService(upstream).ForwardAsAnthropic(
		context.Background(), c, inboundNativeAccount(true, "inbound_native", ""), body, "", "",
	)

	require.Error(t, err)
	require.Empty(t, upstream.requests, "no upstream request may be sent when the native endpoint is missing")
	require.Equal(t, http.StatusBadRequest, c.Writer.Status())
}

// A model that does not list "anthropic" must never be sent to chat as a substitute.
func TestInboundNativeForward_RestrictedMessagesModelNeverFallsBackToChat(t *testing.T) {
	t.Parallel()

	account := inboundNativeAccount(true, "inbound_native", inboundNativeAnthropicBase)
	account.Extra[openai_compat.ExtraKeyInboundNativeProtocols] = map[string]any{
		"gpt-x": []any{APIProtocolChatCompletions},
	}
	body := []byte(`{"model":"gpt-x","max_tokens":16,"stream":false,"messages":[{"role":"user","content":"hi"}]}`)
	upstream := &httpUpstreamRecorder{}
	c := newOpenAIRejectedFieldTestContext(body)

	_, err := newOpenAIRejectedFieldTestService(upstream).ForwardAsAnthropic(
		context.Background(), c, account, body, "", "",
	)

	require.Error(t, err)
	require.Empty(t, upstream.requests)
	require.Equal(t, http.StatusBadRequest, c.Writer.Status())
}

func TestInboundNativeForward_ResponsesInboundStaysNativeWhenSupported(t *testing.T) {
	t.Parallel()

	body := []byte(`{"model":"gpt-x","stream":false,"input":"hi"}`)
	upstream := &httpUpstreamRecorder{responses: []*http.Response{
		newOpenAIRejectedFieldTestResponse(http.StatusOK, inboundNativeResponsesOK),
	}}
	c := newOpenAIRejectedFieldTestContext(body)

	_, err := newOpenAIRejectedFieldTestService(upstream).Forward(
		context.Background(), c, inboundNativeAccount(true, "inbound_native", ""), body,
	)

	require.NoError(t, err)
	require.Len(t, upstream.requests, 1)
	require.Equal(t, "/v1/responses", upstream.requests[0].URL.Path)
	require.True(t, gjson.GetBytes(upstream.bodies[0], "input").Exists(), "native responses body must pass through")
}

func TestInboundNativeForward_ResponsesWithoutSupportFailsWithoutConverting(t *testing.T) {
	t.Parallel()

	body := []byte(`{"model":"gpt-x","stream":false,"input":"hi"}`)
	upstream := &httpUpstreamRecorder{}
	c := newOpenAIRejectedFieldTestContext(body)

	_, err := newOpenAIRejectedFieldTestService(upstream).Forward(
		context.Background(), c, inboundNativeAccount(false, "inbound_native", ""), body,
	)

	require.Error(t, err)
	require.Empty(t, upstream.requests, "no upstream request may be sent when the native endpoint is missing")
	require.Equal(t, http.StatusBadRequest, c.Writer.Status())
}

// A model whose list omits "responses" must fail for a responses request, not be converted to chat.
func TestInboundNativeForward_RestrictedResponsesModelFailsWithoutConverting(t *testing.T) {
	t.Parallel()

	account := inboundNativeAccount(true, "inbound_native", "")
	account.Extra[openai_compat.ExtraKeyInboundNativeProtocols] = map[string]any{
		"gpt-x": []any{APIProtocolChatCompletions},
	}
	body := []byte(`{"model":"gpt-x","stream":false,"input":"hi"}`)
	upstream := &httpUpstreamRecorder{}
	c := newOpenAIRejectedFieldTestContext(body)

	_, err := newOpenAIRejectedFieldTestService(upstream).Forward(
		context.Background(), c, account, body,
	)

	require.Error(t, err)
	require.Empty(t, upstream.requests)
	require.Equal(t, http.StatusBadRequest, c.Writer.Status())
}

// Regression guard for accounts that never opted in (the legacy path must not change).
func TestLegacyForward_ChatStillConvertsToResponses(t *testing.T) {
	t.Parallel()

	body := []byte(`{"model":"gpt-x","stream":false,"messages":[{"role":"user","content":"hi"}]}`)
	upstream := &httpUpstreamRecorder{responses: []*http.Response{
		newOpenAIRejectedFieldTestResponse(http.StatusOK, inboundNativeResponsesSSE),
	}}
	c := newOpenAIRejectedFieldTestContext(body)

	_, err := newOpenAIRejectedFieldTestService(upstream).ForwardAsChatCompletions(
		context.Background(), c, inboundNativeAccount(true, "", ""), body, "", "",
	)

	require.NoError(t, err)
	require.Len(t, upstream.requests, 1)
	require.Equal(t, "/v1/responses", upstream.requests[0].URL.Path)
}
