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

func remoteContractGatewayTestConfig() *config.Config {
	return &config.Config{
		Security: config.SecurityConfig{
			URLAllowlist: config.URLAllowlistConfig{
				Enabled:           false,
				AllowInsecureHTTP: true,
			},
		},
	}
}

func remoteContractGatewayTestAccount(name string) *Account {
	return &Account{
		ID:          101,
		Name:        name,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Concurrency: 1,
		Credentials: map[string]any{
			"api_key":  "sk-test",
			"base_url": "http://upstream.example",
		},
	}
}

func TestForwardAsRawChatCompletions_ContinueOnLengthConcatenates(t *testing.T) {
	gin.SetMode(gin.TestMode)

	body := []byte(`{"model":"glm","messages":[{"role":"user","content":"q"}],"stream":false}`)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	upstream := &httpUpstreamRecorder{responses: []*http.Response{
		{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}, "x-request-id": []string{"rid_len"}},
			Body: io.NopCloser(strings.NewReader(
				`{"id":"chatcmpl_1","object":"chat.completion","model":"glm","choices":[{"index":0,"message":{"role":"assistant","content":"Hello "},"finish_reason":"length"}]}`,
			)),
		},
		{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}, "x-request-id": []string{"rid_stop"}},
			Body: io.NopCloser(strings.NewReader(
				`{"id":"chatcmpl_2","object":"chat.completion","model":"glm","choices":[{"index":0,"message":{"role":"assistant","content":"world"},"finish_reason":"stop"}]}`,
			)),
		},
	}}
	svc := &OpenAIGatewayService{cfg: remoteContractGatewayTestConfig(), httpUpstream: upstream}
	account := remoteContractGatewayTestAccount("betallm")

	result, err := svc.forwardAsRawChatCompletions(context.Background(), c, account, body, "")
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Len(t, upstream.bodies, 2)
	require.Contains(t, rec.Body.String(), "Hello world")
	require.Equal(t, "stop", gjson.GetBytes(rec.Body.Bytes(), "choices.0.finish_reason").String())
	require.Contains(t, string(upstream.bodies[1]), "Continue from the exact point")
}

func TestForwardAsRawChatCompletions_ContinueNudgeErrorKeepsPartial(t *testing.T) {
	gin.SetMode(gin.TestMode)

	body := []byte(`{"model":"glm","messages":[{"role":"user","content":"q"}],"stream":false}`)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	upstream := &httpUpstreamRecorder{responses: []*http.Response{
		{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}, "x-request-id": []string{"rid_len"}},
			Body: io.NopCloser(strings.NewReader(
				`{"id":"chatcmpl_1","choices":[{"index":0,"message":{"role":"assistant","content":"partial-ok"},"finish_reason":"length"}]}`,
			)),
		},
		{
			StatusCode: http.StatusBadGateway,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"nudge failed"}}`)),
		},
	}}
	svc := &OpenAIGatewayService{cfg: remoteContractGatewayTestConfig(), httpUpstream: upstream}
	account := remoteContractGatewayTestAccount("betallm")

	result, err := svc.forwardAsRawChatCompletions(context.Background(), c, account, body, "")
	require.Error(t, err)
	require.NotNil(t, result)
	require.Contains(t, rec.Body.String(), "partial-ok")
	require.NotContains(t, rec.Body.String(), "nudge failed")
}

func TestForwardAsRawChatCompletions_NormalizesEmptyStepfunSSEIdentity(t *testing.T) {
	gin.SetMode(gin.TestMode)

	body := []byte(`{"model":"step-2","messages":[{"role":"user","content":"q"}],"stream":true}`)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	upstreamBody := strings.Join([]string{
		`data: {"id":"chatcmpl_1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"","type":"","function":{"name":"","arguments":"{\"x\":1}"}}]}}]}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}, "x-request-id": []string{"rid_sse"}},
		Body:       io.NopCloser(strings.NewReader(upstreamBody)),
	}}
	svc := &OpenAIGatewayService{cfg: remoteContractGatewayTestConfig(), httpUpstream: upstream}
	account := remoteContractGatewayTestAccount("stepfun")

	result, err := svc.forwardAsRawChatCompletions(context.Background(), c, account, body, "")
	require.NoError(t, err)
	require.NotNil(t, result)

	downstream := rec.Body.String()
	require.Contains(t, downstream, `{\"x\":1}`)
	require.NotContains(t, downstream, `"id":""`)
	require.NotContains(t, downstream, `"type":""`)
	require.NotContains(t, downstream, `"name":""`)
}
