package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/handler"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestProvideServiceBuildInfo(t *testing.T) {
	in := handler.BuildInfo{
		Version:   "v-test",
		BuildType: "release",
	}
	out := provideServiceBuildInfo(in)
	require.Equal(t, in.Version, out.Version)
	require.Equal(t, in.BuildType, out.BuildType)
}

func TestProvideCleanup_WithMinimalDependencies_NoPanic(t *testing.T) {
	cfg := &config.Config{}

	oauthSvc := service.NewOAuthService(nil, nil)
	openAIOAuthSvc := service.NewOpenAIOAuthService(nil, nil)
	geminiOAuthSvc := service.NewGeminiOAuthService(nil, nil, nil, nil, cfg)
	antigravityOAuthSvc := service.NewAntigravityOAuthService(nil)

	tokenRefreshSvc := service.NewTokenRefreshService(
		nil,
		oauthSvc,
		openAIOAuthSvc,
		geminiOAuthSvc,
		antigravityOAuthSvc,
		nil,
		nil,
		cfg,
		nil,
	)
	accountExpirySvc := service.NewAccountExpiryService(nil, time.Second)
	codexVersionSyncSvc := service.NewOpenAICodexVersionSyncService(nil, nil, nil, time.Second)
	claudeCodeVersionSyncSvc := service.NewClaudeCodeVersionSyncService(nil, nil, nil, time.Second)
	proxyExpirySvc := service.NewProxyExpiryService(nil, time.Second)
	subscriptionExpirySvc := service.NewSubscriptionExpiryService(nil, time.Second)
	pricingSvc := service.NewPricingService(cfg, nil)
	emailQueueSvc := service.NewEmailQueueService(nil, 1)
	billingCacheSvc := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
	idempotencyCleanupSvc := service.NewIdempotencyCleanupService(nil, cfg)
	schedulerSnapshotSvc := service.NewSchedulerSnapshotService(nil, nil, nil, nil, cfg)
	opsSystemLogSinkSvc := service.NewOpsSystemLogSink(nil)

	cleanup := provideCleanup(
		nil, // entClient
		nil, // redis
		&service.OpsMetricsCollector{},
		&service.OpsAggregationService{},
		&service.OpsAlertEvaluatorService{},
		&service.OpsCleanupService{},
		&service.OpsScheduledReportService{},
		opsSystemLogSinkSvc,
		nil, // opsService
		nil, // opsIngressRejectAggregator
		nil, // apiKeyService
		nil, // authCacheInvalidationWorker
		schedulerSnapshotSvc,
		tokenRefreshSvc,
		accountExpirySvc,
		nil, // cnProviderBalanceCheck
		codexVersionSyncSvc,
		claudeCodeVersionSyncSvc,
		proxyExpirySvc,
		subscriptionExpirySvc,
		&service.UsageCleanupService{},
		idempotencyCleanupSvc,
		&service.BatchImageCleanupService{},
		nil, // batchImageWorker
		pricingSvc,
		emailQueueSvc,
		billingCacheSvc,
		&service.UsageRecordWorkerPool{},
		&service.SubscriptionService{},
		oauthSvc,
		openAIOAuthSvc,
		geminiOAuthSvc,
		antigravityOAuthSvc,
		nil, // grokOAuth
		nil, // openAIGateway
		nil, // scheduledTestRunner
		nil, // backupSvc
		nil, // paymentOrderExpiry
		nil, // channelMonitorRunner
		nil, // channelMonitorV2Aggregator
		nil, // quotaFlusher
		nil, // upstreamBillingProbe
		nil, // ollamaCloudUsage
		nil, // opencodeGoUsage
		nil, // auditLog
		nil, // openAIAutoReset
		nil, // promptAudit
		nil, // pluginManager
		nil, // distillCollector
	)

	require.NotPanics(t, func() {
		cleanup()
	})
}

type distillSettingsRepoLoadErr struct {
	err error
}

func (r *distillSettingsRepoLoadErr) Get(ctx context.Context, key string) (*service.Setting, error) {
	return nil, r.err
}

func (r *distillSettingsRepoLoadErr) GetValue(ctx context.Context, key string) (string, error) {
	return "", r.err
}

func (r *distillSettingsRepoLoadErr) Set(ctx context.Context, key, value string) error {
	return r.err
}

func (r *distillSettingsRepoLoadErr) GetMultiple(ctx context.Context, keys []string) (map[string]string, error) {
	return nil, r.err
}

func (r *distillSettingsRepoLoadErr) SetMultiple(ctx context.Context, settings map[string]string) error {
	return r.err
}

func (r *distillSettingsRepoLoadErr) GetAll(ctx context.Context) (map[string]string, error) {
	return nil, r.err
}

func (r *distillSettingsRepoLoadErr) Delete(ctx context.Context, key string) error {
	return r.err
}

type prefixEncryptor struct{}

func (prefixEncryptor) Encrypt(plaintext string) (string, error) { return plaintext, nil }
func (prefixEncryptor) Decrypt(ciphertext string) (string, error) {
	return ciphertext, nil
}

func TestProvideDistillCollectorLogsDriveDisabledOnSettingsError(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelError})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	cfg := &config.Config{}
	cfg.Distill.Enabled = true
	cfg.Distill.Dir = t.TempDir()
	cfg.Distill.Drive.Enabled = true
	cfg.Distill.Drive.FolderID = "folder"
	cfg.Distill.Drive.ClientID = "client-id"

	repo := &distillSettingsRepoLoadErr{err: errors.New("settings read failed")}
	creds := service.NewDistillCredentials(repo, prefixEncryptor{}, cfg.Distill.Drive.ClientID)
	collector := provideDistillCollector(cfg, creds, nil, nil)
	require.NotNil(t, collector)
	require.True(t, collector.Enabled())
	require.Contains(t, buf.String(), "distill.drive_disabled")
	require.NotContains(t, buf.String(), "distill.capture_disabled")
}
