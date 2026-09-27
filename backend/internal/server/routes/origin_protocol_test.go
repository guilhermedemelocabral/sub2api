package routes

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
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
