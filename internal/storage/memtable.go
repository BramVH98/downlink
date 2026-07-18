package storage

import (
	"sort"
	"sync"
)

/*
Memtable is the in-memory write buffer.
Every write lands here first (after being logged to the WAL)
Once it grows past a size threshold, it is flushed to an immutable on-disk segment
and a fresh memtable is created for new writes. Flush step is Phase 2 not implemented yet.
*/

type Memtable struct {
	mu     sync.RWMutex
	points map[string][]Point // keyed by series name]
}

func NewMemtable() *Memtable {
	return &Memtable{
		points: make(map[string][]Point),
	}
}

func (m *Memtable) Put(p Point) {
	m.mu.Lock()
	defer m.mu.Unlock()

	series := m.points[p.Series]
	//Fast path: points usually arrive in roughly increasing timestamp order
	if len(series) == 0 || series[len(series)-1].Timestamp <= p.Timestamp {
		m.points[p.Series] = append(series, p)
		return
	}

	//Slow path: out-of-order point, insert in sorted order
	idx := sort.Search(len(series), func(i int) bool {
		return series[i].Timestamp >= p.Timestamp
	})
	series = append(series, Point{}) // make room
	copy(series[idx+1:], series[idx:])
	series[idx] = p
	m.points[p.Series] = series
}

// Range returns all points for a series in the given time range [start, end)
func (m *Memtable) Range(series string, start, end int64) []Point {
	m.mu.RLock()
	defer m.mu.RUnlock()

	all := m.points[series]
	lo := sort.Search(len(all), func(i int) bool { return all[i].Timestamp >= start })
	hi := sort.Search(len(all), func(i int) bool { return all[i].Timestamp >= end })

	out := make([]Point, hi-lo)
	copy(out, all[lo:hi])
	return out
}

/*
Len returns the total number of points currently buffered, across all series.
Used to decide when to flush the memtable to disk.
*/

func (m *Memtable) Len() int {
	m.mu.RLock()
	defer m.mu.RUnlock()

	n := 0
	for _, series := range m.points {
		n += len(series)
	}
	return n
}

// SeriesNames returns the distinct series names currently buffered in the memtable.
func (m *Memtable) SeriesNames() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	names := make([]string, 0, len(m.points))
	for name := range m.points {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
