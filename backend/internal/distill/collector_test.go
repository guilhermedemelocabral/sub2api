package distill

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type failOnceDrive struct {
	uploads atomic.Int32
	id      string
}

func (f *failOnceDrive) Upload(ctx context.Context, name string, r io.ReadSeeker, size int64) (string, error) {
	if f.uploads.Add(1) == 1 {
		return "", errors.New("transient upload failure")
	}
	return f.id, nil
}

func (f *failOnceDrive) FindByName(ctx context.Context, name string) (string, bool, error) {
	return "", false, nil
}

func TestCollectorUploadsClosedPartAfterRetry(t *testing.T) {
	dir := t.TempDir()
	day := time.Date(2006, 1, 2, 12, 0, 0, 0, time.UTC)
	now := func() time.Time { return day }

	drive := &failOnceDrive{id: "drive-file-123"}
	c := NewCollector(CollectorConfig{
		Enabled:      true,
		Dir:          dir,
		QueueSize:    8,
		QueueBytes:   1 << 20,
		PartBytes:    1,
		DriveEnabled: false,
		Now:          now,
		Backoff:      func(int) time.Duration { return 0 },
		AutoUpload:   false,
	})
	c.driveEnabled = true
	c.drive = drive
	require.True(t, c.Enabled())
	t.Cleanup(func() { _ = c.Close() })

	line1 := []byte("a\n")
	line2 := []byte("b\n")
	c.Enqueue(line1)
	c.Enqueue(line2)

	closedPart := filepath.Join(dir, "2006-01-02-001.jsonl")
	require.Eventually(t, func() bool { return c.Written() >= 2 }, 10*time.Second, 2*time.Millisecond)

	var closedPath string
	require.Eventually(t, func() bool {
		select {
		case closedPath = <-c.ClosedForUpload():
			return closedPath != ""
		default:
			_, err := os.Stat(closedPart)
			return err == nil
		}
	}, 10*time.Second, 2*time.Millisecond)
	if closedPath == "" {
		closedPath = closedPart
	}
	require.FileExists(t, closedPath)

	require.NoError(t, c.uploadPartWithRetry(context.Background(), closedPath))
	require.NoFileExists(t, closedPath)

	ledger, err := loadLedger(dir)
	require.NoError(t, err)
	require.Equal(t, "drive-file-123", ledger[filepath.Base(closedPath)])
	require.Equal(t, int32(2), drive.uploads.Load())
}

func TestCollectorDriveDisabledKeepsClosedPart(t *testing.T) {
	dir := t.TempDir()
	day := time.Date(2006, 1, 2, 12, 0, 0, 0, time.UTC)
	now := func() time.Time { return day }

	closedPath := filepath.Join(dir, "2006-01-01-001.jsonl")
	require.NoError(t, os.WriteFile(closedPath, []byte("closed-day\n"), 0o600))

	c := NewCollector(CollectorConfig{
		Enabled:      true,
		Dir:          dir,
		QueueSize:    8,
		QueueBytes:   1 << 20,
		PartBytes:    1 << 20,
		DriveEnabled: false,
		Now:          now,
	})
	require.True(t, c.Enabled())
	t.Cleanup(func() { _ = c.Close() })

	c.Enqueue([]byte("today\n"))
	require.Eventually(t, func() bool { return c.Written() >= 1 }, 10*time.Second, 2*time.Millisecond)

	require.FileExists(t, closedPath)
	_, err := os.Stat(filepath.Join(dir, "uploaded.json"))
	require.True(t, os.IsNotExist(err))
}

func TestCollectorLogsAsyncTurnDropped(t *testing.T) {
	dir := t.TempDir()

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	day := time.Date(2006, 1, 2, 12, 0, 0, 0, time.UTC)
	c := NewCollector(CollectorConfig{
		Enabled:    true,
		Dir:        dir,
		QueueSize:  8,
		QueueBytes: 1 << 20,
		PartBytes:  1 << 20,
		Now:        func() time.Time { return day },
	})
	require.True(t, c.Enabled())
	c.writer.hooks.Store(&partHooks{
		write: func(*os.File, []byte) (int, error) {
			return 0, errors.New("disk full")
		},
	})
	t.Cleanup(func() { _ = c.Close() })

	c.Enqueue([]byte("line-body-must-not-log\n"))

	require.Eventually(t, func() bool {
		return strings.Contains(buf.String(), "distill.turn_dropped") &&
			strings.Contains(buf.String(), `"reason":"append"`) &&
			strings.Contains(buf.String(), `"dropped":1`)
	}, 5*time.Second, 10*time.Millisecond)
	require.NotContains(t, buf.String(), "line-body-must-not-log")
}

type failNTimesDrive struct {
	remaining atomic.Int32
	id        string
}

func (f *failNTimesDrive) Upload(ctx context.Context, name string, r io.ReadSeeker, size int64) (string, error) {
	if f.remaining.Add(-1) >= 0 {
		return "", errors.New("transient upload failure")
	}
	return f.id, nil
}

func (f *failNTimesDrive) FindByName(ctx context.Context, name string) (string, bool, error) {
	return "", false, nil
}

func TestCollectorStartupReconcileRetriesUntilSuccess(t *testing.T) {
	dir := t.TempDir()
	closedName := "2006-01-01-001.jsonl"
	closedPath := filepath.Join(dir, closedName)
	require.NoError(t, os.WriteFile(closedPath, []byte("closed\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "2006-01-02-001.jsonl"), []byte("open\n"), 0o600))

	drive := &failNTimesDrive{id: "remote-id"}
	drive.remaining.Store(2)

	day := time.Date(2006, 1, 2, 12, 0, 0, 0, time.UTC)
	c := NewCollector(CollectorConfig{
		Enabled:      true,
		Dir:          dir,
		QueueSize:    8,
		QueueBytes:   1 << 20,
		PartBytes:    1 << 20,
		DriveEnabled: true,
		Drive:        drive,
		Now:          func() time.Time { return day },
		Backoff:      func(int) time.Duration { return 0 },
		AutoUpload:   false,
	})
	require.True(t, c.Enabled())
	t.Cleanup(func() { _ = c.Close() })

	require.Eventually(t, func() bool {
		_, err := os.Stat(closedPath)
		return os.IsNotExist(err)
	}, 5*time.Second, 10*time.Millisecond)

	ledger, err := loadLedger(dir)
	require.NoError(t, err)
	require.Equal(t, "remote-id", ledger[closedName])
}

type recordingFailOnceDrive struct {
	id    string
	calls atomic.Int32
	mu    sync.Mutex
	names []string
}

func (d *recordingFailOnceDrive) Upload(ctx context.Context, name string, r io.ReadSeeker, size int64) (string, error) {
	d.mu.Lock()
	d.names = append(d.names, name)
	d.mu.Unlock()
	if d.calls.Add(1) == 1 {
		return "", errors.New("transient upload failure")
	}
	return d.id, nil
}

func (d *recordingFailOnceDrive) FindByName(context.Context, string) (string, bool, error) {
	return "", false, nil
}

func (d *recordingFailOnceDrive) uploaded() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]string, len(d.names))
	copy(out, d.names)
	return out
}

func TestCollectorReconcileRetrySkipsPartOpenedDuringBackoff(t *testing.T) {
	dir := t.TempDir()
	closedName := "2006-01-01-001.jsonl"
	closedPath := filepath.Join(dir, closedName)
	require.NoError(t, os.WriteFile(closedPath, []byte("closed-yesterday\n"), 0o600))

	openName := "2006-01-02-001.jsonl"
	openPath := filepath.Join(dir, openName)
	today := time.Date(2006, 1, 2, 12, 0, 0, 0, time.UTC)

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	drive := &recordingFailOnceDrive{id: "remote-id"}
	c := NewCollector(CollectorConfig{
		Enabled:      true,
		Dir:          dir,
		QueueSize:    8,
		QueueBytes:   1 << 20,
		PartBytes:    1 << 20,
		DriveEnabled: true,
		Drive:        drive,
		Now:          func() time.Time { return today },
		Backoff: func(attempt int) time.Duration {
			if attempt == 0 {
				if err := os.WriteFile(openPath, []byte("open-today\n"), 0o600); err != nil {
					t.Errorf("write open part: %v", err)
				}
			}
			return 0
		},
		AutoUpload: false,
	})
	require.True(t, c.Enabled())
	t.Cleanup(func() { _ = c.Close() })

	require.Eventually(t, func() bool {
		if _, err := os.Stat(closedPath); !os.IsNotExist(err) {
			return false
		}
		return strings.Contains(buf.String(), "distill.part_uploaded")
	}, 5*time.Second, 5*time.Millisecond)

	body, err := os.ReadFile(openPath)
	require.NoError(t, err)
	require.Equal(t, "open-today\n", string(body))

	ledger, err := loadLedger(dir)
	require.NoError(t, err)
	require.Equal(t, "remote-id", ledger[closedName])
	_, openInLedger := ledger[openName]
	require.False(t, openInLedger)

	require.Equal(t, []string{closedName, closedName}, drive.uploaded())

	logs := buf.String()
	require.Contains(t, logs, "distill.part_uploaded")
	require.Contains(t, logs, closedName)
	require.NotContains(t, logs, openName)
	require.NotContains(t, logs, "open-today")
	require.NotContains(t, logs, "closed-yesterday")
}

func TestCollectorPartClosedDuringReconcileRetryUploadedOnce(t *testing.T) {
	dir := t.TempDir()
	yesterdayName := "2006-01-01-001.jsonl"
	require.NoError(t, os.WriteFile(filepath.Join(dir, yesterdayName), []byte("yesterday\n"), 0o600))

	today := time.Date(2006, 1, 2, 12, 0, 0, 0, time.UTC)
	var c *Collector
	var midRetryPart string

	drive := &recordingFailOnceDrive{id: "remote-id"}
	c = NewCollector(CollectorConfig{
		Enabled:      true,
		Dir:          dir,
		QueueSize:    8,
		QueueBytes:   1 << 20,
		PartBytes:    1,
		DriveEnabled: true,
		Drive:        drive,
		Now:          func() time.Time { return today },
		Backoff: func(attempt int) time.Duration {
			if attempt == 0 && c != nil {
				c.Enqueue([]byte("a\n"))
				c.Enqueue([]byte("b\n"))
				midRetryPart = filepath.Join(dir, "2006-01-02-001.jsonl")
			}
			return 0
		},
		AutoUpload: true,
	})
	require.True(t, c.Enabled())
	t.Cleanup(func() { _ = c.Close() })

	require.Eventually(t, func() bool {
		if midRetryPart == "" {
			return false
		}
		_, err := os.Stat(midRetryPart)
		return os.IsNotExist(err)
	}, 5*time.Second, 5*time.Millisecond)

	require.NotEmpty(t, midRetryPart)
	partBase := filepath.Base(midRetryPart)

	uploaded := drive.uploaded()
	var count int
	for _, name := range uploaded {
		if name == partBase {
			count++
		}
	}
	require.Equal(t, 1, count, "uploaded names: %v", uploaded)

	ledger, err := loadLedger(dir)
	require.NoError(t, err)
	require.Equal(t, "remote-id", ledger[partBase])
}

func TestCollectorCloseDoesNotHangOnSlowDriveHTTP(t *testing.T) {
	dir := t.TempDir()
	closedPath := filepath.Join(dir, "2006-01-01-001.jsonl")
	require.NoError(t, os.WriteFile(closedPath, []byte("x\n"), 0o600))

	require.Equal(t, time.Duration(0), DefaultDriveUploadHTTPClient().Timeout)

	entered := make(chan struct{})
	var once sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(entered) })
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Minute):
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	drive := &HTTPDrive{
		FolderID:     testFolderID,
		Token:        testToken,
		UploadBase:   srv.URL,
		DriveBase:    srv.URL,
		UploadClient: DefaultDriveUploadHTTPClient(),
	}

	day := time.Date(2006, 1, 2, 12, 0, 0, 0, time.UTC)
	c := NewCollector(CollectorConfig{
		Enabled:      true,
		Dir:          dir,
		QueueSize:    8,
		QueueBytes:   1 << 20,
		PartBytes:    1 << 20,
		DriveEnabled: true,
		Drive:        drive,
		Now:          func() time.Time { return day },
		Backoff:      func(int) time.Duration { return time.Hour },
		AutoUpload:   true,
	})
	require.True(t, c.Enabled())

	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("Drive request did not start")
	}

	done := make(chan struct{})
	go func() {
		_ = c.Close()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close hung on in-flight Drive request")
	}
}
