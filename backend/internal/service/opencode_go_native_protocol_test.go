//go:build unit

package service

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func openCodeGoNativeTestAccount() *Account {
	return &Account{
		ID:          823,
		Name:        "OpenCode Go",
		Platform:    PlatformOpenCodeGo,
		Type:        AccountTypeAPIKey,
		Concurrency: 1,
		Credentials: map[string]any{
			"api_key":      "sk-opencode",
			"account_mode": AccountModeGo,
			"api_protocol": APIProtocolAdaptive,
			"base_url":     "https://opencode.ai/zen/go/v1",
			"api_base_urls": map[string]any{
				APIProtocolChatCompletions: "https://opencode.ai/zen/go/v1",
				APIProtocolAnthropic:       "https://opencode.ai/zen/go",
				APIProtocolResponses:       "https://opencode.ai/zen/go/v1",
			},
			"model_mapping": map[string]any{
				"mimo-v2.6-flash":                 "mimo-v2.6-flash",
				"opencode-go/deepseek-v4.1-flash": "deepseek-v4.1-flash",
				"muse-spark-1.3-contributor":      "muse-spark-1.3-contributor",
				"qwen3.8-flash":                   "qwen3.8-flash",
			},
			"protocol_rules": []any{
				map[string]any{"pattern": "mimo-v2.6-flash", "protocol": APIProtocolChatCompletions},
				map[string]any{"pattern": "deepseek-v4.1-flash", "protocol": APIProtocolChatCompletions},
				map[string]any{"pattern": "muse-spark-1.3-contributor", "protocol": APIProtocolResponses},
				map[string]any{"pattern": "qwen3.8-flash", "protocol": APIProtocolAnthropic},
			},
		},
	}
}

func TestOpenCodeGoRejectsForeignProtocol(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tests := []struct {
		name        string
		path        string
		body        string
		forward     func(*OpenAIGatewayService, *gin.Context, *Account, []byte) error
		wantMessage string
		errPath     string
	}{
		{
			name:        "chat for a Responses model",
			path:        "/v1/chat/completions",
			body:        `{"model":"muse-spark-1.3-contributor","stream":false,"messages":[{"role":"user","content":"hi"}]}`,
			wantMessage: "Use POST /v1/responses.",
			errPath:     "error.message",
			forward: func(svc *OpenAIGatewayService, c *gin.Context, account *Account, body []byte) error {
				_, err := svc.ForwardAsChatCompletions(context.Background(), c, account, body, "", "")
				return err
			},
		},
		{
			name:        "messages for a Chat Completions model",
			path:        "/v1/messages",
			body:        `{"model":"opencode-go/deepseek-v4.1-flash","max_tokens":32,"stream":false,"messages":[{"role":"user","content":"hi"}]}`,
			wantMessage: "Use POST /v1/chat/completions.",
			errPath:     "error.message",
			forward: func(svc *OpenAIGatewayService, c *gin.Context, account *Account, body []byte) error {
				_, err := svc.ForwardAsAnthropic(context.Background(), c, account, body, "", "")
				return err
			},
		},
		{
			name:        "responses for an Anthropic model",
			path:        "/v1/responses",
			body:        `{"model":"qwen3.8-flash","stream":false,"input":"hi"}`,
			wantMessage: "Use POST /v1/messages.",
			errPath:     "error.message",
			forward: func(svc *OpenAIGatewayService, c *gin.Context, account *Account, body []byte) error {
				SetOpenAIClientTransport(c, OpenAIClientTransportHTTP)
				_, err := svc.Forward(context.Background(), c, account, body)
				return err
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			upstream := &httpUpstreamRecorder{}
			svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: upstream}
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, tt.path, bytes.NewReader([]byte(tt.body)))
			c.Request.Header.Set("Content-Type", "application/json")

			err := tt.forward(svc, c, openCodeGoNativeTestAccount(), []byte(tt.body))
			require.Error(t, err)
			require.Nil(t, upstream.lastReq, "a foreign-protocol request must not reach the provider")
			require.Equal(t, http.StatusBadRequest, recorder.Code)
			require.Contains(t, gjson.Get(recorder.Body.String(), tt.errPath).String(), tt.wantMessage)
			if tt.path == "/v1/messages" {
				require.Equal(t, "error", gjson.Get(recorder.Body.String(), "type").String(), "Anthropic clients expect the Anthropic error envelope")
			}
		})
	}
}

func TestOpenCodeGoNativeProtocolStillForwards(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := []byte(`{"model":"mimo-v2.6-flash","stream":false,"messages":[{"role":"user","content":"hi"}]}`)
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body: io.NopCloser(strings.NewReader(
			`{"id":"c1","object":"chat.completion","model":"mimo-v2.6-flash","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3}}`,
		)),
	}}
	svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: upstream}
	c := adaptiveProtocolTestContext("/v1/chat/completions", body)

	_, err := svc.ForwardAsChatCompletions(context.Background(), c, openCodeGoNativeTestAccount(), body, "", "")
	require.NoError(t, err)
	require.NotNil(t, upstream.lastReq)
	require.Equal(t, "https://opencode.ai/zen/go/v1/chat/completions", upstream.lastReq.URL.String())
	require.Equal(t, "mimo-v2.6-flash", gjson.GetBytes(upstream.lastBody, "model").String())
}
