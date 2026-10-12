package distill

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

const maxUploadBackoff = 5 * time.Minute

// CollectorConfig configures local capture and optional Drive upload.
type CollectorConfig struct {
	Enabled      bool
	Dir          string
	QueueSize    int
	QueueBytes   int64
	PartBytes    int64
	DriveEnabled bool
	Drive        DriveAPI
	Now          func() time.Time
	Backoff      func(attempt int) time.Duration
	AutoUpload   bool
}

// Collector captures normalized turn lines and uploads closed parts when Drive is configured.
type Collector struct {
	enabled      atomic.Bool
	driveEnabled bool
	dir          string
	writer       *Writer
	drive        DriveAPI
	closedUpload chan string
	backoff      func(attempt int) time.Duration
	now          func() time.Time
	uploadCtx    context.Context
	cancelUpload context.CancelFunc

	reconcileSynced atomic.Bool

	stopOnce    sync.Once
	stopCh      chan struct{}
	uploadWg    sync.WaitGroup
	watchWg     sync.WaitGroup
	reconcileWg sync.WaitGroup
}

func NewCollector(cfg CollectorConfig) *Collector {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	uploadCtx, cancelUpload := context.WithCancel(context.Background())
	c := &Collector{
		dir:          cfg.Dir,
		driveEnabled: cfg.DriveEnabled,
		drive:        cfg.Drive,
		closedUpload: make(chan string, 32),
		stopCh:       make(chan struct{}),
		now:          cfg.Now,
		uploadCtx:    uploadCtx,
		cancelUpload: cancelUpload,
	}
	if cfg.Backoff == nil {
		c.backoff = defaultUploadBackoff
	} else {
		c.backoff = cfg.Backoff
	}
	if !cfg.Enabled {
		return c
	}

	w, err := NewWriter(cfg.Dir, cfg.QueueSize, cfg.QueueBytes, cfg.PartBytes, cfg.Now)
	if err != nil {
		slog.Error("distill.capture_disabled", "err", err)
		return c
	}
	w.SetDropObserver(func(reason string, total int64) {
		slog.Warn("distill.turn_dropped", "reason", reason, "dropped", total)
	})
	c.writer = w
	c.enabled.Store(true)

	if cfg.DriveEnabled && cfg.Drive != nil {
		c.startStartupReconcile()
	} else {
		c.reconcileSynced.Store(true)
	}

	c.watchWg.Add(1)
	go c.watchClosedParts(cfg.Now)

	autoUpload := cfg.AutoUpload
	if autoUpload && cfg.DriveEnabled && cfg.Drive != nil {
		c.uploadWg.Add(1)
		go c.runUploader()
	}

	return c
}

func defaultUploadBackoff(attempt int) time.Duration {
	if attempt < 0 {
		attempt = 0
	}
	d := time.Second
	for i := 0; i < attempt && d < maxUploadBackoff; i++ {
		d *= 2
	}
	if d > maxUploadBackoff {
		return maxUploadBackoff
	}
	return d
}

func (c *Collector) Enabled() bool {
	return c != nil && c.enabled.Load()
}

func (c *Collector) Written() int64 {
	if c == nil || c.writer == nil {
		return 0
	}
	return c.writer.Written()
}

func (c *Collector) ClosedForUpload() <-chan string {
	return c.closedUpload
}

func (c *Collector) Enqueue(line []byte) {
	if c == nil || !c.enabled.Load() || c.writer == nil {
		return
	}
	c.writer.Enqueue(line)
}

func (c *Collector) Close() error {
	if c == nil {
		return nil
	}
	c.stopOnce.Do(func() {
		close(c.stopCh)
		// Unblock an in-flight Drive call. The 5 minute client timeout remains
		// the backstop only for attempts that are not shutting down.
		if c.cancelUpload != nil {
			c.cancelUpload()
		}
	})
	c.reconcileWg.Wait()
	if c.writer != nil {
		if err := c.writer.Close(); err != nil {
			return err
		}
	}
	c.watchWg.Wait()
	c.uploadWg.Wait()
	return nil
}

func (c *Collector) startStartupReconcile() {
	c.reconcileWg.Add(1)
	go func() {
		defer c.reconcileWg.Done()
		attempt := 0
		for {
			select {
			case <-c.stopCh:
				return
			default:
			}
			openName := openPartBaseName(c.dir, c.now)
			err := Reconcile(c.uploadCtx, c.dir, openName, c.drive)
			if err == nil {
				c.reconcileSynced.Store(true)
				return
			}
			select {
			case <-c.stopCh:
				return
			default:
			}
			part := ReconcileFailedPart(err)
			if part == "" {
				part = "reconcile"
			}
			slog.Error("distill.upload_failed", "part", part, "err", err)
			delay := c.backoff(attempt)
			attempt++
			timer := time.NewTimer(delay)
			select {
			case <-c.stopCh:
				timer.Stop()
				return
			case <-timer.C:
			}
		}
	}()
}

func (c *Collector) watchClosedParts(now func() time.Time) {
	defer c.watchWg.Done()
	seen := make(map[string]struct{})
	var startupPending map[string]struct{}
	if c.driveEnabled && c.drive != nil && c.writer != nil {
		startupPending = make(map[string]struct{})
		for _, p := range c.writer.ClosedParts() {
			startupPending[p] = struct{}{}
		}
	}
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-c.stopCh:
			return
		case <-ticker.C:
			if c.writer == nil {
				continue
			}
			for _, p := range c.writer.ClosedParts() {
				if _, ok := seen[p]; ok {
					continue
				}
				if startupPending != nil {
					if _, pending := startupPending[p]; pending && !c.reconcileSynced.Load() {
						continue
					}
				}
				seen[p] = struct{}{}
				select {
				case c.closedUpload <- p:
				case <-c.stopCh:
					return
				}
			}
			_ = now
		}
	}
}

func (c *Collector) runUploader() {
	defer c.uploadWg.Done()
	for {
		select {
		case <-c.stopCh:
			return
		case path := <-c.closedUpload:
			_ = c.uploadPartWithRetry(c.uploadCtx, path)
		}
	}
}

func (c *Collector) uploadPartWithRetry(ctx context.Context, path string) error {
	if c == nil || !c.driveEnabled || c.drive == nil {
		return nil
	}
	attempt := 0
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-c.stopCh:
			return context.Canceled
		default:
		}
		err := uploadPartOnce(ctx, c.dir, path, c.drive)
		if err == nil {
			return nil
		}
		slog.Error("distill.upload_failed", "part", filepath.Base(path), "err", err)
		delay := c.backoff(attempt)
		attempt++
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-c.stopCh:
			timer.Stop()
			return context.Canceled
		case <-timer.C:
		}
	}
}

func uploadPartOnce(ctx context.Context, dir, path string, api DriveAPI) error {
	name := filepath.Base(path)
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	ledger, err := loadLedger(dir)
	if err != nil {
		return err
	}
	if id, ok := ledger[name]; ok && id != "" {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}

	if id, ok, err := api.FindByName(ctx, name); err != nil {
		return err
	} else if ok {
		ledger[name] = id
		if err := saveLedger(dir, ledger); err != nil {
			return err
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		slog.Info("distill.part_uploaded", "part", name)
		return nil
	}

	f, err := os.Open(path)
	if err != nil {
		return err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	id, err := api.Upload(ctx, name, f, st.Size())
	f.Close()
	if err != nil {
		return err
	}
	ledger[name] = id
	if err := saveLedger(dir, ledger); err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	slog.Info("distill.part_uploaded", "part", name)
	return nil
}

// openPartBaseName is the writer's open part: the highest NNN for today's UTC date.
func openPartBaseName(dir string, now func() time.Time) string {
	if now == nil {
		now = time.Now
	}
	_, openPath, _, err := scanParts(dir, now)
	if err != nil || openPath == "" {
		return ""
	}
	return filepath.Base(openPath)
}
