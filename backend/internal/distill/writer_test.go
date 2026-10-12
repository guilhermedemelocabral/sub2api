package distill

import (
	"os"
	"path/filepath"
	"strings"
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

	w, err := NewWriter(dir, 1, 1<<20, 1<<20, now)
	require.NoError(t, err)

	line1 := []byte("first\n")
	line2 := []byte("second\n")
	w.Enqueue(line1)

	start := time.Now()
	w.Enqueue(line2)
	require.Less(t, time.Since(start), 200*time.Millisecond)

	big := make([]byte, 512)
	for i := range big {
		big[i] = 'x'
	}
	big[len(big)-1] = '\n'
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
