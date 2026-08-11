package storage

import (
	"encoding/json"
	"fmt"
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
	FieldEq    map[string]string
	FieldGT    map[string]float64
	FieldLT    map[string]float64
}

func (q LogQuery) MatchesEntry(e LogEntry) bool {
	if e.Timestamp < q.Start {
		return false
	}
	if q.End != 0 && e.Timestamp > q.End {
		return false
	}
	if q.Level != "" && e.Level != q.Level {
		return false
	}
	if q.Source != "" && e.Source != q.Source {
		return false
	}
	if q.Contains != "" && !strings.Contains(strings.ToLower(e.Message), strings.ToLower(q.Contains)) {
		return false
	}

	if q.hasFieldFilters() {
		var fields map[string]interface{}
		_ = json.Unmarshal(e.Fields, &fields)

		for key, want := range q.FieldEq {
			got, ok := fields[key]
			if !ok || fmt.Sprintf("%v", got) != want {
				return false
			}
		}
		for key, threshold := range q.FieldGT {
			got, ok := fields[key].(float64)
			if !ok || got <= threshold {
				return false
			}
		}
		for key, threshold := range q.FieldLT {
			got, ok := fields[key].(float64)
			if !ok || got >= threshold {
				return false
			}
		}
	}
	return true
}

func (q LogQuery) hasFieldFilters() bool {
	return len(q.FieldEq) > 0 || len(q.FieldGT) > 0 || len(q.FieldLT) > 0
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
		if !q.MatchesEntry(e) {
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
