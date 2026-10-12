package distill

import (
	"errors"
	"fmt"
	"io"
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

	hooks        atomic.Pointer[partHooks]
	beforeInsert func(seq uint64, path string)

	rejectEnqueue bool
	closing       bool
	drainDeadline time.Time
	acceptedMax   uint64

	closeOnce sync.Once
	closeCh   chan struct{}
	done      chan struct{}
}

type queueItem struct {
	line      []byte
	spoolPath string
	failed    bool
}

// partHooks lets tests inject a short write or a failing truncate.
type partHooks struct {
	write    func(*os.File, []byte) (int, error)
	truncate func(*os.File, int64) error
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
	lineCopy := append([]byte(nil), line...)

	w.mu.Lock()
	if w.rejectEnqueue {
		w.mu.Unlock()
		w.dropped.Add(1)
		return
	}
	seq := w.seq.Add(1)
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
	if w.beforeInsert != nil {
		w.beforeInsert(seq, finalPath)
	}
	w.insertSpool(seq, finalPath)
}

func (w *Writer) insertSpool(seq uint64, path string) {
	w.mu.Lock()
	if seq < w.nextWrite {
		w.mu.Unlock()
		_ = os.Remove(path)
		return
	}
	if _, exists := w.pending[seq]; exists {
		w.mu.Unlock()
		_ = os.Remove(path)
		return
	}
	w.pending[seq] = &queueItem{spoolPath: path}
	w.mu.Unlock()
	w.poke()
}

func (w *Writer) insertFailed(seq uint64) {
	w.mu.Lock()
	if seq < w.nextWrite {
		w.mu.Unlock()
		return
	}
	if _, exists := w.pending[seq]; exists {
		w.mu.Unlock()
		return
	}
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
	defer func() {
		if w.partFile != nil {
			_ = w.partFile.Close()
			w.partFile = nil
		}
		close(w.done)
	}()

	for {
		closing, deadline, accepted := w.closeState()
		if closing && !time.Now().Before(deadline) {
			w.abandonThrough(accepted)
			return
		}
		if w.tryWriteNext() {
			continue
		}
		if closing {
			if w.drained(accepted) {
				return
			}
			w.waitWork(deadline)
			continue
		}
		select {
		case <-w.closeCh:
		case <-w.notify:
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func (w *Writer) closeState() (closing bool, deadline time.Time, accepted uint64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.closing, w.drainDeadline, w.acceptedMax
}

func (w *Writer) drained(accepted uint64) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, ok := w.pending[w.nextWrite]; ok {
		return false
	}
	return w.nextWrite > accepted
}

func (w *Writer) waitWork(deadline time.Time) {
	d := time.Until(deadline)
	if d <= 0 {
		return
	}
	timer := time.NewTimer(d)
	select {
	case <-w.notify:
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
	case <-timer.C:
	}
}

func (w *Writer) tryWriteNext() bool {
	w.mu.Lock()
	item, ok := w.pending[w.nextWrite]
	if !ok {
		w.mu.Unlock()
		return false
	}
	delete(w.pending, w.nextWrite)
	if len(item.line) > 0 {
		w.memBytes -= int64(len(item.line))
	}
	w.nextWrite++
	w.mu.Unlock()
	w.commitItem(item)
	return true
}

func (w *Writer) commitItem(item *queueItem) {
	if item.failed {
		return
	}
	line := item.line
	if item.spoolPath != "" {
		data, err := os.ReadFile(item.spoolPath)
		_ = os.Remove(item.spoolPath)
		if err != nil {
			w.dropped.Add(1)
			return
		}
		line = data
	}
	if err := w.appendLine(line); err != nil {
		w.dropped.Add(1)
		return
	}
	w.written.Add(1)
}

func (w *Writer) abandonThrough(max uint64) {
	for {
		w.mu.Lock()
		if w.nextWrite > max {
			w.mu.Unlock()
			return
		}
		item, ok := w.pending[w.nextWrite]
		if ok {
			delete(w.pending, w.nextWrite)
			if len(item.line) > 0 {
				w.memBytes -= int64(len(item.line))
			}
		}
		w.nextWrite++
		w.mu.Unlock()
		if !ok {
			w.dropped.Add(1)
			continue
		}
		w.discard(item)
	}
}

func (w *Writer) discard(item *queueItem) {
	if item == nil {
		w.dropped.Add(1)
		return
	}
	if item.spoolPath != "" {
		_ = os.Remove(item.spoolPath)
	}
	if !item.failed {
		w.dropped.Add(1)
	}
}

func (w *Writer) appendLine(line []byte) error {
	if err := w.ensurePart(len(line)); err != nil {
		return err
	}
	prev := w.partSize
	n, err := w.writePart(w.partFile, line)
	if n < len(line) {
		if n > 0 {
			if rerr := w.rewindPart(prev); rerr != nil {
				w.abandonOpenPart()
			}
		}
		if err == nil {
			err = io.ErrShortWrite
		}
		return err
	}
	w.partSize += int64(n)
	return err
}

func (w *Writer) rewindPart(size int64) error {
	if w.partFile == nil {
		return errors.New("distill writer: no open part")
	}
	if err := w.truncatePart(w.partFile, size); err != nil {
		return err
	}
	if _, err := w.partFile.Seek(size, io.SeekStart); err != nil {
		return err
	}
	w.partSize = size
	return nil
}

func (w *Writer) ensurePart(lineLen int) error {
	today := utcDate(w.now())
	if w.partFile == nil {
		num := 1
		if w.partSeq > 0 && w.partDate == today {
			num = w.partSeq + 1
		}
		return w.openNewPart(today, num)
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

func (w *Writer) writePart(f *os.File, p []byte) (int, error) {
	if h := w.hooks.Load(); h != nil && h.write != nil {
		return h.write(f, p)
	}
	return f.Write(p)
}

func (w *Writer) truncatePart(f *os.File, size int64) error {
	if h := w.hooks.Load(); h != nil && h.truncate != nil {
		return h.truncate(f, size)
	}
	return f.Truncate(size)
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
	w.noteClosed(path)
	return nil
}

func (w *Writer) abandonOpenPart() {
	if w.partFile == nil {
		return
	}
	path := w.partPath
	_ = w.partFile.Close()
	w.partFile = nil
	w.partPath = ""
	w.partSize = 0
	w.noteClosed(path)
}

func (w *Writer) noteClosed(path string) {
	if path == "" {
		return
	}
	w.closedMu.Lock()
	defer w.closedMu.Unlock()
	for _, existing := range w.closedParts {
		if existing == path {
			return
		}
	}
	w.closedParts = append(w.closedParts, path)
}

func (w *Writer) isClosed(path string) bool {
	w.closedMu.RLock()
	defer w.closedMu.RUnlock()
	for _, existing := range w.closedParts {
		if existing == path {
			return true
		}
	}
	return false
}

func (w *Writer) openNewPart(date string, num int) error {
	if num < 1 {
		return fmt.Errorf("distill writer: invalid part number %d", num)
	}
	name := fmt.Sprintf("%s-%03d.jsonl", date, num)
	path := filepath.Join(w.dir, name)
	if w.isClosed(path) {
		return fmt.Errorf("distill writer: refusing to reopen closed part %s", path)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return err
	}
	w.partFile = f
	w.partPath = path
	w.partSize = st.Size()
	w.partDate = date
	w.partSeq = num
	return nil
}

func (w *Writer) Close() error {
	w.closeOnce.Do(func() {
		w.mu.Lock()
		w.rejectEnqueue = true
		w.acceptedMax = w.seq.Load()
		w.drainDeadline = time.Now().Add(2 * time.Second)
		w.closing = true
		w.mu.Unlock()
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
