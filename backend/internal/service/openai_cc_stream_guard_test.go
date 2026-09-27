//go:build unit

package service

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// hangingHeadersUpstream accepts the request and never answers headers, like a
// provider whose backend is down behind a live gateway socket.
type hangingHeadersUpstream struct {
	httpUpstreamRecorder
}

func (u *hangingHeadersUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	select {
	case <-req.Context().Done():
		return nil, req.Context().Err()
	case <-time.After(10 * time.Second):
		return nil, errors.New("header guard did not fire")
	}
}

func (u *hangingHeadersUpstream) DoWithTLS(req *http.Request, proxyURL string, accountID int64, accountConcurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, proxyURL, accountID, accountConcurrency)
}

func ccGuardTestContext(t *testing.T, path string, body []byte) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	return c, rec
}

func ccGuardTestConfig(firstOutputSec, idleSec int) *config.Config {
	cfg := rawChatCompletionsTestConfig()
	cfg.Gateway.OpenAIFirstOutputTimeoutSeconds = firstOutputSec
	cfg.Gateway.StreamDataIntervalTimeout = idleSec
	cfg.Gateway.MaxLineSize = defaultMaxLineSize
	return cfg
}

func TestCCStreamHeaderTimeout_HungUpstreamFailsOver(t *testing.T) {
	body := []byte(`{"model":"qwen3.8-27b","messages":[{"role":"user","content":"ping"}],"stream":true}`)
	c, rec := ccGuardTestContext(t, "/v1/chat/completions", body)
	svc := &OpenAIGatewayService{cfg: ccGuardTestConfig(1, 0), httpUpstream: &hangingHeadersUpstream{}}

	start := time.Now()
	result, err := svc.forwardAsRawChatCompletions(context.Background(), c, rawChatCompletionsTestAccount(), body, "")
	require.Less(t, time.Since(start), 5*time.Second)
	require.Nil(t, result)
	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr)
	require.Equal(t, http.StatusGatewayTimeout, failoverErr.StatusCode)
	require.True(t, failoverErr.ShouldRetryNextAccount())
	require.False(t, c.Writer.Written())
	require.Empty(t, rec.Body.String())
}

func TestCCStreamHeaderTimeout_OnlyGuardsStreams(t *testing.T) {
	svc := &OpenAIGatewayService{cfg: ccGuardTestConfig(120, 180)}
	account := rawChatCompletionsTestAccount()
	require.Equal(t, 120*time.Second, svc.ccStreamHeaderTimeout(account, true, []byte(`{}`)))
	require.Zero(t, svc.ccStreamHeaderTimeout(account, false, []byte(`{}`)), "non-stream bodies arrive only after the full generation")
	require.Zero(t, svc.ccStreamIdleTimeout(account, false))
	require.Equal(t, 180*time.Second, svc.ccStreamIdleTimeout(account, true))

	grok := &Account{ID: 7, Platform: PlatformGrok}
	require.Zero(t, svc.ccStreamHeaderTimeout(grok, true, []byte(`{}`)), "grok keeps its own transport header timeout")
	require.Zero(t, svc.ccStreamIdleTimeout(grok, true))
}

func runCCIdleTest(t *testing.T, upstreamBody io.ReadCloser) (*OpenAIForwardResult, error, *gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	body := []byte(`{"model":"glm-5.3-flash","messages":[{"role":"user","content":"ping"}],"stream":true}`)
	c, rec := ccGuardTestContext(t, "/v1/chat/completions", body)
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       upstreamBody,
	}}
	svc := &OpenAIGatewayService{cfg: ccGuardTestConfig(0, 1), httpUpstream: upstream}
	result, err := svc.forwardAsRawChatCompletions(context.Background(), c, rawChatCompletionsTestAccount(), body, "")
	return result, err, c, rec
}

func TestCCStreamIdle_PreOutputStallFailsOver(t *testing.T) {
	start := time.Now()
	result, err, c, rec := runCCIdleTest(t, newIdleTestBody(""))
	require.Less(t, time.Since(start), 5*time.Second)
	require.Nil(t, result)
	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr)
	require.True(t, failoverErr.ShouldRetryNextAccount())
	require.False(t, c.Writer.Written())
	require.Empty(t, rec.Body.String())
}

func TestCCStreamIdle_PostOutputStallFailsLoudly(t *testing.T) {
	chunk := "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"},\"finish_reason\":null}]}\n\n"
	start := time.Now()
	result, err, _, rec := runCCIdleTest(t, newIdleTestBody(chunk))
	require.Less(t, time.Since(start), 5*time.Second)
	require.NotNil(t, result)
	var failoverErr *UpstreamFailoverError
	require.False(t, errors.As(err, &failoverErr), "bytes already reached the client")
	_, _, ok := OpenAIUpstreamStreamReadErrorDetails(err)
	require.True(t, ok, "post-output stall must surface as a stream error, got %v", err)
	require.Contains(t, rec.Body.String(), `"content":"hi"`)
}

func TestCCStreamIdle_FastStreamUnaffected(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"pong"},"finish_reason":null}]}`,
		"",
		`data: {"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")
	result, err, _, rec := runCCIdleTest(t, io.NopCloser(strings.NewReader(stream)))
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Contains(t, rec.Body.String(), `"content":"pong"`)
	require.Contains(t, rec.Body.String(), "data: [DONE]")
}

func TestCCStreamIdle_MessagesFallbackDoesNotFakeMessageStop(t *testing.T) {
	body := []byte(`{"model":"mimo-v2.6-flash","max_tokens":256,"stream":true,"messages":[{"role":"user","content":"ping"}]}`)
	c, rec := ccGuardTestContext(t, "/v1/messages", body)
	chunk := "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"half\"},\"finish_reason\":null}]}\n\n"
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       newIdleTestBody(chunk),
	}}
	svc := &OpenAIGatewayService{cfg: ccGuardTestConfig(0, 1), httpUpstream: upstream}

	start := time.Now()
	_, err := svc.forwardAnthropicViaRawChatCompletions(context.Background(), c, rawChatCompletionsTestAccount(), body, "")
	require.Less(t, time.Since(start), 5*time.Second)
	require.Error(t, err)
	require.Contains(t, rec.Body.String(), "half")
	require.NotContains(t, rec.Body.String(), "message_stop", "a stalled upstream must not look like a finished answer")
}
