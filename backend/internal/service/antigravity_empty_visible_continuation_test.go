package service

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestAntigravityChatStream_EmptyStopContinuesOnceWithRealText(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upstream := &queuedHTTPUpstreamStub{responses: []*http.Response{
		antigravityCompatEmptyStopResponse(),
		antigravityCompatTextResponse("resposta real"),
	}}
	svc := newAntigravityCompatService(config.GatewayConfig{MaxLineSize: defaultMaxLineSize}, upstream)
	body := []byte(`{"model":"gemini-3.1-pro-high","messages":[{"role":"user","content":"explique o pipe"}],"stream":true}`)
	c, recorder := newAntigravityCompatContext(http.MethodPost, "/v1/chat/completions", body)

	result, err := svc.ForwardAsChatCompletions(context.Background(), c, newAntigravityCompatAccount(AccountTypeOAuth), body, nil)

	require.NoError(t, err)
	require.NotNil(t, result)
	require.Len(t, upstream.requestBodies, 2)
	require.Equal(t, gjson.GetBytes(upstream.requestBodies[0], "model").String(), gjson.GetBytes(upstream.requestBodies[1], "model").String())
	firstContents := gjson.GetBytes(upstream.requestBodies[0], "request.contents").Array()
	secondContents := gjson.GetBytes(upstream.requestBodies[1], "request.contents").Array()
	require.Len(t, secondContents, len(firstContents)+2)
	require.Equal(t, "model", secondContents[len(secondContents)-2].Get("role").String())
	require.Equal(t, "user", secondContents[len(secondContents)-1].Get("role").String())
	require.Equal(t, antigravityEmptyTurnContinuationText, secondContents[len(secondContents)-1].Get("parts.0.text").String())
	require.Contains(t, recorder.Body.String(), "resposta real")
	require.NotContains(t, recorder.Body.String(), antigravityEmptyTurnContinuationText)
	require.NotContains(t, recorder.Body.String(), "turno sem saída")
}

func TestAntigravityChatStream_SecondEmptyStopDoesNotRetryOrInventText(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upstream := &queuedHTTPUpstreamStub{responses: []*http.Response{
		antigravityCompatEmptyStopResponse(),
		antigravityCompatEmptyStopResponse(),
	}}
	svc := newAntigravityCompatService(config.GatewayConfig{MaxLineSize: defaultMaxLineSize}, upstream)
	body := []byte(`{"model":"gemini-3.1-pro-high","messages":[{"role":"user","content":"explique o pipe"}],"stream":true}`)
	c, recorder := newAntigravityCompatContext(http.MethodPost, "/v1/chat/completions", body)

	result, err := svc.ForwardAsChatCompletions(context.Background(), c, newAntigravityCompatAccount(AccountTypeOAuth), body, nil)

	require.Nil(t, result)
	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr)
	require.Equal(t, http.StatusBadGateway, failoverErr.StatusCode)
	require.False(t, failoverErr.RetryableOnSameAccount)
	require.Equal(t, NextAccountStop, failoverErr.NextAccountAction)
	require.Len(t, upstream.requestBodies, 2)
	require.NotContains(t, recorder.Body.String(), "turno sem saída")
	require.NotContains(t, recorder.Body.String(), antigravityEmptyTurnContinuationText)
}

func antigravityCompatEmptyStopResponse() *http.Response {
	body := `data: {"response":{"candidates":[{"content":{"role":"model","parts":[]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":12,"candidatesTokenCount":0,"thoughtsTokenCount":0}}}` + "\n\n"
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func antigravityCompatTextResponse(text string) *http.Response {
	body := `data: {"response":{"responseId":"resp_cont","candidates":[{"content":{"parts":[{"text":"` + text + `"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":20,"candidatesTokenCount":4}}}` + "\n\n"
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}
