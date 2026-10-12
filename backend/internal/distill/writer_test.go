package distill

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestWriterTwoSmallLinesSamePart(t *testing.T) {
	dir := t.TempDir()
	day := time.Date(2006, 1, 2, 12, 0, 0, 0, time.UTC)
	now := func() time.Time { return day }

	w, err := NewWriter(dir, 8, 1<<20, 1<<20, now)
	require.NoError(t, err)

	line1 := []byte("{\"a\":1}\n")
	line2 := []byte("{\"b\":2}\n")
	w.Enqueue(line1)
	w.Enqueue(line2)
	require.NoError(t, w.Close())

	part := filepath.Join(dir, "2006-01-02-001.jsonl")
	info, err := os.Stat(part)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0600), info.Mode().Perm())

	data, err := os.ReadFile(part)
	require.NoError(t, err)
	require.Equal(t, string(line1)+string(line2), string(data))
	require.Equal(t, int64(2), w.Written())
}

func TestWriterPartRotationBySize(t *testing.T) {
	dir := t.TempDir()
	day := time.Date(2006, 1, 2, 12, 0, 0, 0, time.UTC)
	now := func() time.Time { return day }

	line1 := []byte("0123456789\n") // 11 bytes
	partBytes := int64(12)          // next line would exceed

	w, err := NewWriter(dir, 8, 1<<20, partBytes, now)
	require.NoError(t, err)

	line2 := []byte("abcdefghijklmnop\n")
	w.Enqueue(line1)
	w.Enqueue(line2)
	require.NoError(t, w.Close())

	part1 := filepath.Join(dir, "2006-01-02-001.jsonl")
	part2 := filepath.Join(dir, "2006-01-02-002.jsonl")
	require.FileExists(t, part1)
	require.FileExists(t, part2)

	data2, err := os.ReadFile(part2)
	require.NoError(t, err)
	require.Equal(t, string(line2), string(data2))

	closed := w.ClosedParts()
	require.Contains(t, closed, part1)
}

func TestWriterPartRotationByUTCDate(t *testing.T) {
	dir := t.TempDir()
	day := time.Date(2006, 1, 2, 23, 0, 0, 0, time.UTC)
	var current time.Time
	current = day
	now := func() time.Time { return current }

	w, err := NewWriter(dir, 8, 1<<20, 1<<30, now)
	require.NoError(t, err)

	line1 := []byte("day-one\n")
	w.Enqueue(line1)
	require.Eventually(t, func() bool { return w.Written() >= 1 }, time.Second, 5*time.Millisecond)

	current = time.Date(2006, 1, 3, 0, 0, 0, 0, time.UTC)
	line2 := []byte("day-two\n")
	w.Enqueue(line2)
	require.NoError(t, w.Close())

	part1 := filepath.Join(dir, "2006-01-02-001.jsonl")
	part2 := filepath.Join(dir, "2006-01-03-001.jsonl")
	require.FileExists(t, part1)
	require.FileExists(t, part2)

	data1, _ := os.ReadFile(part1)
	require.Equal(t, string(line1), string(data1))
	data2, _ := os.ReadFile(part2)
	require.Equal(t, string(line2), string(data2))

	closed := w.ClosedParts()
	require.Contains(t, closed, part1)
}

func TestWriterQueueSpillAndClose(t *testing.T) {
	dir := t.TempDir()
	day := time.Date(2006, 1, 2, 12, 0, 0, 0, time.UTC)
	now := func() time.Time { return day }

	const queueBytes = int64(32)
	w, err := NewWriter(dir, 1, queueBytes, 1<<20, now)
	require.NoError(t, err)

	line1 := []byte("first\n")
	line2 := []byte("second\n")
	w.Enqueue(line1)

	start := time.Now()
	w.Enqueue(line2)
	require.Less(t, time.Since(start), 200*time.Millisecond)

	big := make([]byte, queueBytes+1)
	for i := range big {
		big[i] = 'x'
	}
	big[len(big)-1] = '\n'
	require.Greater(t, int64(len(big)), queueBytes)
	start = time.Now()
	w.Enqueue(big)
	require.Less(t, time.Since(start), 200*time.Millisecond)

	require.NoError(t, w.Close())

	var found strings.Builder
	err = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !strings.HasSuffix(path, ".jsonl") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		found.Write(data)
		return nil
	})
	require.NoError(t, err)
	body := found.String()
	require.Contains(t, body, string(line1))
	require.Contains(t, body, string(line2))
	require.Contains(t, body, string(big))
}

func TestNewWriterDirIsFile(t *testing.T) {
	f := filepath.Join(t.TempDir(), "notadir")
	require.NoError(t, os.WriteFile(f, []byte("x"), 0600))

	_, err := NewWriter(f, 8, 1<<20, 1<<20, time.Now)
	require.Error(t, err)
}

func TestWriterCloseIdleReturnsPromptly(t *testing.T) {
	dir := t.TempDir()
	day := time.Date(2006, 1, 2, 12, 0, 0, 0, time.UTC)
	w, err := NewWriter(dir, 8, 1<<20, 1<<20, func() time.Time { return day })
	require.NoError(t, err)

	start := time.Now()
	require.NoError(t, w.Close())
	require.Less(t, time.Since(start), 500*time.Millisecond)
}

func TestWriterCloseReturnsWhenQueueDrains(t *testing.T) {
	dir := t.TempDir()
	day := time.Date(2006, 1, 2, 12, 0, 0, 0, time.UTC)
	w, err := NewWriter(dir, 8, 1<<20, 1<<20, func() time.Time { return day })
	require.NoError(t, err)

	w.Enqueue([]byte("one\n"))
	w.Enqueue([]byte("two\n"))

	start := time.Now()
	require.NoError(t, w.Close())
	require.Less(t, time.Since(start), 500*time.Millisecond)
	require.Equal(t, int64(2), w.Written())
	require.Equal(t, int64(0), w.Dropped())
}

func TestWriterCloseCapsDespiteEnqueue(t *testing.T) {
	dir := t.TempDir()
	day := time.Date(2006, 1, 2, 12, 0, 0, 0, time.UTC)
	w, err := NewWriter(dir, 10000, 32<<20, 1<<30, func() time.Time { return day })
	require.NoError(t, err)

	stop := make(chan struct{})
	defer close(stop)
	go func() {
		line := []byte("{\"k\":1}\n")
		for {
			select {
			case <-stop:
				return
			default:
			}
			w.mu.Lock()
			n := len(w.pending)
			w.mu.Unlock()
			if n >= 64 {
				runtime.Gosched()
				continue
			}
			w.Enqueue(line)
		}
	}()
	require.Eventually(t, func() bool { return w.Written() > 0 }, time.Second, 5*time.Millisecond)

	start := time.Now()
	done := make(chan struct{})
	go func() {
		_ = w.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Close did not return within 2s while Enqueue continued")
	}
	require.Less(t, time.Since(start), 3*time.Second)
}

func TestWriterRotatePartCloseErrorAdvancesState(t *testing.T) {
	dir := t.TempDir()
	day := time.Date(2006, 1, 2, 12, 0, 0, 0, time.UTC)
	line1 := []byte("0123456789\n")
	w, err := NewWriter(dir, 8, 1<<20, 12, func() time.Time { return day })
	require.NoError(t, err)

	var closeCalls int
	var closeMu sync.Mutex
	w.hooks.Store(&partHooks{
		close: func(f *os.File) error {
			closeMu.Lock()
			closeCalls++
			n := closeCalls
			closeMu.Unlock()
			if n == 1 {
				if err := f.Close(); err != nil {
					return err
				}
				return errors.New("close failed")
			}
			return f.Close()
		},
	})

	w.Enqueue(line1)
	require.Eventually(t, func() bool { return w.Written() >= 1 }, time.Second, 5*time.Millisecond)

	line2 := []byte("abcdefghijklmnop\n")
	line3 := []byte("x") // fits in part 001 (11+1) but must land in part 002 after failed rotate
	w.Enqueue(line2)
	w.Enqueue(line3)
	require.NoError(t, w.Close())

	part1 := filepath.Join(dir, "2006-01-02-001.jsonl")
	part2 := filepath.Join(dir, "2006-01-02-002.jsonl")
	require.Contains(t, w.ClosedParts(), part1)

	data1, err := os.ReadFile(part1)
	require.NoError(t, err)
	require.Equal(t, string(line1), string(data1))

	require.FileExists(t, part2)
	data2, err := os.ReadFile(part2)
	require.NoError(t, err)
	require.Equal(t, string(line3), string(data2))
	require.NotContains(t, string(data2), string(line2))

	require.Equal(t, int64(2), w.Written())
	require.Equal(t, int64(1), w.Dropped())
}

func TestWriterFailedRotationDoesNotReopenClosedPart(t *testing.T) {
	dir := t.TempDir()
	day := time.Date(2006, 1, 2, 12, 0, 0, 0, time.UTC)
	line1 := []byte("0123456789\n")
	w, err := NewWriter(dir, 8, 1<<20, 12, func() time.Time { return day })
	require.NoError(t, err)

	w.Enqueue(line1)
	require.Eventually(t, func() bool { return w.Written() >= 1 }, time.Second, 5*time.Millisecond)

	require.NoError(t, os.Mkdir(filepath.Join(dir, "2006-01-02-002.jsonl"), 0700))

	line2 := []byte("abcdefghijklmnop\n")
	line3 := []byte("ZZZZZZZZZZZZZZZZ\n")
	w.Enqueue(line2)
	w.Enqueue(line3)
	require.NoError(t, w.Close())

	part1 := filepath.Join(dir, "2006-01-02-001.jsonl")
	data, err := os.ReadFile(part1)
	require.NoError(t, err)
	require.Equal(t, string(line1), string(data))
	require.Contains(t, w.ClosedParts(), part1)
	require.GreaterOrEqual(t, w.Dropped(), int64(2))
}

func TestWriterShortWriteTruncatesBeforeNextLine(t *testing.T) {
	dir := t.TempDir()
	day := time.Date(2006, 1, 2, 12, 0, 0, 0, time.UTC)
	w, err := NewWriter(dir, 4, 1<<20, 1<<20, func() time.Time { return day })
	require.NoError(t, err)

	var calls int
	var callsMu sync.Mutex
	w.hooks.Store(&partHooks{
		write: func(f *os.File, p []byte) (int, error) {
			callsMu.Lock()
			calls++
			n := calls
			callsMu.Unlock()
			if n == 2 {
				cut := 4
				if cut > len(p) {
					cut = len(p)
				}
				nw, werr := f.Write(p[:cut])
				if werr != nil {
					return nw, werr
				}
				return nw, io.ErrShortWrite
			}
			return f.Write(p)
		},
	})

	good := []byte("good\n")
	next := []byte("next-line\n")
	w.Enqueue(good)
	w.Enqueue([]byte("BADLINE\n"))
	w.Enqueue(next)
	require.NoError(t, w.Close())

	data, err := os.ReadFile(filepath.Join(dir, "2006-01-02-001.jsonl"))
	require.NoError(t, err)
	require.Equal(t, string(good)+string(next), string(data))
	require.Equal(t, int64(1), w.Dropped())
	require.Equal(t, int64(2), w.Written())
}

func TestWriterShortWriteAbandonsPartWhenTruncateFails(t *testing.T) {
	dir := t.TempDir()
	day := time.Date(2006, 1, 2, 12, 0, 0, 0, time.UTC)
	w, err := NewWriter(dir, 4, 1<<20, 1<<20, func() time.Time { return day })
	require.NoError(t, err)

	good := []byte("good\n")
	bad := []byte("BADLINE\n")
	next := []byte("next-line\n")
	torn := []byte("TORN")
	w.hooks.Store(&partHooks{
		write: func(f *os.File, p []byte) (int, error) {
			if bytes.Equal(p, bad) {
				n, werr := f.Write(torn)
				if werr != nil {
					return n, werr
				}
				return n, io.ErrShortWrite
			}
			return f.Write(p)
		},
		truncate: func(*os.File, int64) error {
			return errors.New("truncate failed")
		},
	})

	w.Enqueue(good)
	w.Enqueue(bad)
	w.Enqueue(next)
	require.NoError(t, w.Close())

	part1 := filepath.Join(dir, "2006-01-02-001.jsonl")
	part2 := filepath.Join(dir, "2006-01-02-002.jsonl")
	data1, err := os.ReadFile(part1)
	require.NoError(t, err)
	require.Equal(t, string(good)+string(torn), string(data1))
	require.NotContains(t, string(data1), string(next))
	require.Contains(t, w.ClosedParts(), part1)

	data2, err := os.ReadFile(part2)
	require.NoError(t, err)
	require.Equal(t, string(next), string(data2))
	require.Equal(t, int64(1), w.Dropped())
	require.NotContains(t, w.ClosedParts(), part2)
}

func TestWriterLateSpoolAfterAbandonDeletesFile(t *testing.T) {
	dir := t.TempDir()
	day := time.Date(2006, 1, 2, 12, 0, 0, 0, time.UTC)
	w, err := NewWriter(dir, 1, 8, 1<<20, func() time.Time { return day })
	require.NoError(t, err)

	release := make(chan struct{})
	entered := make(chan struct{})
	w.beforeInsert = func(seq uint64, path string) {
		select {
		case <-entered:
		default:
			close(entered)
		}
		<-release
	}

	line := append(bytes.Repeat([]byte("y"), 64), '\n')
	w.Enqueue(line)

	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("spill did not reach insert")
	}

	start := time.Now()
	require.NoError(t, w.Close())
	elapsed := time.Since(start)
	require.Greater(t, elapsed, time.Second)
	require.Less(t, elapsed, 3*time.Second)
	require.Equal(t, int64(1), w.Dropped())

	close(release)

	require.Eventually(t, func() bool {
		matches, err := filepath.Glob(filepath.Join(dir, "spool", "*"))
		return err == nil && len(matches) == 0
	}, time.Second, 10*time.Millisecond)
	require.Equal(t, int64(1), w.Dropped())
}

func TestWriterLateFailedInsertDoesNotDropTwice(t *testing.T) {
	dir := t.TempDir()
	day := time.Date(2006, 1, 2, 12, 0, 0, 0, time.UTC)
	w, err := NewWriter(dir, 4, 1<<20, 1<<20, func() time.Time { return day })
	require.NoError(t, err)

	w.mu.Lock()
	w.nextWrite = 2
	w.mu.Unlock()
	w.dropped.Store(1)

	w.insertFailed(1)

	require.Equal(t, int64(1), w.Dropped())
	w.mu.Lock()
	_, exists := w.pending[1]
	w.mu.Unlock()
	require.False(t, exists)
	require.NoError(t, w.Close())
}
