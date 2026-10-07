package service

import (
	"context"
	"net/http"
	"strings"

	"github.com/tidwall/gjson"
)

// AntigravityQuotaExhaustedReason identifies confirmed account quota exhaustion,
// not generic rate limits, model capacity failures, or preflight cooldowns.
const AntigravityQuotaExhaustedReason GatewayFailureReason = "antigravity_quota_exhausted"

// ToUpstreamFailoverError preserves quota evidence across protocol boundaries.
// Legacy account switches retain their 503 and sticky-session billing behavior.
func (e *AntigravityAccountSwitchError) ToUpstreamFailoverError() *UpstreamFailoverError {
	status := http.StatusServiceUnavailable
	if e.Reason == AntigravityQuotaExhaustedReason {
		status = http.StatusTooManyRequests
	}
	return &UpstreamFailoverError{
		StatusCode:        status,
		Reason:            e.Reason,
		ResponseBody:      e.ResponseBody,
		ResponseHeaders:   e.ResponseHeaders.Clone(),
		ForceCacheBilling: e.IsStickySession,
	}
}

func isAntigravityQuotaExhausted(account *Account, status int, body []byte) bool {
	return account != nil && account.Platform == PlatformAntigravity &&
		status == http.StatusTooManyRequests && classifyAntigravity429(body) == antigravity429QuotaExhausted
}

// antigravityPreflightQuotaReason only trusts active locks that were written
// from confirmed upstream quota exhaustion. Historical/generic locks stay 503.
func antigravityPreflightQuotaReason(ctx context.Context, account *Account, requestedModel string) GatewayFailureReason {
	if account == nil || account.Platform != PlatformAntigravity {
		return ""
	}
	limits, _ := account.Extra[modelRateLimitsKey].(map[string]any)
	for _, key := range account.modelRateLimitKeysForRequest(ctx, requestedModel) {
		limit, _ := limits[key].(map[string]any)
		if account.isRateLimitActiveForKey(key) && limit["reason"] == string(AntigravityQuotaExhaustedReason) {
			return AntigravityQuotaExhaustedReason
		}
	}
	return ""
}

func (s *AntigravityGatewayService) handleAntigravityQuotaExhausted(
	p antigravityRetryLoopParams,
	resp *http.Response,
	respBody []byte,
	baseURL string,
) *smartRetryResult {
	modelName := ""
	gjson.GetBytes(respBody, "error.details").ForEach(func(_, detail gjson.Result) bool {
		if model := strings.TrimSpace(detail.Get("metadata.model").String()); model != "" {
			modelName = normalizeAntigravityModelName(model)
			return false
		}
		return true
	})
	if modelName == "" {
		// The serialized request has already applied mappings and thinking suffixes.
		modelName = normalizeAntigravityModelName(gjson.GetBytes(p.body, "model").String())
	}
	if modelName == "" {
		modelName = resolveCreditsOveragesModelKey(p.ctx, p.account, "", p.requestedModel)
	}

	var creditsResp *http.Response
	if p.account.IsOveragesEnabled() && !p.account.isCreditsExhausted() &&
		!gjson.GetBytes(p.body, "enabledCreditTypes").Exists() {
		creditsResult := s.attemptCreditsOveragesRetry(p, baseURL, modelName, 0, resp.StatusCode, respBody)
		if creditsResult.resp != nil && creditsResult.resp.StatusCode < http.StatusBadRequest {
			return &smartRetryResult{action: smartRetryActionBreakWithResp, resp: creditsResult.resp}
		}
		creditsResp = creditsResult.resp
	}

	resetAt := s.resolveResetTime(ParseGeminiRateLimitResetTime(respBody), s.getDefaultRateLimitDuration())
	if !s.setAntigravityModelRateLimits(p.ctx, p.accountRepo, p.account, modelName, p.prefix, resp.StatusCode, resetAt, false, string(AntigravityQuotaExhaustedReason)) && p.handleError != nil {
		p.handleError(p.ctx, p.prefix, p.account, resp.StatusCode, resp.Header, respBody, p.requestedModel, p.groupID, p.sessionHash, p.isStickySession)
	}
	s.clearStickySession(p.ctx, p.groupID, p.sessionHash)

	// A distinct auth/config/capacity error from credits is not a quota failure.
	if creditsResp != nil {
		return &smartRetryResult{action: smartRetryActionBreakWithResp, resp: creditsResp}
	}
	return &smartRetryResult{
		action: smartRetryActionBreakWithResp,
		switchError: &AntigravityAccountSwitchError{
			OriginalAccountID: p.account.ID,
			RateLimitedModel:  modelName,
			IsStickySession:   p.isStickySession,
			Reason:            AntigravityQuotaExhaustedReason,
			ResponseBody:      append([]byte(nil), respBody...),
			ResponseHeaders:   resp.Header.Clone(),
		},
	}
}
