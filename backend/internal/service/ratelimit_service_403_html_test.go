//go:build unit

package service

import (
	"context"
	"net/http"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

// countingOpenAI403CounterCache 在既有桩的基础上记录递增次数。
// HTML 403 不仅不能处罚账号，连计数都不能加——否则后续真实的账号级 403
// 会踩着这些"白涨"的计数提前触发永久禁用。
type countingOpenAI403CounterCache struct {
	openAI403CounterCacheStub
	increments int
}

func (s *countingOpenAI403CounterCache) IncrementOpenAI403Count(ctx context.Context, accountID int64, window int) (int64, error) {
	s.increments++
	return s.openAI403CounterCacheStub.IncrementOpenAI403Count(ctx, accountID, window)
}

type openAI403TestHarness struct {
	svc     *RateLimitService
	repo    *rateLimitAccountRepoStub
	counter *countingOpenAI403CounterCache
	blocker *runtimeBlockRecorder
	account *Account
}

func newOpenAI403TestHarness(t *testing.T, accountID int64, counts ...int64) *openAI403TestHarness {
	t.Helper()
	repo := &rateLimitAccountRepoStub{}
	counter := &countingOpenAI403CounterCache{openAI403CounterCacheStub: openAI403CounterCacheStub{counts: counts}}
	blocker := &runtimeBlockRecorder{}
	svc := NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
	svc.SetOpenAI403CounterCache(counter)
	svc.SetAccountRuntimeBlocker(blocker)
	return &openAI403TestHarness{
		svc:     svc,
		repo:    repo,
		counter: counter,
		blocker: blocker,
		account: &Account{ID: accountID, Platform: PlatformOpenAI, Type: AccountTypeOAuth},
	}
}

func (h *openAI403TestHarness) handle(body string) bool {
	return h.svc.HandleUpstreamError(
		context.Background(), h.account, http.StatusForbidden, http.Header{}, []byte(body),
	)
}

func (h *openAI403TestHarness) requireNoAccountPenalty(t *testing.T) {
	t.Helper()
	require.Equal(t, 0, h.repo.setErrorCalls, "端点级 403 不得永久禁用账号")
	require.Equal(t, 0, h.repo.tempCalls, "端点级 403 不得把账号设为临时不可调度")
	require.Empty(t, h.blocker.accounts, "端点级 403 不得触发调度阻断通知")
	require.Equal(t, 0, h.counter.increments, "端点级 403 不得递增连续 403 计数")
}

// issue #5334：无效的 /v1/responses 子路径被转发后，上游代理在到达 OpenAI API
// 之前回 HTML 403 页面。这是链路/端点级响应，不是账号凭据失效的证据。
const openAI403HTMLBody = "<!DOCTYPE html>\n<html><head><title>403 Forbidden</title></head>" +
	"<body><h1>403 Forbidden</h1></body></html>"

const openAI403Cloudflare1010Body = "error code: 1010\n"

// Flatt's Cloudflare custom error page is JSON, not the plaintext "error code: 1010".
// The numeric code and error_name are the stable markers; the detector must not
// require the plaintext phrasing or a burst of these 403s disables the account.
const openAI403Cloudflare1010JSONBody = `{"type":"https://developers.cloudflare.com/support/troubleshooting/http-status-codes/cloudflare-1xxx-errors/error-1010/","title":"Error 1010: Access denied","status":403,"detail":"The site owner has blocked access based on your browser's signature.","error_code":1010,"error_name":"browser_signature_banned","cloudflare_error":true,"what_you_should_do":"Do not retry. Your user-agent has been banned by the site owner."}`

func TestHandleUpstreamError_OpenAIHTML403DoesNotPenalizeAccount(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"doctype_prefixed", openAI403HTMLBody},
		{"bare_html_tag", "<html><body>403 Forbidden</body></html>"},
		{"leading_whitespace_and_uppercase", "\n\t  <!DOCTYPE HTML><html><body>Forbidden</body></html>"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newOpenAI403TestHarness(t, 501, 1)

			shouldDisable := h.handle(tc.body)

			require.False(t, shouldDisable, "HTML 403 不得判定账号应下线")
			h.requireNoAccountPenalty(t)
		})
	}
}

// 一个持有 API Key 的调用方反复打无效子路径时，既有实现会在第
// openAI403DisableThreshold 次把账号永久禁用。修复后连续多少次都不该升级。
func TestHandleUpstreamError_OpenAIHTML403RepeatedNeverEscalates(t *testing.T) {
	h := newOpenAI403TestHarness(t, 502, 1, 2, 3, 4, 5)

	for i := 0; i < openAI403DisableThreshold+2; i++ {
		require.False(t, h.handle(openAI403HTMLBody), "第 %d 次 HTML 403 仍不得判定账号应下线", i+1)
	}

	h.requireNoAccountPenalty(t)
}

func TestHandleUpstreamError_OpenAICloudflare1010DoesNotPenalizeAccount(t *testing.T) {
	for _, platform := range []string{PlatformOpenAI, PlatformOpenCodeGo} {
		t.Run(platform, func(t *testing.T) {
			repo := &rateLimitAccountRepoStub{}
			counter := &countingOpenAI403CounterCache{
				openAI403CounterCacheStub: openAI403CounterCacheStub{counts: []int64{openAI403DisableThreshold}},
			}
			blocker := &runtimeBlockRecorder{}
			svc := NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
			svc.SetOpenAI403CounterCache(counter)
			svc.SetAccountRuntimeBlocker(blocker)
			account := &Account{ID: 601, Platform: platform, Type: AccountTypeAPIKey}

			shouldDisable := svc.HandleUpstreamError(
				context.Background(), account, http.StatusForbidden, http.Header{}, []byte(openAI403Cloudflare1010Body),
			)

			require.False(t, shouldDisable, "Cloudflare 1010 不得判定账号应下线")
			require.Equal(t, 0, counter.increments, "Cloudflare 1010 不得递增账号 403 计数")
			require.Equal(t, 0, repo.setErrorCalls)
			require.Equal(t, 0, repo.tempCalls)
			require.Empty(t, blocker.accounts)
		})
	}
}

func TestHandleUpstreamError_OpenAICloudflare1010JSONDoesNotPenalizeAccount(t *testing.T) {
	h := newOpenAI403TestHarness(t, 602, int64(openAI403DisableThreshold), int64(openAI403DisableThreshold)+1)

	for i := 0; i < openAI403DisableThreshold+1; i++ {
		require.False(t, h.handle(openAI403Cloudflare1010JSONBody), "第 %d 次 JSON 1010 仍不得判定账号应下线", i+1)
	}

	h.requireNoAccountPenalty(t)
}

func flattAccount(id int64) *Account {
	return &Account{
		ID:       id,
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"base_url": "https://infer.flatt.com.br/v1",
		},
	}
}

func TestHandleUpstreamError_FlattStopsOnlyForRateLimit(t *testing.T) {
	t.Run("json_1010_and_auth_403_do_not_penalize", func(t *testing.T) {
		h := newOpenAI403TestHarness(t, 701, int64(openAI403DisableThreshold))
		h.account = flattAccount(701)

		require.False(t, h.handle(openAI403Cloudflare1010JSONBody))
		require.False(t, h.handle(`{"error":{"message":"Your account is not authorized"}}`))
		require.False(t, h.svc.HandleUpstreamError(context.Background(), h.account, http.StatusUnauthorized, http.Header{}, []byte(`{"error":{"message":"invalid api key"}}`)))
		h.requireNoAccountPenalty(t)
		require.Equal(t, 0, h.repo.rateLimitedCalls)
	})

	t.Run("429_still_rate_limits", func(t *testing.T) {
		h := newOpenAI403TestHarness(t, 702)
		h.account = flattAccount(702)

		require.False(t, h.svc.HandleUpstreamError(
			context.Background(),
			h.account,
			http.StatusTooManyRequests,
			http.Header{},
			[]byte(`{"error":{"message":"rate limit","type":"rate_limit_error"}}`),
		))
		require.Equal(t, 0, h.repo.setErrorCalls)
		require.Equal(t, 0, h.repo.tempCalls)
		require.Equal(t, 1, h.repo.rateLimitedCalls)
		require.Equal(t, int64(702), h.repo.lastRateLimitedID)
	})

	t.Run("other_host_structured_403_still_penalizes", func(t *testing.T) {
		h := newOpenAI403TestHarness(t, 703, 1)
		h.account = &Account{
			ID:       703,
			Platform: PlatformOpenAI,
			Type:     AccountTypeAPIKey,
			Credentials: map[string]any{
				"base_url": "https://api.openai.com/v1",
			},
		}

		require.True(t, h.handle(`{"error":{"message":"Your account is not authorized"}}`))
		require.Equal(t, 1, h.repo.tempCalls)
		require.Equal(t, 0, h.repo.setErrorCalls)
	})
}

func TestHandleOpenAIAccountUpstreamError_Flatt403DoesNotDisable(t *testing.T) {
	repo := &rateLimitAccountRepoStub{}
	counter := &countingOpenAI403CounterCache{
		openAI403CounterCacheStub: openAI403CounterCacheStub{counts: []int64{openAI403DisableThreshold}},
	}
	rateLimitSvc := NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
	rateLimitSvc.SetOpenAI403CounterCache(counter)
	gateway := &OpenAIGatewayService{rateLimitService: rateLimitSvc}
	account := flattAccount(704)

	shouldDisable := gateway.handleOpenAIAccountUpstreamError(
		context.Background(),
		account,
		http.StatusForbidden,
		http.Header{},
		[]byte(openAI403Cloudflare1010JSONBody),
		"glm-5.3-flash",
	)

	require.False(t, shouldDisable)
	require.Equal(t, 0, repo.setErrorCalls)
	require.Equal(t, 0, repo.tempCalls)
	require.Equal(t, 0, counter.increments)
}

// 对照不变式：真正的结构化 JSON 403 是账号级证据，处罚链路必须原样保留。
// 缺了这组断言，上面的跳过逻辑一旦写宽就会把真实的封号 403 也放过去。
func TestHandleUpstreamError_OpenAIStructured403StillPenalizes(t *testing.T) {
	t.Run("first_hit_temp_unschedulable", func(t *testing.T) {
		h := newOpenAI403TestHarness(t, 503, 1)

		require.True(t, h.handle(`{"error":{"message":"Your account is not authorized"}}`))
		require.Equal(t, 1, h.counter.increments)
		require.Equal(t, 1, h.repo.tempCalls)
		require.Equal(t, 0, h.repo.setErrorCalls)
		require.Contains(t, h.repo.lastTempReason, "Your account is not authorized")
		require.Len(t, h.blocker.accounts, 1)
	})

	t.Run("threshold_disables", func(t *testing.T) {
		h := newOpenAI403TestHarness(t, 504, int64(openAI403DisableThreshold))

		require.True(t, h.handle(`{"error":{"message":"workspace forbidden by policy"}}`))
		require.Equal(t, 1, h.repo.setErrorCalls)
		require.Contains(t, h.repo.lastErrorMsg, "workspace forbidden by policy")
	})

	// 非 HTML 的非结构化响应（纯文本网关错误）不在本次放行范围内，维持原有处罚。
	t.Run("plain_text_body_unchanged", func(t *testing.T) {
		h := newOpenAI403TestHarness(t, 505, 1)

		require.True(t, h.handle("Forbidden"))
		require.Equal(t, 1, h.repo.tempCalls)
	})
}

// 作用域守卫：放行只针对 OpenAI 平台。其他平台的 403 处理不受影响。
func TestHandleUpstreamError_HTML403OnOtherPlatformsUnchanged(t *testing.T) {
	for _, platform := range []string{PlatformAnthropic, PlatformGemini} {
		t.Run(platform, func(t *testing.T) {
			repo := &rateLimitAccountRepoStub{}
			svc := NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
			account := &Account{ID: 506, Platform: platform, Type: AccountTypeAPIKey}

			shouldDisable := svc.HandleUpstreamError(
				context.Background(), account, http.StatusForbidden, http.Header{}, []byte(openAI403HTMLBody),
			)

			require.True(t, shouldDisable)
			require.Equal(t, 1, repo.setErrorCalls, "其他平台保持原有 SetError 行为")
		})
	}
}

func TestIsHTMLResponse(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"doctype_lower", "<!doctype html><html></html>", true},
		{"doctype_upper", "<!DOCTYPE HTML>", true},
		{"bare_html", "<html lang=\"en\">", true},
		{"leading_whitespace", "\n\n   <html>", true},
		{"json_error", `{"error":{"message":"forbidden"}}`, false},
		{"plain_text", "Forbidden", false},
		{"empty", "", false},
		// XML/SVG 之类不是 HTML，不在放行范围内。
		{"xml_declaration", `<?xml version="1.0"?><error/>`, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, isHTMLResponse([]byte(tc.body)))
		})
	}
}
