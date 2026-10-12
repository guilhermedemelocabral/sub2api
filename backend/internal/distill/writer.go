package distill

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type Writer struct {
	dir        string
	queueSize  int
	queueBytes int64
	partBytes  int64
	now        func() time.Time

	seq atomic.Uint64

	mu       sync.Mutex
	pending  map[uint64]*queueItem
	memBytes int64
	notify   chan struct{}

	closedParts []string
	closedMu    sync.RWMutex

	nextWrite uint64

	dropped atomic.Int64
	written atomic.Int64

	partFile *os.File
	partPath string
	partSize int64
	partDate string
	partSeq  int

	closeOnce sync.Once
	closeCh   chan struct{}
	done      chan struct{}
}

type queueItem struct {
	line      []byte
	spoolPath string
	failed    bool
}

func NewWriter(dir string, queueSize int, queueBytes, partBytes int64, now func() time.Time) (*Writer, error) {
	if now == nil {
		now = time.Now
	}
	info, err := os.Stat(dir)
	if err == nil {
		if !info.IsDir() {
			return nil, fmt.Errorf("distill writer: %q is not a directory", dir)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	} else if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}

	w := &Writer{
		dir:        dir,
		queueSize:  queueSize,
		queueBytes: queueBytes,
		partBytes:  partBytes,
		now:        now,
		pending:    make(map[uint64]*queueItem),
		notify:     make(chan struct{}, 1),
		closeCh:    make(chan struct{}),
		done:       make(chan struct{}),
		nextWrite:  1,
	}

	closed, openPath, openSize, err := scanParts(dir, now)
	if err != nil {
		return nil, err
	}
	w.closedParts = closed

	if openPath != "" {
		f, err := os.OpenFile(openPath, os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			return nil, err
		}
		w.partFile = f
		w.partPath = openPath
		w.partSize = openSize
		w.partDate = utcDate(now())
		base := strings.TrimSuffix(filepath.Base(openPath), ".jsonl")
		if idx := strings.LastIndex(base, "-"); idx >= 0 {
			if n, err := strconv.Atoi(base[idx+1:]); err == nil {
				w.partSeq = n
			}
		}
	}

	go w.run()
	return w, nil
}

func utcDate(t time.Time) string {
	return t.UTC().Format("2006-01-02")
}

func scanParts(dir string, now func() time.Time) (closed []string, openPath string, openSize int64, err error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, "", 0, err
	}
	today := utcDate(now())
	type part struct {
		path string
		date string
		num  int
	}
	var parts []part
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		base := strings.TrimSuffix(name, ".jsonl")
		idx := strings.LastIndex(base, "-")
		if idx < 0 {
			continue
		}
		date := base[:idx]
		numStr := base[idx+1:]
		num, err := strconv.Atoi(numStr)
		if err != nil || num < 1 {
			continue
		}
		parts = append(parts, part{
			path: filepath.Join(dir, name),
			date: date,
			num:  num,
		})
	}
	sort.Slice(parts, func(i, j int) bool {
		if parts[i].date != parts[j].date {
			return parts[i].date < parts[j].date
		}
		return parts[i].num < parts[j].num
	})

	var best *part
	for i := range parts {
		p := &parts[i]
		if p.date == today {
			if best == nil || p.num > best.num {
				best = p
			}
		} else {
			closed = append(closed, p.path)
		}
	}
	for i := range parts {
		p := &parts[i]
		if p.date == today && (best == nil || p.path != best.path) {
			closed = append(closed, p.path)
		}
	}
	if best != nil {
		st, err := os.Stat(best.path)
		if err != nil {
			return nil, "", 0, err
		}
		return closed, best.path, st.Size(), nil
	}
	return closed, "", 0, nil
}

func (w *Writer) Enqueue(line []byte) {
	seq := w.seq.Add(1)
	lineCopy := append([]byte(nil), line...)

	w.mu.Lock()
	if len(w.pending) < w.queueSize && w.memBytes+int64(len(lineCopy)) <= w.queueBytes {
		w.pending[seq] = &queueItem{line: lineCopy}
		w.memBytes += int64(len(lineCopy))
		w.mu.Unlock()
		w.poke()
		return
	}
	w.mu.Unlock()

	go w.spillToDisk(seq, lineCopy)
}

func (w *Writer) spillToDisk(seq uint64, line []byte) {
	spoolDir := filepath.Join(w.dir, "spool")
	if err := os.MkdirAll(spoolDir, 0700); err != nil {
		w.insertFailed(seq)
		return
	}
	finalPath := filepath.Join(spoolDir, fmt.Sprintf("%d.json", seq))
	tmpPath := finalPath + ".tmp"
	f, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		w.insertFailed(seq)
		return
	}
	if _, err := f.Write(line); err != nil {
		f.Close()
		os.Remove(tmpPath)
		w.insertFailed(seq)
		return
	}
	if err := f.Close(); err != nil {
		os.Remove(tmpPath)
		w.insertFailed(seq)
		return
	}
	if err := os.Rename(tmpPath, finalPath); err != nil {
		os.Remove(tmpPath)
		w.insertFailed(seq)
		return
	}

	w.mu.Lock()
	w.pending[seq] = &queueItem{spoolPath: finalPath}
	w.mu.Unlock()
	w.poke()
}

func (w *Writer) insertFailed(seq uint64) {
	w.mu.Lock()
	w.pending[seq] = &queueItem{failed: true}
	w.mu.Unlock()
	w.dropped.Add(1)
	w.poke()
}

func (w *Writer) poke() {
	select {
	case w.notify <- struct{}{}:
	default:
	}
}

func (w *Writer) run() {
	defer close(w.done)
	var closing bool
	var drainDeadline time.Time

	for {
		written := w.tryWriteNext(closing, drainDeadline)
		if written {
			continue
		}

		if closing && time.Now().After(drainDeadline) {
			w.abandonRemaining()
			if w.partFile != nil {
				w.partFile.Close()
				w.partFile = nil
			}
			return
		}

		if closing {
			time.Sleep(5 * time.Millisecond)
			continue
		}

		select {
		case <-w.closeCh:
			closing = true
			drainDeadline = time.Now().Add(2 * time.Second)
		case <-w.notify:
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func (w *Writer) tryWriteNext(closing bool, drainDeadline time.Time) bool {
	w.mu.Lock()
	item, ok := w.pending[w.nextWrite]
	if !ok {
		w.mu.Unlock()
		if closing && time.Now().After(drainDeadline) {
			max := w.seq.Load()
			if w.nextWrite <= max {
				w.dropped.Add(1)
				w.nextWrite++
				return true
			}
		}
		return false
	}
	delete(w.pending, w.nextWrite)
	if len(item.line) > 0 {
		w.memBytes -= int64(len(item.line))
	}
	seq := w.nextWrite
	w.nextWrite++
	w.mu.Unlock()

	if item.failed {
		return true
	}

	line := item.line
	if item.spoolPath != "" {
		data, err := os.ReadFile(item.spoolPath)
		os.Remove(item.spoolPath)
		if err != nil {
			w.dropped.Add(1)
			return true
		}
		line = data
	}

	if err := w.appendLine(line); err != nil {
		w.dropped.Add(1)
		return true
	}
	w.written.Add(1)
	_ = seq
	return true
}

func (w *Writer) abandonRemaining() {
	max := w.seq.Load()
	for w.nextWrite <= max {
		w.mu.Lock()
		item, ok := w.pending[w.nextWrite]
		if ok {
			delete(w.pending, w.nextWrite)
			if len(item.line) > 0 {
				w.memBytes -= int64(len(item.line))
			}
			w.mu.Unlock()
			if !item.failed {
				line := item.line
				if item.spoolPath != "" {
					data, err := os.ReadFile(item.spoolPath)
					os.Remove(item.spoolPath)
					if err != nil {
						w.dropped.Add(1)
						w.nextWrite++
						continue
					}
					line = data
				}
				if err := w.appendLine(line); err != nil {
					w.dropped.Add(1)
				} else {
					w.written.Add(1)
				}
			}
			w.nextWrite++
			continue
		}
		w.mu.Unlock()
		w.dropped.Add(1)
		w.nextWrite++
	}
}

func (w *Writer) appendLine(line []byte) error {
	if err := w.ensurePart(len(line)); err != nil {
		return err
	}
	n, err := w.partFile.Write(line)
	if err != nil {
		return err
	}
	w.partSize += int64(n)
	return nil
}

func (w *Writer) ensurePart(lineLen int) error {
	today := utcDate(w.now())
	if w.partFile == nil {
		return w.openNewPart(today, 1)
	}
	if today != w.partDate {
		if err := w.rotatePart(); err != nil {
			return err
		}
		return w.openNewPart(today, 1)
	}
	if w.partSize+int64(lineLen) > w.partBytes {
		if err := w.rotatePart(); err != nil {
			return err
		}
		return w.openNextPartSameDay()
	}
	return nil
}

func (w *Writer) openNextPartSameDay() error {
	return w.openNewPart(w.partDate, w.partSeq+1)
}

func (w *Writer) rotatePart() error {
	if w.partFile == nil {
		return nil
	}
	path := w.partPath
	if err := w.partFile.Close(); err != nil {
		return err
	}
	w.partFile = nil
	w.partPath = ""
	w.partSize = 0
	w.closedMu.Lock()
	w.closedParts = append(w.closedParts, path)
	w.closedMu.Unlock()
	return nil
}

func (w *Writer) openNewPart(date string, num int) error {
	name := fmt.Sprintf("%s-%03d.jsonl", date, num)
	path := filepath.Join(w.dir, name)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	w.partFile = f
	w.partPath = path
	st, err := f.Stat()
	if err != nil {
		return err
	}
	w.partSize = st.Size()
	w.partDate = date
	w.partSeq = num
	return nil
}

func (w *Writer) Close() error {
	w.closeOnce.Do(func() {
		close(w.closeCh)
	})
	<-w.done
	return nil
}

func (w *Writer) Dropped() int64 {
	return w.dropped.Load()
}

func (w *Writer) Written() int64 {
	return w.written.Load()
}

func (w *Writer) ClosedParts() []string {
	w.closedMu.RLock()
	defer w.closedMu.RUnlock()
	out := make([]string, len(w.closedParts))
	copy(out, w.closedParts)
	return out
}
