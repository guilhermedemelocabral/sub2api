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

func TestCollectorCloseDoesNotHangOnSlowDriveHTTP(t *testing.T) {
	dir := t.TempDir()
	closedPath := filepath.Join(dir, "2006-01-01-001.jsonl")
	require.NoError(t, os.WriteFile(closedPath, []byte("x\n"), 0o600))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(500 * time.Millisecond)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	drive := &HTTPDrive{
		FolderID:   testFolderID,
		Token:      testToken,
		UploadBase: srv.URL,
		DriveBase:  srv.URL,
		Client:     &http.Client{Timeout: 50 * time.Millisecond},
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
