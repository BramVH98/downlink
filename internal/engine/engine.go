/*
Package engine wires together the WAL(robustness), the memtable/logstore(fast in mem query),
and segments into one coherent store
*/
package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"homelab-tsdb/internal/storage"
	"homelab-tsdb/internal/wal"
)

type Engine struct {
	mu sync.RWMutex

	dataDir string
	segDir  string
	walPath string

	w   *wal.WAL
	mem *storage.Memtable
	log *storage.LogStore

	coldPoints []storage.Point
	coldLogs   []storage.LogEntry

	flushThreshold int
	nextLogID      int64 //atomic assigns stable, increasing IDs
}

// Open creates datadir if needed, replays existing WAL, loads any existing segment files
func Open(dataDir string, flushThreshold int) (*Engine, error) {
	segDir := filepath.Join(dataDir, "segments")
	if err := os.MkdirAll(segDir, 0o755); err != nil {
		return nil, fmt.Errorf("engine: create segments dir: %w", err)
	}

	e := &Engine{
		dataDir:        dataDir,
		segDir:         segDir,
		walPath:        filepath.Join(dataDir, "current.wal"),
		mem:            storage.NewMemtable(),
		log:            storage.NewLogStore(),
		flushThreshold: flushThreshold,
	}

	//recover anything written since the last succesful flush
	err := wal.Replay(e.walPath, func(payload []byte) error {
		kind, point, entry, err := storage.Unwrap(payload)
		if err != nil {
			return err
		}
		switch kind {
		case storage.KindPoint:
			e.mem.Put(point)
		case storage.KindLog:
			e.log.Put(entry)
			e.bumpNextLogID(entry.ID)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("engine: wal replay: %w", err)
	}

	if err := e.loadSegments(); err != nil {
		return nil, fmt.Errorf("enigne: load segments: %w", err)
	}

	w, err := wal.Open(e.walPath)
	if err != nil {
		return nil, fmt.Errorf("engine: open wal: %w", err)
	}
	e.w = w

	/*
		Anything just replayed from the WAL is sitting in the hot buffers now,
	*/
	if e.mem.Len() > 0 || e.log.Len() > 0 {
		if err := e.flushLocked(); err != nil {
			return nil, fmt.Errorf("engine: compact replayed wal data: %w", err)
		}
	}
	return e, nil
}

// bumpNextLogID ensures ID counter resumes above the highest ID seen during recovery
func (e *Engine) bumpNextLogID(seenID int64) {
	for {
		cur := atomic.LoadInt64(&e.nextLogID)
		if seenID < cur {
			return
		}
		if atomic.CompareAndSwapInt64(&e.nextLogID, cur, seenID+1) {
			return
		}
	}
}

func (e *Engine) loadSegments() error {
	entries, err := os.ReadDir(e.segDir)
	if err != nil {
		return err
	}

	var names []string
	for _, ent := range entries {
		if !ent.IsDir() {
			names = append(names, ent.Name())
		}
	}
	sort.Strings(names)

	for _, name := range names {
		seg, err := storage.ReadSegment(filepath.Join(e.segDir, name))
		if err != nil {
			return fmt.Errorf("segment: %s: %w", name, err)
		}
		e.coldPoints = append(e.coldPoints, seg.Points...)
		e.coldLogs = append(e.coldLogs, seg.Logs...)
		for _, l := range seg.Logs {
			e.bumpNextLogID(l.ID)
		}
	}
	return nil
}

// WriteMetric durably logs and buffers one metric point
func (e *Engine) WriteMetric(p storage.Point) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	if p.Timestamp == 0 {
		p.Timestamp = time.Now().UnixNano()
	}
	if err := e.w.Append(storage.WrapPoint(p)); err != nil {
		return err
	}
	e.mem.Put(p)
	return e.maybeFlushLocked()
}

// WriteLog durably logs and buffers one log entry
func (e *Engine) WriteLog(l storage.LogEntry) (storage.LogEntry, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	if l.Timestamp == 0 {
		l.Timestamp = time.Now().UnixNano()
	}
	if len(l.Fields) == 0 {
		l.Fields = []byte("{}")
	}
	l.ID = atomic.AddInt64(&e.nextLogID, 1) - 1

	if err := e.w.Append(storage.WrapLog(l)); err != nil {
		return storage.LogEntry{}, err
	}
	e.log.Put(l)
	if err := e.maybeFlushLocked(); err != nil {
		return storage.LogEntry{}, err
	}
	return l, nil
}

// QueryMetrics returns points for a series across both cold(flushed) and hot data, merged and time-ordered
func (e *Engine) QueryMetrics(series string, start, end int64) []storage.Point {
	e.mu.RLock()
	defer e.mu.RUnlock()

	if end == 0 {
		end = time.Now().Add(24 * time.Hour).UnixNano()
	}

	var out []storage.Point
	for _, p := range e.coldPoints {
		if p.Series == series && p.Timestamp >= start && p.Timestamp <= end {
			out = append(out, p)
		}
	}
	out = append(out, e.mem.Range(series, start, end)...)

	sort.Slice(out, func(i, j int) bool { return out[i].Timestamp < out[j].Timestamp })
	return out
}

// QueryLogs returns log entries matching q across both cold and hot data
// Newest first
func (e *Engine) QueryLogs(q storage.LogQuery, limit, offset int) []storage.LogEntry {
	e.mu.RLock()
	defer e.mu.RUnlock()

	hot := e.log.Query(q, 0)

	var cold []storage.LogEntry
	for i := len(e.coldLogs) - 1; i >= 0; i-- {
		if l := e.coldLogs[i]; q.MatchesEntry(l) {
			cold = append(cold, l)
		}
	}

	merged := append(hot, cold...)
	sort.Slice(merged, func(i, j int) bool { return merged[i].Timestamp > merged[j].Timestamp })

	if offset > 0 {
		if offset >= len(merged) {
			return nil
		}
		merged = merged[offset:]
	}
	if limit > 0 && len(merged) > limit {
		merged = merged[offset:]
	}
	return merged
}

// ListServices returns every distinct log source/service name seen, acorss both cold and hot data
func (e *Engine) ListServices() []string {
	e.mu.RLock()
	defer e.mu.RUnlock()

	seen := make(map[string]struct{})
	for _, l := range e.coldLogs {
		seen[l.Source] = struct{}{}
	}
	for _, l := range e.log.All() {
		seen[l.Source] = struct{}{}
	}

	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Stats is the payload for the dashboard sidebar: counts by level and by services across all data
type Stats struct {
	TotalLogs   int64
	TotalEvents int64
	ByLevel     map[string]int64
	ByService   map[string]int64
}

func (e *Engine) Stats() Stats {
	e.mu.RLock()
	defer e.mu.RUnlock()

	st := Stats{ByLevel: make(map[string]int64), ByService: make(map[string]int64)}

	count := func(l storage.LogEntry) {
		st.TotalLogs++
		st.ByLevel[l.Level]++
		st.ByService[l.Source]++
	}
	for _, l := range e.coldLogs {
		count(l)
	}
	for _, l := range e.log.All() {
		count(l)
	}
	return st
}

// maybeFlushLocked flush to a segment if the hot buffer has grown pass the threshold
func (e *Engine) maybeFlushLocked() error {
	if e.mem.Len()+e.log.Len() < e.flushThreshold {
		return nil
	}
	return e.flushLocked()
}

// Flush forces an immediatee flush regardless of buffer capacity
func (e *Engine) Flush() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.flushLocked()
}

func (e *Engine) flushLocked() error {
	points := e.mem.All()
	logs := e.log.All()
	if len(points) == 0 && len(logs) == 0 {
		return nil
	}

	segPath := filepath.Join(e.segDir, fmt.Sprintf("seg-%20d.seg", time.Now().UnixNano()))
	if err := storage.WriteSegment(segPath, storage.Segment{Points: points, Logs: logs}); err != nil {
		return fmt.Errorf("engine: flush: %w", err)
	}

	e.coldPoints = append(e.coldPoints, points...)
	e.coldLogs = append(e.coldLogs, logs...)
	e.mem.Clear()
	e.log.Clear()

	if err := e.w.Close(); err != nil {
		return fmt.Errorf("engine: close wal before reset: %w", err)
	}
	if err := os.Truncate(e.walPath, 0); err != nil {
		return fmt.Errorf("engine: truncate wal: %w", err)
	}
	w, err := wal.Open(e.walPath)
	if err != nil {
		return fmt.Errorf("engine: reopen wal: %w", err)
	}
	e.w = w

	return nil
}

// ApplyRetention removes cold data older than cutoffNanos and rewrites the remaining cold data
func (e *Engine) ApplyRetention(cutoffNanos int64) (pointsDropped, logsDropped int, err error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	keptPoints := e.coldPoints[:0:0]
	for _, p := range e.coldPoints {
		if p.Timestamp >= cutoffNanos {
			keptPoints = append(keptPoints, p)
		}
	}
	keptLogs := e.coldLogs[:0:0]
	for _, l := range e.coldLogs {
		if l.Timestamp >= cutoffNanos {
			keptLogs = append(keptLogs, l)
		}
	}

	pointsDropped = len(e.coldPoints) - len(keptPoints)
	logsDropped = len(e.coldLogs) - len(keptLogs)
	if pointsDropped == 0 && logsDropped == 0 {
		return 0, 0, nil
	}

	oldSegDir := e.segDir
	entries, err := os.ReadDir(oldSegDir)
	if err != nil {
		return 0, 0, fmt.Errorf("engine: retention: list segments: %w", err)
	}

	if len(keptPoints) > 0 || len(keptLogs) > 0 {
		segPath := filepath.Join(e.segDir, fmt.Sprintf("seg-%20d.seg", time.Now().UnixNano()))
		if err := storage.WriteSegment(segPath, storage.Segment{Points: keptPoints, Logs: keptLogs}); err != nil {
			return 0, 0, fmt.Errorf("engine: retention: write consolidated segment: %w", err)
		}
	}

	for _, ent := range entries {
		if ent.IsDir() {
			continue
		}
		if err := os.Remove(filepath.Join(oldSegDir, ent.Name())); err != nil {
			return 0, 0, fmt.Errorf("engine: retention: remove old segment %s: %w", ent.Name(), err)
		}
	}

	e.coldPoints = keptPoints
	e.coldLogs = keptLogs
	return pointsDropped, logsDropped, nil
}

func (e *Engine) Close() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.w.Close()
}
