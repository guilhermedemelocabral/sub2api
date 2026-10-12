package distill

import (
	"context"
	"log/slog"
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
	uploadMu        sync.Mutex

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

// NoteNormalizeDrop counts a turn rejected by Normalize and logs distill.turn_dropped
// with reason=normalize only. The request body is not logged.
func (c *Collector) NoteNormalizeDrop() {
	if c == nil || c.writer == nil {
		return
	}
	c.writer.dropped.Add(1)
	slog.Warn("distill.turn_dropped", "reason", "normalize")
}

func (c *Collector) Close() error {
	if c == nil {
		return nil
	}
	c.stopOnce.Do(func() {
		close(c.stopCh)
		// Cancel in-flight Drive uploads; upload HTTP has no overall timeout.
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
			c.uploadMu.Lock()
			err := Reconcile(c.uploadCtx, c.dir, c.now, c.drive)
			c.uploadMu.Unlock()
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
				if c.driveEnabled && c.drive != nil && !c.reconcileSynced.Load() {
					continue
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
		c.uploadMu.Lock()
		err := uploadPartOnce(ctx, c.dir, path, c.drive)
		c.uploadMu.Unlock()
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
	return uploadOrAdoptPart(ctx, dir, path, api)
}
