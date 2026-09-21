package service

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
)

type CatalogSyncer interface {
	SyncUpstreamModelCatalog(ctx context.Context, account *Account) (*UpstreamModelCatalog, error)
}

type CatalogPollerConfig struct {
	Interval time.Duration
	StaleTTL time.Duration
	Now      func() time.Time
	List     func(context.Context) ([]Account, error)
	IsGemini func(platform string) bool
	Persist  func(ctx context.Context, account *Account, catalog *UpstreamModelCatalog) error
}

func DefaultGeminiPlatform(platform string) bool {
	switch strings.ToLower(strings.TrimSpace(platform)) {
	case "gemini", "antigravity", "google", "aistudio", "gemini-cli":
		return true
	default:
		return false
	}
}

type catalogAccountState struct {
	Catalog   *UpstreamModelCatalog
	LastOK    time.Time
	LastError string
	Fresh     bool
}

type UpstreamCatalogPoller struct {
	syncer CatalogSyncer
	cfg    CatalogPollerConfig
	mu     sync.Mutex
	state  map[int64]*catalogAccountState
}

func NewUpstreamCatalogPoller(syncer CatalogSyncer, cfg CatalogPollerConfig) *UpstreamCatalogPoller {
	if cfg.Interval <= 0 {
		cfg.Interval = time.Minute
	}
	if cfg.StaleTTL <= 0 {
		cfg.StaleTTL = 15 * time.Minute
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.IsGemini == nil {
		cfg.IsGemini = DefaultGeminiPlatform
	}
	return &UpstreamCatalogPoller{syncer: syncer, cfg: cfg, state: map[int64]*catalogAccountState{}}
}

func copyCatalogAccountState(src *catalogAccountState) *catalogAccountState {
	if src == nil {
		return nil
	}
	dst := &catalogAccountState{
		LastOK:    src.LastOK,
		LastError: src.LastError,
		Fresh:     src.Fresh,
	}
	if src.Catalog != nil {
		cp := *src.Catalog
		if len(src.Catalog.Models) > 0 {
			cp.Models = append([]string(nil), src.Catalog.Models...)
		}
		dst.Catalog = &cp
	}
	return dst
}

func (p *UpstreamCatalogPoller) Tick(ctx context.Context) error {
	if p == nil || p.syncer == nil || p.cfg.List == nil {
		return nil
	}
	accounts, err := p.cfg.List(ctx)
	if err != nil {
		return err
	}
	now := p.cfg.Now()
	for i := range accounts {
		account := &accounts[i]
		if p.cfg.IsGemini(account.Platform) {
			continue
		}
		catalog, syncErr := p.syncer.SyncUpstreamModelCatalog(ctx, account)
		p.mu.Lock()
		prev := p.state[account.ID]
		if syncErr != nil {
			if prev != nil && now.Sub(prev.LastOK) < p.cfg.StaleTTL {
				prev.LastError = syncErr.Error()
				prev.Fresh = true
			} else if prev != nil {
				prev.Fresh = false
				prev.LastError = syncErr.Error()
			}
			p.mu.Unlock()
			continue
		}
		p.state[account.ID] = &catalogAccountState{Catalog: catalog, LastOK: now, Fresh: true}
		p.mu.Unlock()
		if p.cfg.Persist != nil {
			_ = p.cfg.Persist(ctx, account, catalog)
		}
	}
	return nil
}

func (p *UpstreamCatalogPoller) State(accountID int64) *catalogAccountState {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return copyCatalogAccountState(p.state[accountID])
}

// FreshCatalogModels returns the last-synced live model IDs for accountID.
// ok is false when no fresh catalog exists; callers must fail open and keep
// serving static configuration. An empty-but-fresh list is authoritative.
func (p *UpstreamCatalogPoller) FreshCatalogModels(accountID int64) (models []string, ok bool) {
	if p == nil {
		return nil, false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	st := p.state[accountID]
	if st == nil || !st.Fresh || st.Catalog == nil {
		return nil, false
	}
	return append([]string(nil), st.Catalog.Models...), true
}

// LiveCatalogReader exposes fresh upstream catalogs to the served-models
// list without firing inference requests: the poller's 1/min GET is the
// only upstream traffic behind it.
type LiveCatalogReader interface {
	FreshCatalogModels(accountID int64) (models []string, ok bool)
}

type liveCatalogReaderBox struct {
	reader LiveCatalogReader
}

var modelsListCatalogReader atomic.Value // holds liveCatalogReaderBox

// SetModelsListCatalogReader installs the process-wide live catalog source
// for served-model pruning. A nil reader disables pruning (fail-open).
func SetModelsListCatalogReader(reader LiveCatalogReader) {
	modelsListCatalogReader.Store(liveCatalogReaderBox{reader: reader})
}

func getModelsListCatalogReader() LiveCatalogReader {
	raw := modelsListCatalogReader.Load()
	if raw == nil {
		return nil
	}
	box, _ := raw.(liveCatalogReaderBox)
	if box.reader == nil {
		return nil
	}
	return box.reader
}

func tickCatalogPoller(ctx context.Context, poller *UpstreamCatalogPoller) {
	if err := poller.Tick(ctx); err != nil {
		logger.LegacyPrintf("service.upstream_catalog_poller", "tick_failed: err=%v", err)
	}
}

// StartUpstreamCatalogPoller runs Tick on one goroutine (immediate, then every Interval).
// stop cancels the loop and waits for the goroutine to exit. It is idempotent.
func StartUpstreamCatalogPoller(ctx context.Context, syncer CatalogSyncer, listFn func(context.Context) ([]Account, error)) (stop func()) {
	stop, _ = startUpstreamCatalogPollerWithHandle(ctx, syncer, listFn)
	return stop
}

func startUpstreamCatalogPollerWithHandle(ctx context.Context, syncer CatalogSyncer, listFn func(context.Context) ([]Account, error)) (func(), *UpstreamCatalogPoller) {
	if ctx == nil {
		ctx = context.Background()
	}
	runCtx, cancel := context.WithCancel(ctx)
	poller := NewUpstreamCatalogPoller(syncer, CatalogPollerConfig{
		Interval: time.Minute,
		StaleTTL: 15 * time.Minute,
		List:     listFn,
	})
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(poller.cfg.Interval)
		defer ticker.Stop()
		tickCatalogPoller(runCtx, poller)
		for {
			select {
			case <-runCtx.Done():
				return
			case <-ticker.C:
				tickCatalogPoller(runCtx, poller)
			}
		}
	}()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			<-done
		})
	}
	return stop, poller
}

// UpstreamCatalogPollerRuntime is the process-wide catalog poller started on boot.
type UpstreamCatalogPollerRuntime struct {
	stop func()
}

func (r *UpstreamCatalogPollerRuntime) Stop() {
	if r == nil || r.stop == nil {
		return
	}
	r.stop()
}

// ProvideUpstreamCatalogPoller starts the catalog poller after repos exist.
func ProvideUpstreamCatalogPoller(accountTestService *AccountTestService, accountRepo AccountRepository) *UpstreamCatalogPollerRuntime {
	if accountTestService == nil || accountRepo == nil {
		return &UpstreamCatalogPollerRuntime{}
	}
	ctx, cancel := context.WithCancel(context.Background())
	stop, poller := startUpstreamCatalogPollerWithHandle(ctx, accountTestService, accountRepo.ListSchedulable)
	// The served-models list prunes mapping keys whose upstream target left
	// the live catalog. This stays GET-only: no inference is ever dispatched.
	SetModelsListCatalogReader(poller)
	return &UpstreamCatalogPollerRuntime{stop: func() {
		stop()
		cancel()
	}}
}
