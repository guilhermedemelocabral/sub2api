package distill

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
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
		Enabled:     true,
		Dir:         dir,
		QueueSize:   8,
		QueueBytes:  1 << 20,
		PartBytes:   1,
		DriveEnabled: true,
		Drive:       drive,
		Now:         now,
		Backoff:     func(int) time.Duration { return 0 },
		AutoUpload:  false,
	})
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
