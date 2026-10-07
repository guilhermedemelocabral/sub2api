//go:build unit

package service

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func antigravityQuotaResponse(model, delay string) *http.Response {
	return antigravityQuotaHTTPResponse(http.StatusTooManyRequests, fmt.Sprintf(`{"error":{"code":429,"status":"RESOURCE_EXHAUSTED","message":"QUOTA_EXHAUSTED","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"QUOTA_EXHAUSTED","metadata":{"model":%q,"quotaResetDelay":%q}}]}}`, model, delay))
}

func antigravityQuotaHTTPResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header: http.Header{
			"Content-Type": []string{"application/json"},
			"X-Request-Id": []string{"quota-request"},
		},
		Body: io.NopCloser(strings.NewReader(body)),
	}
}

func newAntigravityQuotaService(upstream HTTPUpstream) (*AntigravityGatewayService, *stubAntigravityAccountRepo) {
	svc := newAntigravityCompatService(config.GatewayConfig{MaxLineSize: defaultMaxLineSize}, upstream)
	repo := &stubAntigravityAccountRepo{}
	svc.accountRepo = repo
	svc.schedulerSnapshot = &SchedulerSnapshotService{}
	return svc, repo
}

func antigravityQuotaParams(svc *AntigravityGatewayService, ctx context.Context, account *Account) antigravityRetryLoopParams {
	return antigravityRetryLoopParams{
		ctx: ctx, prefix: "[quota-test]", account: account, accessToken: "token",
		action: "streamGenerateContent", body: []byte(`{"model":"gemini-3.1-pro-high","request":{"contents":[{"role":"user","parts":[{"text":"ok"}]}]}}`),
		httpUpstream: svc.httpUpstream, accountRepo: svc.accountRepo,
		handleError: svc.handleUpstreamError, requestedModel: "gemini-3.1-pro-high",
	}
}

func requireAntigravityQuotaFailover(t *testing.T, err error, sticky bool) *UpstreamFailoverError {
	t.Helper()
	var failover *UpstreamFailoverError
	require.ErrorAs(t, err, &failover)
	require.Equal(t, GatewayFailureReason("antigravity_quota_exhausted"), failover.Reason)
	require.Equal(t, http.StatusTooManyRequests, failover.StatusCode)
	require.Equal(t, sticky, failover.ForceCacheBilling)
	require.False(t, failover.RetryableOnSameAccount)
	require.True(t, failover.ShouldRetryNextAccount())
	require.Contains(t, string(failover.ResponseBody), "QUOTA_EXHAUSTED")
	require.Equal(t, "quota-request", failover.ResponseHeaders.Get("x-request-id"))
	return failover
}

func TestAntigravityQuotaFailover_OneQuotaAttemptPersistsCooldown(t *testing.T) {
	for _, withReset := range []bool{true, false} {
		t.Run(fmt.Sprintf("upstream_reset_%t", withReset), func(t *testing.T) {
			t.Setenv(antigravityFallbackSecondsEnv, "90")
			delay := ""
			wantDelay := 90 * time.Second
			if withReset {
				delay = "65.1s"
				wantDelay = 66 * time.Second
			}
			upstream := &queuedHTTPUpstreamStub{responses: []*http.Response{antigravityQuotaResponse("gemini-3.1-pro-high", delay)}}
			svc, repo := newAntigravityQuotaService(upstream)
			cache := &stubSmartRetryCache{}
			svc.cache = cache
			account := newAntigravityCompatAccount(AccountTypeOAuth)
			ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
			defer cancel()
			params := antigravityQuotaParams(svc, ctx, account)
			params.groupID, params.sessionHash, params.isStickySession = 42, "quota-session", true
			before := time.Now()

			result, err := svc.antigravityRetryLoop(params)

			require.Nil(t, result)
			var switchErr *AntigravityAccountSwitchError
			require.ErrorAs(t, err, &switchErr, "real quota must switch immediately, not wait for generic backoff")
			require.Equal(t, account.ID, switchErr.OriginalAccountID)
			require.True(t, switchErr.IsStickySession)
			require.Equal(t, 1, upstream.callCount)
			require.NoError(t, ctx.Err(), "quota handling must finish before the backoff deadline")
			require.Len(t, repo.modelRateLimitCalls, 2)
			require.Equal(t, "gemini-3.1-pro-high", repo.modelRateLimitCalls[0].modelKey)
			require.Equal(t, antigravityGeminiModelRateLimitKey, repo.modelRateLimitCalls[1].modelKey)
			for _, call := range repo.modelRateLimitCalls {
				require.WithinDuration(t, before.Add(wantDelay), call.resetAt, 2*time.Second)
			}
			require.Empty(t, repo.rateCalls)
			require.True(t, account.isModelRateLimitedWithContext(ctx, params.requestedModel))
			require.Len(t, cache.deleteCalls, 1)
			require.Equal(t, int64(42), cache.deleteCalls[0].groupID)
			require.Equal(t, "quota-session", cache.deleteCalls[0].sessionHash)
		})
	}
}

type antigravityQuotaForwardCase struct {
	name, path, body string
	sticky           bool
	call             func(*AntigravityGatewayService, context.Context, *gin.Context, *Account, []byte) (*ForwardResult, error)
}

func antigravityQuotaForwardCases() []antigravityQuotaForwardCase {
	return []antigravityQuotaForwardCase{
		{
			name: "chat_completions", path: "/v1/chat/completions",
			body: `{"model":"gemini-3.1-pro-high","messages":[{"role":"user","content":"ok"}]}`,
			call: func(s *AntigravityGatewayService, ctx context.Context, c *gin.Context, a *Account, body []byte) (*ForwardResult, error) {
				return s.ForwardAsChatCompletions(ctx, c, a, body, nil)
			},
		},
		{
			name: "responses", path: "/v1/responses",
			body: `{"model":"gemini-3.1-pro-high","input":"ok"}`,
			call: func(s *AntigravityGatewayService, ctx context.Context, c *gin.Context, a *Account, body []byte) (*ForwardResult, error) {
				return s.ForwardAsResponses(ctx, c, a, body, nil)
			},
		},
		{
			name: "messages", path: "/v1/messages", sticky: true,
			body: `{"model":"claude-sonnet-4-5","max_tokens":1024,"messages":[{"role":"user","content":"ok"}]}`,
			call: func(s *AntigravityGatewayService, ctx context.Context, c *gin.Context, a *Account, body []byte) (*ForwardResult, error) {
				return s.Forward(ctx, c, a, body, true)
			},
		},
		{
			name: "gemini", path: "/v1beta/models/gemini-3.1-pro-high:generateContent", sticky: true,
			body: `{"contents":[{"role":"user","parts":[{"text":"ok"}]}]}`,
			call: func(s *AntigravityGatewayService, ctx context.Context, c *gin.Context, a *Account, body []byte) (*ForwardResult, error) {
				return s.ForwardGemini(ctx, c, a, "gemini-3.1-pro-high", "generateContent", gjson.GetBytes(body, "stream").Bool(), body, true)
			},
		},
	}
}

func TestAntigravityQuotaFailover_CrossProtocol(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tt := range antigravityQuotaForwardCases() {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream_%t", tt.name, stream), func(t *testing.T) {
				quota := antigravityQuotaResponse("", "65s")
				upstream := &queuedHTTPUpstreamStub{responses: []*http.Response{quota}}
				svc, _ := newAntigravityQuotaService(upstream)
				body := []byte(strings.TrimSuffix(tt.body, "}") + fmt.Sprintf(`,"stream":%t}`, stream))
				c, recorder := newAntigravityCompatContext(http.MethodPost, tt.path, body)
				ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
				defer cancel()

				result, err := tt.call(svc, ctx, c, newAntigravityCompatAccount(AccountTypeOAuth), body)

				require.Nil(t, result)
				requireAntigravityQuotaFailover(t, err, tt.sticky)
				require.Equal(t, 1, upstream.callCount)
				require.False(t, c.Writer.Written())
				require.Empty(t, recorder.Body.String())
			})
		}
	}
}

func TestAntigravityQuotaFailover_Credits(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tt := range []struct {
		name            string
		enabled         bool
		exhausted       bool
		creditResp      *http.Response
		creditErr       error
		wantCalls       int
		wantOK          bool
		wantCreditLimit bool
	}{
		{name: "disabled", wantCalls: 1},
		{name: "already_exhausted", enabled: true, exhausted: true, wantCalls: 1},
		{name: "success", enabled: true, creditResp: antigravityCompatSuccessResponse(), wantCalls: 2, wantOK: true},
		{name: "quota_remains", enabled: true, creditResp: antigravityQuotaResponse("gemini-3.1-pro-high", "65s"), wantCalls: 2},
		{name: "insufficient_credits", enabled: true, creditResp: antigravityQuotaHTTPResponse(http.StatusForbidden, `{"error":{"message":"Insufficient GOOGLE_ONE_AI credits"}}`), wantCalls: 2, wantCreditLimit: true},
		{name: "network_error", enabled: true, creditErr: io.ErrUnexpectedEOF, wantCalls: 2},
	} {
		t.Run(tt.name, func(t *testing.T) {
			upstream := &queuedHTTPUpstreamStub{
				responses: []*http.Response{antigravityQuotaResponse("gemini-3.1-pro-high", "65s"), tt.creditResp},
				errors:    []error{nil, tt.creditErr},
			}
			svc, repo := newAntigravityQuotaService(upstream)
			account := newAntigravityCompatAccount(AccountTypeOAuth)
			account.Extra = map[string]any{"allow_overages": tt.enabled}
			if tt.exhausted {
				account.Extra[modelRateLimitsKey] = map[string]any{creditsExhaustedKey: map[string]any{
					"rate_limit_reset_at": time.Now().Add(time.Hour).Format(time.RFC3339),
				}}
			}
			body := []byte(`{"model":"gemini-3.1-pro-high","input":"ok"}`)
			c, _ := newAntigravityCompatContext(http.MethodPost, "/v1/responses", body)
			ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
			defer cancel()

			result, err := svc.ForwardAsResponses(ctx, c, account, body, nil)

			if tt.wantOK {
				require.NoError(t, err)
				require.NotNil(t, result)
				require.Empty(t, repo.modelRateLimitCalls)
			} else {
				require.Nil(t, result)
				requireAntigravityQuotaFailover(t, err, false)
				require.True(t, account.isModelRateLimitedWithContext(ctx, "gemini-3.1-pro-high"))
			}
			require.Equal(t, tt.wantCalls, upstream.callCount)
			require.False(t, gjson.GetBytes(upstream.requestBodies[0], "enabledCreditTypes").Exists())
			if tt.wantCalls == 2 {
				require.Equal(t, "GOOGLE_ONE_AI", gjson.GetBytes(upstream.requestBodies[1], "enabledCreditTypes.0").String())
			}
			if tt.wantCreditLimit {
				require.True(t, account.isCreditsExhausted())
			}
		})
	}
}

func TestAntigravityQuotaFailover_CreditsPreserveNonQuotaErrors(t *testing.T) {
	for _, tt := range []struct {
		name   string
		status int
		body   string
	}{
		{name: "auth", status: http.StatusUnauthorized, body: `{"error":{"message":"Invalid bearer token"}}`},
		{name: "config", status: http.StatusBadRequest, body: `{"error":{"message":"invalid project resource name"}}`},
		{name: "capacity", status: http.StatusServiceUnavailable, body: `{"error":{"status":"UNAVAILABLE","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"MODEL_CAPACITY_EXHAUSTED","metadata":{"model":"gemini-3.1-pro-high"}}]}}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			upstream := &queuedHTTPUpstreamStub{responses: []*http.Response{
				antigravityQuotaResponse("gemini-3.1-pro-high", "65s"), antigravityQuotaHTTPResponse(tt.status, tt.body),
			}}
			svc, _ := newAntigravityQuotaService(upstream)
			account := newAntigravityCompatAccount(AccountTypeOAuth)
			account.Extra = map[string]any{"allow_overages": true}
			ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
			defer cancel()

			result, err := svc.antigravityRetryLoop(antigravityQuotaParams(svc, ctx, account))

			require.NoError(t, err)
			require.NotNil(t, result)
			require.Equal(t, tt.status, result.resp.StatusCode)
			body, readErr := io.ReadAll(result.resp.Body)
			_ = result.resp.Body.Close()
			require.NoError(t, readErr)
			require.JSONEq(t, tt.body, string(body))
			require.Equal(t, 2, upstream.callCount)
			require.False(t, account.isCreditsExhausted())
		})
	}
}

func TestAntigravityQuotaFailover_CustomPolicy(t *testing.T) {
	for _, handledCode := range []int{http.StatusTooManyRequests, http.StatusUnauthorized} {
		for _, quota := range []bool{false, true} {
			for _, credits := range []bool{false, true} {
				t.Run(fmt.Sprintf("handled_%d/quota_%t/credits_%t", handledCode, quota, credits), func(t *testing.T) {
					response := antigravityQuotaHTTPResponse(http.StatusTooManyRequests, `{"error":{"message":"too many requests"}}`)
					if quota {
						response = antigravityQuotaResponse("gemini-3.1-pro-high", "65s")
					}
					upstream := &queuedHTTPUpstreamStub{responses: []*http.Response{response, antigravityCompatSuccessResponse()}}
					svc, repo := newAntigravityQuotaService(upstream)
					svc.rateLimitService = &RateLimitService{}
					account := newAntigravityCompatAccount(AccountTypeAPIKey)
					account.Credentials["custom_error_codes_enabled"] = true
					account.Credentials["custom_error_codes"] = []any{float64(handledCode)}
					account.Extra = map[string]any{"allow_overages": credits}

					result, err := svc.antigravityRetryLoop(antigravityQuotaParams(svc, context.Background(), account))

					if quota && !credits {
						require.Nil(t, result)
						var switchErr *AntigravityAccountSwitchError
						require.ErrorAs(t, err, &switchErr, "custom policy must not suppress real quota failover")
						require.Len(t, repo.modelRateLimitCalls, 2)
					} else {
						require.NoError(t, err)
						require.NotNil(t, result)
						defer func() { _ = result.resp.Body.Close() }()
						wantStatus := http.StatusTooManyRequests
						if handledCode != http.StatusTooManyRequests {
							wantStatus = http.StatusInternalServerError
						}
						if quota && credits {
							wantStatus = http.StatusOK
						}
						require.Equal(t, wantStatus, result.resp.StatusCode)
					}
					wantCalls := 1
					if quota && credits {
						wantCalls = 2
					}
					require.Equal(t, wantCalls, upstream.callCount)
					if !quota && handledCode != http.StatusTooManyRequests {
						require.Empty(t, repo.modelRateLimitCalls)
					}
				})
			}
		}
	}
}

func TestAntigravityQuotaFailover_RectifierRetries(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tt := range []struct {
		name                string
		gemini              bool
		firstBody           string
		extraSignatureStage bool
	}{
		{name: "claude_signature", firstBody: `{"error":{"message":"Invalid thinking signature"}}`},
		{name: "claude_signature_tools", firstBody: `{"error":{"message":"Invalid thinking signature"}}`, extraSignatureStage: true},
		{name: "claude_budget", firstBody: `{"error":{"message":"thinking budget_tokens must be >= 1024"}}`},
		{name: "gemini_signature", gemini: true, firstBody: `{"response":{"error":{"message":"Corrupted thought signature."}}}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			responses := []*http.Response{antigravityQuotaHTTPResponse(http.StatusBadRequest, tt.firstBody)}
			if tt.extraSignatureStage {
				responses = append(responses, antigravityQuotaHTTPResponse(http.StatusBadRequest, tt.firstBody))
			}
			responses = append(responses, antigravityQuotaResponse("", "65s"))
			upstream := &queuedHTTPUpstreamStub{responses: responses}
			svc, _ := newAntigravityQuotaService(upstream)
			account := newAntigravityCompatAccount(AccountTypeOAuth)
			body := []byte(`{"model":"claude-sonnet-4-5","max_tokens":2048,"thinking":{"type":"enabled","budget_tokens":1024},"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":[{"type":"thinking","thinking":"plan","signature":"bad"},{"type":"tool_use","id":"tool1","name":"run","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"tool1","content":"ok"}]}]}`)
			if tt.gemini {
				body = []byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]},{"role":"model","parts":[{"text":"plan","thought":true,"thoughtSignature":"bad"}]}]}`)
			}
			c, recorder := newAntigravityCompatContext(http.MethodPost, "/v1/messages", body)
			ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
			defer cancel()

			var result *ForwardResult
			var err error
			if tt.gemini {
				result, err = svc.ForwardGemini(ctx, c, account, "gemini-3.1-pro-high", "generateContent", false, body, true)
			} else {
				result, err = svc.Forward(ctx, c, account, body, true)
			}

			require.Nil(t, result)
			requireAntigravityQuotaFailover(t, err, true)
			require.Equal(t, len(responses), upstream.callCount)
			require.False(t, c.Writer.Written())
			require.Empty(t, recorder.Body.String())
		})
	}
}

func TestAntigravityQuotaFailover_EmptyContinuation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tt := range antigravityQuotaForwardCases()[:3] {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream_%t", tt.name, stream), func(t *testing.T) {
				upstream := &queuedHTTPUpstreamStub{responses: []*http.Response{antigravityCompatEmptyStopResponse(), antigravityQuotaResponse("", "65s")}}
				svc, _ := newAntigravityQuotaService(upstream)
				body := []byte(strings.TrimSuffix(tt.body, "}") + fmt.Sprintf(`,"stream":%t}`, stream))
				c, recorder := newAntigravityCompatContext(http.MethodPost, tt.path, body)
				ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
				defer cancel()

				result, err := tt.call(svc, ctx, c, newAntigravityCompatAccount(AccountTypeOAuth), body)

				require.Nil(t, result)
				requireAntigravityQuotaFailover(t, err, tt.sticky)
				require.Equal(t, 2, upstream.callCount, "one initial stream and one continuation quota attempt")
				require.False(t, c.Writer.Written())
				require.Empty(t, recorder.Body.String())
			})
		}
	}
}

type antigravityQuotaReasonRepo struct {
	stubAntigravityAccountRepo
	reasons [][]string
}

func (r *antigravityQuotaReasonRepo) SetModelRateLimit(ctx context.Context, id int64, key string, resetAt time.Time, reasons ...string) error {
	r.reasons = append(r.reasons, append([]string(nil), reasons...))
	return r.stubAntigravityAccountRepo.SetModelRateLimit(ctx, id, key, resetAt, reasons...)
}

func TestAntigravityQuotaFailover_PersistsReason(t *testing.T) {
	upstream := &queuedHTTPUpstreamStub{responses: []*http.Response{antigravityQuotaResponse("gemini-3.1-pro-high", "65s")}}
	svc, _ := newAntigravityQuotaService(upstream)
	repo := &antigravityQuotaReasonRepo{}
	svc.accountRepo = repo
	account := newAntigravityCompatAccount(AccountTypeOAuth)

	_, err := svc.antigravityRetryLoop(antigravityQuotaParams(svc, context.Background(), account))

	var switchErr *AntigravityAccountSwitchError
	require.ErrorAs(t, err, &switchErr)
	require.Equal(t, [][]string{{"antigravity_quota_exhausted"}, {"antigravity_quota_exhausted"}}, repo.reasons)
	limits := account.Extra[modelRateLimitsKey].(map[string]any)
	for _, key := range []string{"gemini-3.1-pro-high", antigravityGeminiModelRateLimitKey} {
		require.Equal(t, "antigravity_quota_exhausted", limits[key].(map[string]any)["reason"])
	}
}

func TestAntigravityQuotaFailover_PreflightVerifiedQuota(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tt := range antigravityQuotaForwardCases() {
		for _, evidence := range []struct {
			name             string
			family           bool
			reason           string
			active           bool
			creditsExhausted bool
			wantQuota        bool
		}{
			{name: "exact_model", reason: "antigravity_quota_exhausted", active: true, wantQuota: true},
			{name: "family", family: true, reason: "antigravity_quota_exhausted", active: true, wantQuota: true},
			{name: "credits_exhausted", reason: "antigravity_quota_exhausted", active: true, creditsExhausted: true, wantQuota: true},
			{name: "expired_evidence_generic_active", family: true, reason: "antigravity_quota_exhausted", wantQuota: false},
			{name: "capacity_reason", reason: "MODEL_CAPACITY_EXHAUSTED", active: true, wantQuota: false},
		} {
			if evidence.family && tt.name == "messages" {
				continue
			}
			t.Run(tt.name+"/"+evidence.name, func(t *testing.T) {
				upstream := &queuedHTTPUpstreamStub{}
				svc, _ := newAntigravityQuotaService(upstream)
				account := newAntigravityCompatAccount(AccountTypeOAuth)
				model := "gemini-3.1-pro-high"
				if tt.name == "messages" {
					model = "claude-sonnet-4-5"
				}
				limits := map[string]any{model: map[string]any{
					"rate_limit_reset_at": time.Now().Add(time.Minute).Format(time.RFC3339),
				}}
				key := model
				if evidence.family {
					key = antigravityGeminiModelRateLimitKey
				}
				resetAt := time.Now().Add(time.Minute)
				if !evidence.active {
					resetAt = time.Now().Add(-time.Minute)
				}
				limits[key] = map[string]any{"reason": evidence.reason, "rate_limit_reset_at": resetAt.Format(time.RFC3339)}
				account.Extra = map[string]any{modelRateLimitsKey: limits}
				if evidence.creditsExhausted {
					account.Extra["allow_overages"] = true
					limits[creditsExhaustedKey] = map[string]any{"rate_limit_reset_at": time.Now().Add(time.Hour).Format(time.RFC3339)}
				}
				body := []byte(tt.body)
				c, _ := newAntigravityCompatContext(http.MethodPost, tt.path, body)

				result, err := tt.call(svc, context.Background(), c, account, body)

				require.Nil(t, result)
				var failover *UpstreamFailoverError
				require.ErrorAs(t, err, &failover)
				if evidence.wantQuota {
					require.Equal(t, GatewayFailureReason("antigravity_quota_exhausted"), failover.Reason)
					require.Equal(t, http.StatusTooManyRequests, failover.StatusCode)
				} else {
					require.Empty(t, failover.Reason)
					require.Equal(t, http.StatusServiceUnavailable, failover.StatusCode)
				}
				require.Equal(t, tt.sticky, failover.ForceCacheBilling)
				require.Zero(t, upstream.callCount)
			})
		}
	}
}

func TestAntigravityQuotaFailover_PreflightRemainsGeneric(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tt := range antigravityQuotaForwardCases() {
		t.Run(tt.name, func(t *testing.T) {
			upstream := &queuedHTTPUpstreamStub{}
			svc, _ := newAntigravityQuotaService(upstream)
			account := newAntigravityCompatAccount(AccountTypeOAuth)
			account.Extra = map[string]any{modelRateLimitsKey: map[string]any{
				"gemini-3.1-pro-high": map[string]any{"rate_limit_reset_at": time.Now().Add(time.Minute).Format(time.RFC3339)},
				"claude-sonnet-4-5":   map[string]any{"rate_limit_reset_at": time.Now().Add(time.Minute).Format(time.RFC3339)},
			}}
			body := []byte(tt.body)
			c, _ := newAntigravityCompatContext(http.MethodPost, tt.path, body)

			result, err := tt.call(svc, context.Background(), c, account, body)

			require.Nil(t, result)
			var failover *UpstreamFailoverError
			require.ErrorAs(t, err, &failover)
			require.Equal(t, http.StatusServiceUnavailable, failover.StatusCode)
			require.Empty(t, failover.Reason)
			require.Equal(t, tt.sticky, failover.ForceCacheBilling)
			require.Zero(t, upstream.callCount)
		})
	}
}
