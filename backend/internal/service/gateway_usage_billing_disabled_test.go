//go:build unit

package service

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

type disabledBillingRepoStub struct {
	lastCmd *UsageBillingCommand
}

func (s *disabledBillingRepoStub) Apply(_ context.Context, cmd *UsageBillingCommand) (*UsageBillingApplyResult, error) {
	s.lastCmd = cmd
	return &UsageBillingApplyResult{Applied: true}, nil
}

func (s *disabledBillingRepoStub) ReserveBatchImageBalance(context.Context, *BatchImageBalanceHoldCommand) (*BatchImageBalanceHoldResult, error) {
	return nil, nil
}

func (s *disabledBillingRepoStub) CaptureBatchImageBalance(context.Context, *BatchImageBalanceHoldCommand) (*BatchImageBalanceHoldResult, error) {
	return nil, nil
}

func (s *disabledBillingRepoStub) ReleaseBatchImageBalance(context.Context, *BatchImageBalanceHoldCommand) (*BatchImageBalanceHoldResult, error) {
	return nil, nil
}

func newDisabledBillingTestParams() *postUsageBillingParams {
	return &postUsageBillingParams{
		Cost:    &CostBreakdown{InputCost: 1, OutputCost: 2, TotalCost: 3, ActualCost: 2.5},
		User:    &User{ID: 1},
		APIKey:  &APIKey{ID: 1},
		Account: &Account{ID: 7},
	}
}

func TestApplyUsageBilling_DisabledZeroesMoney(t *testing.T) {
	cfg := &config.Config{}
	cfg.Billing.Disabled = true
	deps := &billingDeps{cfg: cfg, deferredService: &DeferredService{}}
	repo := &disabledBillingRepoStub{}
	usageLog := &UsageLog{Model: "glm-5.3-flash", InputTokens: 10, OutputTokens: 20}

	applied, err := applyUsageBilling(context.Background(), "req-1", usageLog, newDisabledBillingTestParams(), deps, repo)
	require.NoError(t, err)
	require.True(t, applied)
	require.NotNil(t, repo.lastCmd)
	require.Zero(t, repo.lastCmd.BalanceCost)
	require.Zero(t, repo.lastCmd.SubscriptionCost)
	require.Zero(t, repo.lastCmd.APIKeyQuotaCost)
	require.Zero(t, repo.lastCmd.APIKeyRateLimitCost)
	require.Zero(t, repo.lastCmd.AccountQuotaCost)
	require.Equal(t, 10, repo.lastCmd.InputTokens)
	require.Equal(t, 20, repo.lastCmd.OutputTokens)
	require.Equal(t, "glm-5.3-flash", repo.lastCmd.Model)
}

func TestBillingDepsCarriesConfig(t *testing.T) {
	cfg := &config.Config{}
	cfg.Billing.Disabled = true
	// applyUsageBilling reads the router-only switch from deps.cfg; a nil cfg
	// would silently re-enable money movement on that gateway path.
	require.Same(t, cfg, (&OpenAIGatewayService{cfg: cfg}).billingDeps().cfg)
	require.Same(t, cfg, (&GatewayService{cfg: cfg}).billingDeps().cfg)
}

func TestApplyUsageBilling_EnabledKeepsMoney(t *testing.T) {
	cfg := &config.Config{}
	deps := &billingDeps{cfg: cfg, deferredService: &DeferredService{}}
	repo := &disabledBillingRepoStub{}
	usageLog := &UsageLog{Model: "glm-5.3-flash", InputTokens: 10, OutputTokens: 20}

	applied, err := applyUsageBilling(context.Background(), "req-1", usageLog, newDisabledBillingTestParams(), deps, repo)
	require.NoError(t, err)
	require.True(t, applied)
	require.NotNil(t, repo.lastCmd)
	require.InDelta(t, 2.5, repo.lastCmd.BalanceCost, 1e-12)
}
