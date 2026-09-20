package service

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type fakeCatalogSyncer struct {
	mu    sync.Mutex
	calls int
	err   error
}

func (f *fakeCatalogSyncer) SyncUpstreamModelCatalog(ctx context.Context, account *Account) (*UpstreamModelCatalog, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return &UpstreamModelCatalog{Models: []string{"m1"}}, nil
}

func TestCatalogPollerSkipsGeminiPlatforms(t *testing.T) {
	accounts := []Account{
		{ID: 1, Name: "gemini", Platform: "gemini"},
		{ID: 2, Name: "antigravity", Platform: "antigravity"},
		{ID: 3, Name: "stepfun", Platform: "openai"},
	}
	syncer := &fakeCatalogSyncer{}
	poller := NewUpstreamCatalogPoller(syncer, CatalogPollerConfig{
		Interval: time.Hour,
		StaleTTL: 15 * time.Minute,
		Now:      func() time.Time { return time.Unix(0, 0) },
		List:     func(context.Context) ([]Account, error) { return accounts, nil },
		IsGemini: DefaultGeminiPlatform,
	})
	if err := poller.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if syncer.calls != 1 {
		t.Fatalf("syncer.calls=%d want 1 (openai only)", syncer.calls)
	}
}

func TestCatalogPollerKeepsStaleSnapshotOnError(t *testing.T) {
	now := time.Unix(1_000, 0)
	account := Account{ID: 9, Name: "stepfun", Platform: "openai"}
	syncer := &fakeCatalogSyncer{}
	poller := NewUpstreamCatalogPoller(syncer, CatalogPollerConfig{
		Interval: time.Hour,
		StaleTTL: 15 * time.Minute,
		Now:      func() time.Time { return now },
		List:     func(context.Context) ([]Account, error) { return []Account{account}, nil },
		IsGemini: DefaultGeminiPlatform,
	})
	if err := poller.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	syncer.err = errors.New("500")
	if err := poller.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	state := poller.State(9)
	if state == nil || !state.Fresh || len(state.Catalog.Models) != 1 {
		t.Fatalf("stale cache lost: %+v", state)
	}
}

func TestCatalogPollerDropsAfterTTL(t *testing.T) {
	now := time.Unix(1_000, 0)
	account := Account{ID: 9, Name: "stepfun", Platform: "openai"}
	syncer := &fakeCatalogSyncer{}
	poller := NewUpstreamCatalogPoller(syncer, CatalogPollerConfig{
		Interval: time.Hour,
		StaleTTL: 15 * time.Minute,
		Now:      func() time.Time { return now },
		List:     func(context.Context) ([]Account, error) { return []Account{account}, nil },
		IsGemini: DefaultGeminiPlatform,
	})
	if err := poller.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	syncer.err = errors.New("500")
	now = now.Add(16 * time.Minute)
	if err := poller.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if poller.State(9) != nil && poller.State(9).Fresh {
		t.Fatal("cache should be expired")
	}
}

func TestCatalogPollerNoStateOnFirstError(t *testing.T) {
	account := Account{ID: 9, Name: "stepfun", Platform: "openai"}
	syncer := &fakeCatalogSyncer{err: errors.New("500")}
	poller := NewUpstreamCatalogPoller(syncer, CatalogPollerConfig{
		Interval: time.Hour,
		StaleTTL: 15 * time.Minute,
		Now:      func() time.Time { return time.Unix(0, 0) },
		List:     func(context.Context) ([]Account, error) { return []Account{account}, nil },
		IsGemini: DefaultGeminiPlatform,
	})
	if err := poller.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if poller.State(9) != nil {
		t.Fatalf("State(9)=%+v want nil on first error", poller.State(9))
	}
}

func TestCatalogPollerStateReturnsCopy(t *testing.T) {
	account := Account{ID: 9, Name: "stepfun", Platform: "openai"}
	syncer := &fakeCatalogSyncer{}
	poller := NewUpstreamCatalogPoller(syncer, CatalogPollerConfig{
		Interval: time.Hour,
		StaleTTL: 15 * time.Minute,
		Now:      func() time.Time { return time.Unix(1_000, 0) },
		List:     func(context.Context) ([]Account, error) { return []Account{account}, nil },
		IsGemini: DefaultGeminiPlatform,
	})
	if err := poller.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	state := poller.State(9)
	if state == nil {
		t.Fatal("expected state")
	}
	state.Fresh = false
	state.Catalog.Models[0] = "mutated"
	again := poller.State(9)
	if again == nil || !again.Fresh || again.Catalog.Models[0] != "m1" {
		t.Fatalf("State returned live map entry: %+v", again)
	}
}
