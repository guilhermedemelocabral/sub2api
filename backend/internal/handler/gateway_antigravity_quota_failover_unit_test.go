//go:build unit

package handler

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	middleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type quotaFailoverAccountRepo struct {
	service.AccountRepository
	cooldowns []int64
}

func (r *quotaFailoverAccountRepo) SetModelRateLimit(_ context.Context, id int64, _ string, _ time.Time, _ ...string) error {
	r.cooldowns = append(r.cooldowns, id)
	return nil
}

type quotaFailoverSettingRepo struct {
	service.SettingRepository
}

func (*quotaFailoverSettingRepo) GetValue(context.Context, string) (string, error) {
	return "", nil
}

func (*quotaFailoverSettingRepo) GetValues(context.Context, []string) (map[string]string, error) {
	return map[string]string{}, nil
}

type quotaFailoverUsageRepo struct {
	service.UsageLogRepository
	logs []*service.UsageLog
}

func (r *quotaFailoverUsageRepo) Create(_ context.Context, usage *service.UsageLog) (bool, error) {
	r.logs = append(r.logs, usage)
	return true, nil
}

type quotaFailoverSchedulerCache struct {
	fakeSchedulerCache
	groupID int64
}

func (f *quotaFailoverSchedulerCache) GetSnapshot(_ context.Context, bucket service.SchedulerBucket) ([]*service.Account, bool, error) {
	if bucket.GroupID != f.groupID {
		return nil, true, nil
	}
	var accounts []*service.Account
	for _, account := range f.accounts {
		for _, association := range account.AccountGroups {
			if association.GroupID == bucket.GroupID {
				accounts = append(accounts, account)
				break
			}
		}
	}
	return accounts, true, nil
}

func (f *quotaFailoverSchedulerCache) GetAccount(ctx context.Context, id int64) (*service.Account, error) {
	account, err := f.fakeSchedulerCache.GetAccount(ctx, id)
	if account == nil || err != nil {
		return account, err
	}
	copy := *account
	copy.Extra = make(map[string]any)
	return &copy, nil
}

type quotaFailoverUpstream struct {
	healthyID int64
	attempts  []int64
}

func (u *quotaFailoverUpstream) Do(req *http.Request, _ string, accountID int64, _ int) (*http.Response, error) {
	u.attempts = append(u.attempts, accountID)
	if accountID != u.healthyID {
		body := `{"error":{"code":429,"status":"RESOURCE_EXHAUSTED","message":"Individual quota reached.","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"QUOTA_EXHAUSTED","metadata":{"model":"gemini-3.8-flash-high","quotaResetDelay":"1h"}}]}}`
		return &http.Response{StatusCode: http.StatusTooManyRequests, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
	}
	body := "data: {\"response\":{\"responseId\":\"quota-failover-success\",\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"ok\"}]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":8,\"candidatesTokenCount\":3}}}\n\n"
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
}

func (u *quotaFailoverUpstream) DoWithTLS(req *http.Request, proxyURL string, accountID int64, concurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, proxyURL, accountID, concurrency)
}

func newQuotaFailoverHandler(t *testing.T, poolSize int, healthyID int64) (*GatewayHandler, *service.Group, *quotaFailoverUpstream, *quotaFailoverUsageRepo) {
	t.Helper()
	group := &service.Group{ID: 100, Hydrated: true, Platform: service.PlatformAntigravity, Status: service.StatusActive, RateMultiplier: 1}
	var accounts []*service.Account
	for id := int64(1); id <= int64(poolSize)+1; id++ {
		groupID := group.ID
		if id > int64(poolSize) {
			groupID++
		}
		accounts = append(accounts, &service.Account{
			ID: id, Name: fmt.Sprintf("quota-account-%d", id), Platform: service.PlatformAntigravity,
			Type: service.AccountTypeOAuth, Status: service.StatusActive, Schedulable: true, Concurrency: 1, Priority: int(id),
			Credentials: map[string]any{
				"access_token": "test-token", "project_id": "test-project",
				"model_mapping": map[string]any{"gemini-3.8-flash-high": "gemini-3.8-flash-high", "claude-sonnet-4-5": "claude-sonnet-4-5"},
			},
			AccountGroups: []service.AccountGroup{{AccountID: id, GroupID: groupID}},
		})
	}
	cache := &quotaFailoverSchedulerCache{fakeSchedulerCache: fakeSchedulerCache{accounts: accounts}, groupID: group.ID}
	snapshot := service.NewSchedulerSnapshotService(cache, nil, nil, nil, nil)
	cfg := &config.Config{RunMode: config.RunModeSimple}
	cfg.Default.RateMultiplier = 1
	repo := &quotaFailoverAccountRepo{}
	usageRepo := &quotaFailoverUsageRepo{}
	billingService := service.NewBillingService(cfg, nil)
	deferred := service.NewDeferredService(repo, nil, time.Hour)
	gateway := service.NewGatewayService(
		repo, &fakeGroupRepo{group: group}, usageRepo, nil, nil, nil, nil, nil, cfg, snapshot,
		nil, billingService, nil, nil, nil, nil, deferred, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil,
	)
	billingCache := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
	t.Cleanup(billingCache.Stop)
	upstream := &quotaFailoverUpstream{healthyID: healthyID}
	tokenProvider := service.NewAntigravityTokenProvider(nil, nil, nil)
	ag := service.NewAntigravityGatewayService(repo, nil, nil, tokenProvider, nil, upstream, service.NewSettingService(&quotaFailoverSettingRepo{}, cfg), nil)
	return &GatewayHandler{
		gatewayService: gateway, antigravityGatewayService: ag,
		billingCacheService: billingCache,
		concurrencyHelper:   NewConcurrencyHelper(service.NewConcurrencyService(&fakeConcurrencyCache{}), SSEPingFormatClaude, 0),
		maxAccountSwitches:  10, maxAccountSwitchesGemini: 3, cfg: cfg,
	}, group, upstream, usageRepo
}

func TestGatewayAntigravityQuotaFailoverScansOnlyCurrentGroup(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, endpoint := range []struct {
		name, path, body string
		call             func(*GatewayHandler, *gin.Context)
	}{
		{name: "chat completions", path: "/v1/chat/completions", body: `{"model":"gemini-3.8-flash-high","messages":[{"role":"user","content":"hi"}]}`, call: (*GatewayHandler).ChatCompletions},
		{name: "responses", path: "/v1/responses", body: `{"model":"gemini-3.8-flash-high","input":"hi"}`, call: (*GatewayHandler).Responses},
		{name: "messages", path: "/antigravity/v1/messages", body: `{"model":"claude-sonnet-4-5","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`, call: (*GatewayHandler).Messages},
		{name: "gemini", path: "/antigravity/v1beta/models/gemini-3.8-flash-high:generateContent", body: `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`, call: (*GatewayHandler).GeminiV1BetaModels},
		{name: "chat completions stream", path: "/v1/chat/completions", body: `{"model":"gemini-3.8-flash-high","stream":true,"messages":[{"role":"user","content":"hi"}]}`, call: (*GatewayHandler).ChatCompletions},
		{name: "responses stream", path: "/v1/responses", body: `{"model":"gemini-3.8-flash-high","stream":true,"input":"hi"}`, call: (*GatewayHandler).Responses},
		{name: "messages stream", path: "/antigravity/v1/messages", body: `{"model":"claude-sonnet-4-5","stream":true,"max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`, call: (*GatewayHandler).Messages},
		{name: "gemini stream", path: "/antigravity/v1beta/models/gemini-3.8-flash-high:streamGenerateContent", body: `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`, call: (*GatewayHandler).GeminiV1BetaModels},
	} {
		for _, exhausted := range []bool{false, true} {
			name := endpoint.name + "/healthy last account"
			healthyID := int64(20)
			if exhausted {
				name = endpoint.name + "/healthy account in another group"
				healthyID = 21
			}
			t.Run(name, func(t *testing.T) {
				h, group, upstream, usage := newQuotaFailoverHandler(t, 20, healthyID)
				recorder := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(recorder)
				req := httptest.NewRequest(http.MethodPost, endpoint.path, bytes.NewBufferString(endpoint.body))
				req.Header.Set("Content-Type", "application/json")
				ctx, cancel := context.WithTimeout(req.Context(), 2*time.Second)
				defer cancel()
				ctx = context.WithValue(ctx, ctxkey.Group, group)
				ctx = context.WithValue(ctx, ctxkey.ForcePlatform, service.PlatformAntigravity)
				c.Request = req.WithContext(ctx)
				c.Set(string(middleware.ContextKeyForcePlatform), service.PlatformAntigravity)
				modelAction := "/gemini-3.8-flash-high:generateContent"
				if strings.HasSuffix(endpoint.path, ":streamGenerateContent") {
					modelAction = "/gemini-3.8-flash-high:streamGenerateContent"
				}
				c.Params = gin.Params{{Key: "modelAction", Value: modelAction}}
				apiKey := &service.APIKey{ID: 1, UserID: 1, GroupID: &group.ID, Group: group, Status: service.StatusActive, User: &service.User{ID: 1, Concurrency: 10, Balance: 100}}
				c.Set(string(middleware.ContextKeyAPIKey), apiKey)
				c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 1, Concurrency: 10})
				endpoint.call(h, c)
				if exhausted {
					require.Equal(t, http.StatusTooManyRequests, recorder.Code, recorder.Body.String())
					require.Empty(t, usage.logs)
				} else {
					require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
					require.Contains(t, recorder.Body.String(), "ok")
					require.Len(t, usage.logs, 1)
					require.Equal(t, int64(20), usage.logs[0].AccountID)
				}
				require.Len(t, upstream.attempts, 20)
				for i, id := range upstream.attempts {
					require.Equal(t, int64(i+1), id, "each account must be attempted once, within the current group")
				}
			})
		}
	}
}
