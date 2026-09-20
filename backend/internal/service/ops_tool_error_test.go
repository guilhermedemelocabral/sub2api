package service

import (
	"errors"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func opsToolErrorTestContext() *gin.Context {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	return c
}

func opsToolErrorEvents(t *testing.T, c *gin.Context) []*OpsUpstreamErrorEvent {
	t.Helper()
	v, ok := c.Get(OpsUpstreamErrorsKey)
	require.True(t, ok, "expected upstream error events in context")
	events, ok := v.([]*OpsUpstreamErrorEvent)
	require.True(t, ok)
	return events
}

func TestAppendRequestTranslationErrorEvent_ToolError(t *testing.T) {
	t.Parallel()
	c := opsToolErrorTestContext()
	account := &Account{ID: 7, Name: "flatt", Platform: PlatformOpenAI, Type: AccountTypeAPIKey}

	appendRequestTranslationErrorEvent(c, account, "glm-5.3-flash",
		errors.New(`custom tool "search" conflicts with a function tool of the same name`))

	events := opsToolErrorEvents(t, c)
	require.Len(t, events, 1)
	require.Equal(t, "tool_error", events[0].Kind)
	require.Equal(t, "request_translation", events[0].Stage)
	require.Equal(t, int64(7), events[0].AccountID)
	require.Equal(t, "flatt", events[0].AccountName)
	require.Equal(t, PlatformOpenAI, events[0].Platform)
	require.Contains(t, events[0].Detail, "glm-5.3-flash")
	require.Contains(t, events[0].Message, "conflicts")
}

func TestAppendRequestTranslationErrorEvent_GenericError(t *testing.T) {
	t.Parallel()
	c := opsToolErrorTestContext()

	appendRequestTranslationErrorEvent(c, nil, "", errors.New("unexpected end of JSON input"))

	events := opsToolErrorEvents(t, c)
	require.Len(t, events, 1)
	require.Equal(t, "request_error", events[0].Kind)
	require.Equal(t, "request_translation", events[0].Stage)
	require.Zero(t, events[0].AccountID)
	require.Empty(t, events[0].Detail)
}

func TestIsToolRelatedTranslationError(t *testing.T) {
	t.Parallel()
	for _, msg := range []string{
		`tool_choice "auto" is invalid here`,
		`function_call output missing call_id`,
		`cannot lower function call output`,
		`mcp_tool result too large`,
		`parallel_tool_calls without tools`,
	} {
		require.True(t, isToolRelatedTranslationError(errors.New(msg)), msg)
	}
	for _, msg := range []string{
		"unexpected end of JSON input",
		"model is required",
		"context deadline exceeded",
	} {
		require.False(t, isToolRelatedTranslationError(errors.New(msg)), msg)
	}
}

func TestAppendRequestTranslationErrorEvent_NilGuards(t *testing.T) {
	t.Parallel()
	require.NotPanics(t, func() {
		appendRequestTranslationErrorEvent(nil, nil, "", errors.New("x"))
		appendRequestTranslationErrorEvent(opsToolErrorTestContext(), nil, "", nil)
	})
}
