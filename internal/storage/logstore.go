package storage

import (
	"sort"
	"strings"
	"sync"
)

/*
LogStore is the in-memory write buffer for log entries, kept sorted by timestamp.
Mirrors the Memtable for time series data, but for log entries instead of points.
*/
type LogStore struct {
	mu      sync.RWMutex
	entries []LogEntry // sorted by timestamp
}

func NewLogStore() *LogStore {
	return &LogStore{}
}

// Put inserts a log entry in timestamp order
func (s *LogStore) Put(e LogEntry) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.entries) == 0 || s.entries[len(s.entries)-1].Timestamp <= e.Timestamp {
		s.entries = append(s.entries, e)
		return
	}

	idx := sort.Search(len(s.entries), func(i int) bool {
		return s.entries[i].Timestamp >= e.Timestamp
	})
	s.entries = append(s.entries, LogEntry{}) // make room
	copy(s.entries[idx+1:], s.entries[idx:])
	s.entries[idx] = e
}

/*
LogQuery filters which log entries Query returns. Zero-value fields mean
"Don't filter on this field"
*/

type LogQuery struct {
	Source     string //exact match, empty = any
	Level      string //exact match, empty = any
	Contains   string // case-insensitive substring match on Message
	Start, End int64  //timestamp range; End=0 means "no upper bound"
}

/*
query returns entries matching Q, newest first, capped at limit
(limit<=0 means no cap)
*/

func (s *LogStore) Query(q LogQuery, limit int) []LogEntry {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var out []LogEntry
	for i := len(s.entries) - 1; i >= 0; i-- {
		e := s.entries[i]

		if e.Timestamp < q.Start {
			continue
		}
		if q.End != 0 && e.Timestamp > q.End {
			continue
		}
		if q.Source != "" && e.Source != q.Source {
			continue
		}
		if q.Level != "" && e.Level != q.Level {
			continue
		}
		if q.Contains != "" && !strings.Contains(strings.ToLower(e.Message), strings.ToLower(q.Contains)) {
			continue
		}

		out = append(out, e)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out
}

// Len returns the number of buffered log entries
func (s *LogStore) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.entries)
}

// All returns a copy of every buffered entry, eldest first Used for flushing
func (s *LogStore) All() []LogEntry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]LogEntry, len(s.entries))
	copy(out, s.entries)
	return out
}

// Clear empties the store. Used after flushing to disk.
func (s *LogStore) Clear() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries = nil
}
