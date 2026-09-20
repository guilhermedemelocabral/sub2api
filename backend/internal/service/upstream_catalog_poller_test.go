package service

import (
	"bytes"
	"context"
	"errors"
	"log"
	"strings"
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

func TestStartUpstreamCatalogPollerTicksOnceAndStops(t *testing.T) {
	var mu sync.Mutex
	inFlight := 0
	maxInFlight := 0
	calls := 0
	started := make(chan struct{})

	listFn := func(ctx context.Context) ([]Account, error) {
		mu.Lock()
		inFlight++
		if inFlight > maxInFlight {
			maxInFlight = inFlight
		}
		calls++
		if calls == 1 {
			close(started)
		}
		mu.Unlock()

		select {
		case <-ctx.Done():
		case <-time.After(20 * time.Millisecond):
		}

		mu.Lock()
		inFlight--
		mu.Unlock()
		return []Account{{ID: 1, Platform: "openai"}}, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stop := StartUpstreamCatalogPoller(ctx, &fakeCatalogSyncer{}, listFn)
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("expected immediate Tick on start")
	}
	stop()
	stop() // idempotent

	mu.Lock()
	gotCalls := calls
	gotMax := maxInFlight
	mu.Unlock()
	if gotCalls < 1 {
		t.Fatal("expected at least one Tick")
	}
	if gotMax > 1 {
		t.Fatalf("concurrent Ticks: maxInFlight=%d", gotMax)
	}

	time.Sleep(30 * time.Millisecond)
	mu.Lock()
	if calls != gotCalls {
		t.Fatalf("Tick after stop: %d -> %d", gotCalls, calls)
	}
	mu.Unlock()
}

func TestStartUpstreamCatalogPollerStopsOnContextCancel(t *testing.T) {
	started := make(chan struct{})
	listFn := func(ctx context.Context) ([]Account, error) {
		select {
		case <-started:
		default:
			close(started)
		}
		return []Account{{ID: 2, Platform: "openai"}}, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	stop := StartUpstreamCatalogPoller(ctx, &fakeCatalogSyncer{}, listFn)
	select {
	case <-started:
	case <-time.After(time.Second):
		cancel()
		stop()
		t.Fatal("expected immediate Tick on start")
	}
	cancel()
	done := make(chan struct{})
	go func() {
		stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("stop did not return after context cancel")
	}
}

func TestStartUpstreamCatalogPollerLogsListFailure(t *testing.T) {
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev) })

	started := make(chan struct{})
	listFn := func(ctx context.Context) ([]Account, error) {
		select {
		case <-started:
		default:
			close(started)
		}
		return nil, errors.New("list boom")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stop := StartUpstreamCatalogPoller(ctx, &fakeCatalogSyncer{}, listFn)
	defer stop()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("expected Tick on start")
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		out := buf.String()
		if strings.Contains(out, "tick_failed") && strings.Contains(out, "list boom") {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("log missing tick failure: %q", buf.String())
}
