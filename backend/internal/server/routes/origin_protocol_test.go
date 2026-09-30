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

func TestMultiprotocolFamiliesAdmitNativeInbound(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cases := []struct {
		name    string
		model   string
		inbound string
		admit   bool
	}{
		{"stepfun messages", "step-5-preview", originInboundMessages, true},
		{"stepfun responses", "step-5-preview", originInboundResponses, true},
		{"flatt messages", "betallm-glm53flash", originInboundMessages, true},
		{"flatt responses rejected", "betallm-glm53flash", originInboundResponses, false},
		{"llmboost messages", "qwen3.8-flash-next", originInboundMessages, true},
		{"llmboost case-insensitive", "Qwen3.8-27B-Base-W8A16", originInboundMessages, true},
		{"llmboost responses rejected", "qwen3.8-flash-next", originInboundResponses, false},
		{"codex messages", "gpt-5.6-luna", originInboundMessages, true},
		{"codex responses", "gpt-5.6-sol", originInboundResponses, true},
		{"minimax messages", "MiniMax-M3", originInboundMessages, true},
		{"minimax responses rejected", "MiniMax-M3", originInboundResponses, false},
		{"unknown still fails closed", "some-cc-only-model", originInboundMessages, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"`+tc.model+`"}`))
			c.Request.Header.Set("Content-Type", "application/json")
			c.Request = c.Request.WithContext(service.WithCompositeRouteDecision(c.Request.Context(), service.CompositeRouteDecision{
				Matched: true, TargetPlatform: service.PlatformOpenAI,
				PublicModel: tc.model, UpstreamModel: tc.model,
			}))
			c.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{Group: &service.Group{Platform: service.PlatformComposite}})
			rejected := rejectForeignOriginProtocol(c, tc.inbound)
			require.Equal(t, !tc.admit, rejected)
		})
	}
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
