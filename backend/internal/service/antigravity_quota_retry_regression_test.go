//go:build unit

package service

import (
	"context"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestAntigravityQuotaFailover_InternalRetryPreservesQuota(t *testing.T) {
	for _, mode := range []string{"rate_limit", "model_capacity", "single_account"} {
		for _, credits := range []bool{false, true} {
			t.Run(mode+map[bool]string{false: "/no_credits", true: "/credits_success"}[credits], func(t *testing.T) {
				upstream := &queuedHTTPUpstreamStub{responses: []*http.Response{
					antigravityQuotaResponse("gemini-3.1-pro-high", "65s"),
					antigravityCompatSuccessResponse(),
				}}
				svc, repo := newAntigravityQuotaService(upstream)
				account := newAntigravityCompatAccount(AccountTypeOAuth)
				account.Extra = map[string]any{"allow_overages": credits}
				ctx, cancel := context.WithTimeout(context.Background(), 2500*time.Millisecond)
				defer cancel()
				params := antigravityQuotaParams(svc, ctx, account)
				initial := antigravityQuotaHTTPResponse(http.StatusTooManyRequests, `{"error":{"status":"RESOURCE_EXHAUSTED","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"RATE_LIMIT_EXCEEDED","metadata":{"model":"gemini-3.1-pro-high"}},{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"0.1s"}]}}`)
				if mode == "model_capacity" {
					initial = antigravityQuotaHTTPResponse(http.StatusServiceUnavailable, `{"error":{"status":"UNAVAILABLE","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"MODEL_CAPACITY_EXHAUSTED","metadata":{"model":"gemini-3.1-pro-high"}},{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"0.1s"}]}}`)
				}
				initialBody, err := io.ReadAll(initial.Body)
				require.NoError(t, err)
				_ = initial.Body.Close()
				var result *smartRetryResult
				if mode == "single_account" {
					initial.StatusCode = http.StatusServiceUnavailable
					result = svc.handleSingleAccountRetryInPlace(params, initial, initialBody, "https://ag.test", time.Second, "gemini-3.1-pro-high")
				} else {
					result = svc.handleSmartRetry(params, initial, initialBody, "https://ag.test", 0, []string{"https://ag.test"})
				}
				require.NotNil(t, result)
				require.Nil(t, result.err)
				if credits {
					require.Nil(t, result.switchError)
					require.NotNil(t, result.resp)
					require.Equal(t, http.StatusOK, result.resp.StatusCode)
					_ = result.resp.Body.Close()
					require.Equal(t, 2, upstream.callCount)
					require.Contains(t, string(upstream.requestBodies[1]), "enabledCreditTypes")
					require.Empty(t, repo.modelRateLimitCalls)
				} else {
					require.NotNil(t, result.switchError)
					requireAntigravityQuotaFailover(t, result.switchError.ToUpstreamFailoverError(), false)
					require.Equal(t, 1, upstream.callCount)
					require.Len(t, repo.modelRateLimitCalls, 2)
					for _, call := range repo.modelRateLimitCalls {
						require.WithinDuration(t, time.Now().Add(65*time.Second), call.resetAt, 2*time.Second)
					}
				}
			})
		}
	}
}

func TestAntigravityQuotaFailover_CreditPermissionKeywordsRemainGeneric(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{name: "permission", status: http.StatusForbidden, body: `{"error":{"status":"PERMISSION_DENIED","message":"GOOGLE_ONE_AI is not permitted for this project"}}`},
		{name: "auth", status: http.StatusUnauthorized, body: `{"error":{"status":"UNAUTHENTICATED","message":"Invalid bearer token for GOOGLE_ONE_AI"}}`},
		{name: "config", status: http.StatusBadRequest, body: `{"error":{"status":"INVALID_ARGUMENT","message":"Invalid enabledCreditTypes GOOGLE_ONE_AI"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := &queuedHTTPUpstreamStub{responses: []*http.Response{
				antigravityQuotaResponse("gemini-3.1-pro-high", "65s"),
				antigravityQuotaHTTPResponse(tc.status, tc.body),
			}}
			svc, _ := newAntigravityQuotaService(upstream)
			account := newAntigravityCompatAccount(AccountTypeOAuth)
			account.Extra = map[string]any{"allow_overages": true}
			result, err := svc.antigravityRetryLoop(antigravityQuotaParams(svc, context.Background(), account))
			require.NoError(t, err)
			require.NotNil(t, result)
			require.Equal(t, tc.status, result.resp.StatusCode)
			body, err := io.ReadAll(result.resp.Body)
			_ = result.resp.Body.Close()
			require.NoError(t, err)
			require.JSONEq(t, tc.body, string(body))
			require.False(t, account.isCreditsExhausted())
			require.Equal(t, 2, upstream.callCount)
		})
	}
}
