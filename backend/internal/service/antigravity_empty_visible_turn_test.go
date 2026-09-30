package service

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestAntigravityChatStream_EmptyStopRetriesInsteadOfSyntheticText(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := newAntigravityCompatService(config.GatewayConfig{MaxLineSize: defaultMaxLineSize}, nil)
	c, recorder := newAntigravityCompatContext(http.MethodPost, "/v1/chat/completions", nil)

	line := `data: {"response":{"candidates":[{"content":{"role":"model","parts":[]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":12,"candidatesTokenCount":0,"thoughtsTokenCount":40}}}`
	result, err := svc.handleChatCompletionsStreamingFromAntigravity(
		c,
		malformedStreamResponse(line),
		time.Now(),
		"gemini-3.8-flash-high",
		true,
	)

	require.Nil(t, result)
	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr)
	require.Equal(t, http.StatusBadGateway, failoverErr.StatusCode)
	require.True(t, failoverErr.RetryableOnSameAccount)
	require.False(t, c.Writer.Written())
	require.NotContains(t, recorder.Body.String(), "turno sem saída")
	require.Empty(t, recorder.Body.String())
}

func TestAntigravityChatStream_RealTextDoesNotRetry(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := newAntigravityCompatService(config.GatewayConfig{MaxLineSize: defaultMaxLineSize}, nil)
	c, recorder := newAntigravityCompatContext(http.MethodPost, "/v1/chat/completions", nil)

	line := `data: {"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"resposta real"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":3}}}`
	result, err := svc.handleChatCompletionsStreamingFromAntigravity(
		c,
		malformedStreamResponse(line),
		time.Now(),
		"gemini-3.8-flash-high",
		true,
	)

	require.NoError(t, err)
	require.NotNil(t, result)
	require.Contains(t, recorder.Body.String(), "resposta real")
	require.NotContains(t, recorder.Body.String(), "turno sem saída")
}

func TestAntigravityMessagesStream_EmptyStopRetriesInsteadOfSyntheticText(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := newAntigravityTestService(&config.Config{Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize}})
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	line := `data: {"response":{"candidates":[{"content":{"role":"model","parts":[{"thought":true}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":8,"candidatesTokenCount":0,"thoughtsTokenCount":15}}}`
	result, err := svc.handleClaudeStreamingResponse(c, malformedStreamResponse(line), time.Now(), "gemini-3.8-flash-high")

	require.Nil(t, result)
	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr)
	require.True(t, failoverErr.RetryableOnSameAccount)
	require.False(t, c.Writer.Written())
	require.NotContains(t, rec.Body.String(), "turno sem saída")
}

func TestAntigravityChatNonStream_EmptyStopRetriesInsteadOfSyntheticText(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := newAntigravityCompatService(config.GatewayConfig{MaxLineSize: defaultMaxLineSize}, nil)
	c, recorder := newAntigravityCompatContext(http.MethodPost, "/v1/chat/completions", nil)

	line := `data: {"response":{"candidates":[{"content":{"role":"model","parts":[]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":4,"candidatesTokenCount":0,"thoughtsTokenCount":9}}}`
	result, err := svc.handleChatCompletionsNonStreamingFromAntigravity(
		c,
		malformedStreamResponse(line),
		time.Now(),
		"gemini-3.8-flash-high",
	)

	require.Nil(t, result)
	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr)
	require.True(t, failoverErr.RetryableOnSameAccount)
	require.NotContains(t, recorder.Body.String(), "turno sem saída")
	require.Empty(t, recorder.Body.String())
}
