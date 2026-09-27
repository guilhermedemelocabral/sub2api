package service

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

const malformedGeminiLine = `data: {"response":{"candidates":[{"content":{"role":"model","parts":[{"thoughtSignature":"EtsC"}]},"finishReason":"MALFORMED_FUNCTION_CALL","finishMessage":"Malformed function call: print(get_weather(city='SP'))"}],"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":0}}}`

func TestIsAntigravityMalformedFunctionCallLine(t *testing.T) {
	require.True(t, isAntigravityMalformedFunctionCallLine(malformedGeminiLine))
	require.True(t, isAntigravityMalformedFunctionCallLine(`data: {"candidates":[{"finishReason":"MALFORMED_FUNCTION_CALL"}]}`))
	require.False(t, isAntigravityMalformedFunctionCallLine(`data: {"response":{"candidates":[{"content":{"parts":[{"text":"MALFORMED_FUNCTION_CALL is a word"}]},"finishReason":"STOP"}]}}`))
	require.False(t, isAntigravityMalformedFunctionCallLine(`data: {"response":{"candidates":[{"finishReason":"STOP"}]}}`))
	require.False(t, isAntigravityMalformedFunctionCallLine(""))
}

func malformedStreamResponse(lines ...string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(strings.Join(lines, "\n\n") + "\n\n")),
	}
}

func TestAntigravityChatStream_MalformedBeforeOutputRetriesThenDelivers(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := newAntigravityCompatService(config.GatewayConfig{MaxLineSize: defaultMaxLineSize}, nil)
	c, recorder := newAntigravityCompatContext(http.MethodPost, "/v1/chat/completions", nil)

	for attempt := 1; attempt <= antigravityMalformedRetryLimit; attempt++ {
		result, err := svc.handleChatCompletionsStreamingFromAntigravity(c, malformedStreamResponse(malformedGeminiLine), time.Now(), "gemini-3.8-flash-high", true)
		require.Nil(t, result, "attempt %d", attempt)
		var failoverErr *UpstreamFailoverError
		require.ErrorAs(t, err, &failoverErr)
		require.True(t, failoverErr.RetryableOnSameAccount)
		require.True(t, failoverErr.RequestScopedTransient, "a model-side malformed call must not unschedule the account")
		require.Equal(t, 1, failoverErr.SameAccountRetryMax)
		require.False(t, c.Writer.Written())
		require.Empty(t, recorder.Body.String())
	}

	result, err := svc.handleChatCompletionsStreamingFromAntigravity(c, malformedStreamResponse(malformedGeminiLine), time.Now(), "gemini-3.8-flash-high", true)
	require.NoError(t, err, "past the retry budget the upstream answer is delivered")
	require.NotNil(t, result)
	require.Contains(t, recorder.Body.String(), "data: [DONE]")
}

func TestAntigravityChatStream_MalformedAfterTextIsNotRetried(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := newAntigravityCompatService(config.GatewayConfig{MaxLineSize: defaultMaxLineSize}, nil)
	c, recorder := newAntigravityCompatContext(http.MethodPost, "/v1/chat/completions", nil)

	textLine := `data: {"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"Vou consultar."}]}}]}}`
	result, err := svc.handleChatCompletionsStreamingFromAntigravity(c, malformedStreamResponse(textLine, malformedGeminiLine), time.Now(), "gemini-3.8-flash-high", true)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Contains(t, recorder.Body.String(), "Vou consultar.")
}

func TestAntigravityMessagesStream_MalformedBeforeOutputRetries(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := newAntigravityTestService(&config.Config{Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize}})
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	result, err := svc.handleClaudeStreamingResponse(c, malformedStreamResponse(malformedGeminiLine), time.Now(), "gemini-3.8-flash-high")
	require.Nil(t, result)
	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr)
	require.True(t, failoverErr.RequestScopedTransient)
	require.False(t, c.Writer.Written())
	require.Empty(t, rec.Body.String())
}

func TestAntigravityMessagesStream_MalformedAfterTextIsNotRetried(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := newAntigravityTestService(&config.Config{Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize}})
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	textLine := `data: {"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"Vou consultar."}]}}]}}`
	result, err := svc.handleClaudeStreamingResponse(c, malformedStreamResponse(textLine, malformedGeminiLine), time.Now(), "gemini-3.8-flash-high")
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Contains(t, rec.Body.String(), "Vou consultar.")
	require.Contains(t, rec.Body.String(), "message_stop")
}
