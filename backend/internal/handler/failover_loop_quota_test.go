package handler

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func antigravityQuotaFailoverError() *service.UpstreamFailoverError {
	return &service.UpstreamFailoverError{
		StatusCode: http.StatusTooManyRequests,
		Scope:      service.GatewayFailureScopeAccount,
		Reason:     service.AntigravityQuotaExhaustedReason,
	}
}

func TestAntigravityQuotaFailoverReachesHealthyAccountBeyondSwitchBudget(t *testing.T) {
	for _, tc := range []struct {
		name        string
		maxSwitches int
		poolSize    int
	}{
		{name: "general", maxSwitches: 10, poolSize: 20},
		{name: "gemini", maxSwitches: 3, poolSize: 8},
		{name: "zero generic budget", maxSwitches: 0, poolSize: 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			fs := NewFailoverState(tc.maxSwitches, false)
			unscheduler := &mockTempUnscheduler{}
			var attempts []int64
			var healthyID int64
			for iteration := 0; iteration < tc.poolSize; iteration++ {
				var selectedID int64
				for id := int64(1); id <= int64(tc.poolSize); id++ {
					if _, excluded := fs.FailedAccountIDs[id]; !excluded {
						selectedID = id
						break
					}
				}
				require.NotZero(t, selectedID)
				attempts = append(attempts, selectedID)
				if selectedID == int64(tc.poolSize) {
					healthyID = selectedID
					break
				}
				action := fs.HandleFailoverError(ctx, unscheduler, selectedID, service.PlatformAntigravity, maxSameAccountRetries, antigravityQuotaFailoverError())
				require.Equal(t, FailoverContinue, action, "quota must not stop selection while untried accounts remain")
			}
			require.Equal(t, int64(tc.poolSize), healthyID)
			require.Len(t, attempts, tc.poolSize)
			for i, id := range attempts {
				require.Equal(t, int64(i+1), id, "failed accounts must never be selected twice")
			}
			require.Equal(t, tc.poolSize-1, fs.SwitchCount)
			require.Empty(t, unscheduler.calls)
		})
	}
}

func TestAntigravityQuotaFailoverEntirePoolTerminates(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	fs := NewFailoverState(3, false)
	unscheduler := &mockTempUnscheduler{}
	for id := int64(1); id <= 20; id++ {
		require.Equal(t, FailoverContinue, fs.HandleFailoverError(ctx, unscheduler, id, service.PlatformAntigravity, maxSameAccountRetries, antigravityQuotaFailoverError()))
	}
	require.Len(t, fs.FailedAccountIDs, 20)
	require.Equal(t, FailoverExhausted, fs.HandleSelectionExhausted(ctx))
	require.Len(t, fs.FailedAccountIDs, 20)
}

func TestAntigravityQuotaFailoverPreservesGenericErrorBudget(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	fs := NewFailoverState(1, false)
	unscheduler := &mockTempUnscheduler{}
	generic := newTestFailoverErr(http.StatusBadGateway, false, false)
	require.Equal(t, FailoverContinue, fs.HandleFailoverError(ctx, unscheduler, 1, service.PlatformAntigravity, 0, generic))
	for id := int64(2); id <= 5; id++ {
		require.Equal(t, FailoverContinue, fs.HandleFailoverError(ctx, unscheduler, id, service.PlatformAntigravity, 0, antigravityQuotaFailoverError()))
	}
	require.Equal(t, FailoverExhausted, fs.HandleFailoverError(ctx, unscheduler, 6, service.PlatformAntigravity, 0, generic))
	require.Equal(t, 5, fs.SwitchCount)
	require.Len(t, fs.FailedAccountIDs, 6)
}

func TestAntigravityQuotaFailoverDoesNotReviveExcludedAccountsAfter503(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	fs := NewFailoverState(3, false)
	unscheduler := &mockTempUnscheduler{}
	require.Equal(t, FailoverContinue, fs.HandleFailoverError(ctx, unscheduler, 1, service.PlatformAntigravity, 0, antigravityQuotaFailoverError()))
	require.Equal(t, FailoverContinue, fs.HandleFailoverError(ctx, unscheduler, 2, service.PlatformAntigravity, 0, newTestFailoverErr(http.StatusServiceUnavailable, false, false)))
	require.Equal(t, FailoverExhausted, fs.HandleSelectionExhausted(ctx))
	require.Contains(t, fs.FailedAccountIDs, int64(1))
	require.Contains(t, fs.FailedAccountIDs, int64(2))
}

func TestAntigravityQuotaFailoverSkipsSameAccountRetries(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	fs := NewFailoverState(1, true)
	unscheduler := &mockTempUnscheduler{}
	err := antigravityQuotaFailoverError()
	err.RetryableOnSameAccount = true
	require.Equal(t, FailoverContinue, fs.HandleFailoverError(ctx, unscheduler, 1, service.PlatformAntigravity, maxSameAccountRetries, err))
	require.Contains(t, fs.FailedAccountIDs, int64(1))
	require.Empty(t, fs.SameAccountRetryCount)
	require.Empty(t, unscheduler.calls)
	require.True(t, fs.ForceCacheBilling)
}

func TestAntigravityQuotaFailoverDuplicateAccountTerminates(t *testing.T) {
	fs := NewFailoverState(1, false)
	unscheduler := &mockTempUnscheduler{}
	require.Equal(t, FailoverContinue, fs.HandleFailoverError(context.Background(), unscheduler, 1, service.PlatformAntigravity, 0, antigravityQuotaFailoverError()))
	require.Equal(t, FailoverExhausted, fs.HandleFailoverError(context.Background(), unscheduler, 1, service.PlatformAntigravity, 0, antigravityQuotaFailoverError()))
	require.Equal(t, 1, fs.SwitchCount)
}

func TestAntigravityQuotaFailoverRespectsCancellationAndExplicitStop(t *testing.T) {
	unscheduler := &mockTempUnscheduler{}
	t.Run("canceled", func(t *testing.T) {
		fs := NewFailoverState(0, false)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		require.Equal(t, FailoverCanceled, fs.HandleFailoverError(ctx, unscheduler, 1, service.PlatformAntigravity, 0, antigravityQuotaFailoverError()))
		require.Empty(t, fs.FailedAccountIDs)
		require.Zero(t, fs.SwitchCount)
	})
	t.Run("stop", func(t *testing.T) {
		fs := NewFailoverState(0, false)
		err := antigravityQuotaFailoverError()
		err.NextAccountAction = service.NextAccountStop
		require.Equal(t, FailoverExhausted, fs.HandleFailoverError(context.Background(), unscheduler, 1, service.PlatformAntigravity, 0, err))
		require.Empty(t, fs.FailedAccountIDs)
		require.Zero(t, fs.SwitchCount)
	})
}

func TestAntigravityQuotaFailoverExceptionIsProviderSpecific(t *testing.T) {
	fs := NewFailoverState(0, false)
	require.Equal(t, FailoverExhausted, fs.HandleFailoverError(context.Background(), &mockTempUnscheduler{}, 1, service.PlatformOpenAI, 0, antigravityQuotaFailoverError()))
	require.Zero(t, fs.SwitchCount)
}
