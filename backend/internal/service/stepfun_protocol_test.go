package service

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestStepFunChatCompletionsUsesRealEndpoint(t *testing.T) {
	gin.SetMode(gin.TestMode)
	require.True(t, IsStepFunChatCompletionsModel("step-5-preview"))
	require.True(t, IsStepFunChatCompletionsModel("vendor/step-5-preview"))
	require.False(t, IsStepFunChatCompletionsModel("stepfun-step-5-preview"))
	require.False(t, IsStepFunChatCompletionsModel("gpt-5.4"))

	body := []byte(`{"model":"step-5-preview","stream":false,"temperature":0.2,"messages":[{"role":"user","content":"hi"}]}`)
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body: io.NopCloser(strings.NewReader(
			`{"id":"c1","object":"chat.completion","model":"step-5-preview","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3}}`,
		)),
	}}
	svc := &OpenAIGatewayService{cfg: &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{AllowInsecureHTTP: true}}}, httpUpstream: upstream}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	account := &Account{
		ID: 7, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Concurrency: 1,
		Credentials: map[string]any{"api_key": "sk-test", "base_url": "http://upstream.example/v1"},
	}

	result, err := svc.ForwardAsChatCompletions(context.Background(), c, account, body, "", "")
	require.NoError(t, err)
	require.NotNil(t, result)
	require.NotNil(t, upstream.lastReq)
	require.Equal(t, "http://upstream.example/v1/chat/completions", upstream.lastReq.URL.String())
	require.Equal(t, "step-5-preview", gjson.GetBytes(upstream.lastBody, "model").String())
	require.Equal(t, "hi", gjson.GetBytes(upstream.lastBody, "messages.0.content").String())
	require.Equal(t, 0.2, gjson.GetBytes(upstream.lastBody, "temperature").Float())
	require.False(t, gjson.GetBytes(upstream.lastBody, "input").Exists())
}
