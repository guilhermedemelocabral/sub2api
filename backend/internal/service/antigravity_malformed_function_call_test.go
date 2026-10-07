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
		require.False(t, failoverErr.RetryableOnSameAccount, "malformed function call must rotate to a clean account in the pool")
		require.False(t, c.Writer.Written())
		require.Empty(t, recorder.Body.String())
	}

	// Past the retry budget, gateway returns an explicit 502 error instead of delivering an empty/corrupted payload
	result, err := svc.handleChatCompletionsStreamingFromAntigravity(c, malformedStreamResponse(malformedGeminiLine), time.Now(), "gemini-3.8-flash-high", true)
	require.Nil(t, result)
	require.Error(t, err)
}

func TestAntigravityChatStream_MalformedAfterTextIsNotRetried(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := newAntigravityCompatService(config.GatewayConfig{MaxLineSize: defaultMaxLineSize}, nil)
	c, recorder := newAntigravityCompatContext(http.MethodPost, "/v1/chat/completions", nil)

	textLine := `data: {"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"Vou consultar."}]}}]}}`
	result, err := svc.handleChatCompletionsStreamingFromAntigravity(c, malformedStreamResponse(textLine, malformedGeminiLine), time.Now(), "gemini-3.8-flash-high", true)
	// Text already delivered: the corrupt tail must not be retried or masked as
	// a clean finish; it is reported as an in-band SSE error instead.
	require.ErrorContains(t, err, "malformed function call")
	require.NotNil(t, result)
	var failoverErr *UpstreamFailoverError
	require.NotErrorAs(t, err, &failoverErr)
	require.Contains(t, recorder.Body.String(), "Vou consultar.")
	require.Contains(t, recorder.Body.String(), "malformed_function_call")
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
	// Text already delivered: the corrupt tail must not be retried or masked as
	// a clean finish; it is reported as an in-band SSE error instead.
	require.ErrorContains(t, err, "malformed function call")
	require.NotNil(t, result)
	var failoverErr *UpstreamFailoverError
	require.NotErrorAs(t, err, &failoverErr)
	require.Contains(t, rec.Body.String(), "Vou consultar.")
	require.Contains(t, rec.Body.String(), "malformed_function_call")
}
