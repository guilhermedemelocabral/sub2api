package routes

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestOriginAPIRejection(t *testing.T) {
	require.Empty(t, originAPIRejection(service.PlatformAntigravity, originInboundMessages))
	require.Empty(t, originAPIRejection(service.PlatformGemini, originInboundChat))
	require.Empty(t, originAPIRejection(service.PlatformAntigravity, originInboundGemini))

	require.Contains(t, originAPIRejection(service.PlatformOpenAI, originInboundMessages), "Chat Completions")
	require.Contains(t, originAPIRejection(service.PlatformOpenAI, originInboundResponses), "Chat Completions")
	require.Empty(t, originAPIRejection(service.PlatformOpenAI, originInboundChat))

	require.Empty(t, originAPIRejection(service.PlatformAnthropic, originInboundMessages))
	require.Empty(t, originAPIRejection(service.PlatformKimi, originInboundMessages))
}

func TestStepFunResponsesStaysOnRealChatEndpoint(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"step-5-preview","input":"hi"}`))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{Group: &service.Group{Platform: service.PlatformOpenAI}})

	require.True(t, rejectForeignOriginProtocol(c, originInboundResponses))
	require.Equal(t, http.StatusBadRequest, recorder.Code)
	require.Contains(t, recorder.Body.String(), "Use POST /v1/chat/completions")
}
