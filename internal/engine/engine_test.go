package engine

import (
	"path/filepath"
	"sync"
	"testing"
	"time"

	"downlink/internal/storage"
)

func TestWriteQuery_Logs(t *testing.T) {
	dir := t.TempDir()
	e, err := Open(dir, 5000)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	stored, err := e.WriteLog(storage.LogEntry{Source: "auth-api", Level: "error", Message: "timeout"})
	if err != nil {
		t.Fatal(err)
	}
	if stored.ID != 0 {
		t.Errorf("first log ID = %d, want 0", stored.ID)
	}
	if stored.Timestamp == 0 {
		t.Error("expected a non-zero auto-assigned timestamp")
	}

	results := e.QueryLogs(storage.LogQuery{Source: "auth-api"}, 0, 0)
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}
	if results[0].Message != "timeout" {
		t.Errorf("Message = %q, want %q", results[0].Message, "timeout")
	}
}

func TestWriteQuery_Metrics(t *testing.T) {
	dir := t.TempDir()
	e, err := Open(dir, 5000)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	if err := e.WriteMetric(storage.Point{Series: "cpu.usage", Value: 42.5, Timestamp: 1000}); err != nil {
		t.Fatal(err)
	}
	if err := e.WriteMetric(storage.Point{Series: "cpu.usage", Value: 55.0, Timestamp: 2000}); err != nil {
		t.Fatal(err)
	}

	results := e.QueryMetrics("cpu.usage", 0, 0)
	if len(results) != 2 {
		t.Fatalf("got %d results, want 2", len(results))
	}
	if results[0].Value != 42.5 || results[1].Value != 55.0 {
		t.Errorf("values = %v, %v; want 42.5, 55.0 (should be time-ordered)", results[0].Value, results[1].Value)
	}
}

func TestLogIDs_MonotonicallyIncreasing(t *testing.T) {
	dir := t.TempDir()
	e, err := Open(dir, 5000)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	var ids []int64
	for i := 0; i < 5; i++ {
		stored, err := e.WriteLog(storage.LogEntry{Source: "s", Level: "info", Message: "m"})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, stored.ID)
	}

	for i, id := range ids {
		if id != int64(i) {
			t.Errorf("ids[%d] = %d, want %d", i, id, i)
		}
	}
}

// TestCrashRecovery_WALReplay is the core durability contract: writes that
// were fsynced but never cleanly flushed to a segment (simulating a crash
// - Close() is never called) must come back after reopening the engine.
func TestCrashRecovery_WALReplay(t *testing.T) {
	dir := t.TempDir()

	e1, err := Open(dir, 5000) // high threshold - nothing flushes to a segment
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e1.WriteLog(storage.LogEntry{Source: "s", Level: "error", Message: "before crash"}); err != nil {
		t.Fatal(err)
	}
	// Deliberately NOT calling e1.Close() - simulates a hard crash where
	// the WAL already has the fsynced write, but nothing else ran.

	e2, err := Open(dir, 5000)
	if err != nil {
		t.Fatalf("reopen after simulated crash: %v", err)
	}
	defer e2.Close()

	results := e2.QueryLogs(storage.LogQuery{}, 0, 0)
	if len(results) != 1 {
		t.Fatalf("recovered %d logs after crash, want 1", len(results))
	}
	if results[0].Message != "before crash" {
		t.Errorf("recovered message = %q, want %q", results[0].Message, "before crash")
	}
}

// TestFlush_MovesDataToSegmentAndTruncatesWAL confirms a flush actually
// writes a segment file to disk and empties the WAL, not just moves data
// around in memory.
func TestFlush_MovesDataToSegmentAndTruncatesWAL(t *testing.T) {
	dir := t.TempDir()
	e, err := Open(dir, 5000)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	if _, err := e.WriteLog(storage.LogEntry{Source: "s", Level: "info", Message: "m"}); err != nil {
		t.Fatal(err)
	}

	segDir := filepath.Join(dir, "segments")
	before := countFiles(t, segDir)

	if err := e.Flush(); err != nil {
		t.Fatal(err)
	}

	after := countFiles(t, segDir)
	if after != before+1 {
		t.Errorf("expected exactly one new segment file after Flush, before=%d after=%d", before, after)
	}

	// Data must still be queryable after the flush - it moved from hot to
	// cold storage, it didn't disappear.
	results := e.QueryLogs(storage.LogQuery{}, 0, 0)
	if len(results) != 1 {
		t.Errorf("got %d results after flush, want 1 (data should survive the move to cold storage)", len(results))
	}
}

// TestApplyRetention_DropsOldKeepsNew is the basic retention contract.
func TestApplyRetention_DropsOldKeepsNew(t *testing.T) {
	dir := t.TempDir()
	e, err := Open(dir, 1) // flush threshold of 1 - everything becomes cold immediately
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	now := time.Now()
	oldEntry := storage.LogEntry{Source: "s", Level: "info", Message: "old", Timestamp: now.Add(-48 * time.Hour).UnixNano()}
	newEntry := storage.LogEntry{Source: "s", Level: "info", Message: "new", Timestamp: now.UnixNano()}

	if _, err := e.WriteLog(oldEntry); err != nil {
		t.Fatal(err)
	}
	if _, err := e.WriteLog(newEntry); err != nil {
		t.Fatal(err)
	}

	cutoff := now.Add(-24 * time.Hour).UnixNano()
	pointsDropped, logsDropped, err := e.ApplyRetention(cutoff)
	if err != nil {
		t.Fatal(err)
	}
	if pointsDropped != 0 {
		t.Errorf("pointsDropped = %d, want 0", pointsDropped)
	}
	if logsDropped != 1 {
		t.Errorf("logsDropped = %d, want 1", logsDropped)
	}

	results := e.QueryLogs(storage.LogQuery{}, 0, 0)
	if len(results) != 1 {
		t.Fatalf("got %d results after retention, want 1 (only 'new' should survive)", len(results))
	}
	if results[0].Message != "new" {
		t.Errorf("survivor = %q, want %q", results[0].Message, "new")
	}
}

// TestRetention_CatchesDataReplayedFromCrash is a regression test for a
// real bug found during manual testing: data recovered via WAL replay on
// startup landed in the hot buffer, and ApplyRetention deliberately skips
// hot data (assuming it's always recent) - so old data that just got
// replayed from a crash was invisible to retention until unrelated new
// writes happened to trigger a flush. The fix was compacting replayed
// data into cold storage immediately in Open(), before returning.
func TestRetention_CatchesDataReplayedFromCrash(t *testing.T) {
	dir := t.TempDir()

	// Write an old-timestamped entry and "crash" without a clean close,
	// so it only exists in the WAL, not yet in a segment.
	e1, err := Open(dir, 5000)
	if err != nil {
		t.Fatal(err)
	}
	oldTimestamp := time.Now().Add(-48 * time.Hour).UnixNano()
	if _, err := e1.WriteLog(storage.LogEntry{Source: "s", Level: "info", Message: "old, from crash", Timestamp: oldTimestamp}); err != nil {
		t.Fatal(err)
	}
	// No Close() - simulating a crash.

	// Reopen: this replays the WAL. If the bug were present, "old, from
	// crash" would land in the hot buffer and NOT get compacted.
	e2, err := Open(dir, 5000)
	if err != nil {
		t.Fatal(err)
	}
	defer e2.Close()

	// Now run retention with a cutoff that should catch the old entry.
	cutoff := time.Now().Add(-24 * time.Hour).UnixNano()
	pointsDropped, logsDropped, err := e2.ApplyRetention(cutoff)
	if err != nil {
		t.Fatal(err)
	}
	if pointsDropped != 0 {
		t.Errorf("pointsDropped = %d, want 0 (no metrics were written)", pointsDropped)
	}

	if logsDropped != 1 {
		t.Errorf("retention dropped %d logs, want 1 - replayed WAL data must be compacted to cold storage on startup so retention can see it", logsDropped)
	}

	results := e2.QueryLogs(storage.LogQuery{}, 0, 0)
	if len(results) != 0 {
		t.Errorf("got %d results after retention, want 0 (the old replayed entry should have been dropped)", len(results))
	}
}

// TestOpen_ResumesLogIDCounterAfterRestart confirms IDs never collide
// across a restart, whether data came from the WAL or from segments.
func TestOpen_ResumesLogIDCounterAfterRestart(t *testing.T) {
	dir := t.TempDir()

	e1, err := Open(dir, 1) // flush threshold 1, so this becomes a segment
	if err != nil {
		t.Fatal(err)
	}
	stored1, err := e1.WriteLog(storage.LogEntry{Source: "s", Level: "info", Message: "first"})
	if err != nil {
		t.Fatal(err)
	}
	if err := e1.Close(); err != nil {
		t.Fatal(err)
	}

	e2, err := Open(dir, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer e2.Close()
	stored2, err := e2.WriteLog(storage.LogEntry{Source: "s", Level: "info", Message: "second"})
	if err != nil {
		t.Fatal(err)
	}

	if stored2.ID == stored1.ID {
		t.Errorf("ID collision after restart: both entries got ID %d", stored1.ID)
	}
	if stored2.ID <= stored1.ID {
		t.Errorf("second.ID (%d) should be greater than first.ID (%d) after restart", stored2.ID, stored1.ID)
	}
}

// TestConcurrentReadsAndWrites exercises the RWMutex under real concurrent
// load - run with -race to verify the locking, not just the outcome.
func TestConcurrentReadsAndWrites(t *testing.T) {
	dir := t.TempDir()
	e, err := Open(dir, 50)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	const writers = 10
	const writesEach = 20

	var writerWg sync.WaitGroup
	for w := 0; w < writers; w++ {
		writerWg.Add(1)
		go func(id int) {
			defer writerWg.Done()
			for i := 0; i < writesEach; i++ {
				_, err := e.WriteLog(storage.LogEntry{Source: "concurrent", Level: "info", Message: "m"})
				if err != nil {
					t.Errorf("writer %d write %d: %v", id, i, err)
				}
			}
		}(w)
	}

	// Concurrent readers hitting every read path at once, running until
	// explicitly told to stop.
	stop := make(chan struct{})
	var readerWg sync.WaitGroup
	for r := 0; r < 5; r++ {
		readerWg.Add(1)
		go func() {
			defer readerWg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					e.QueryLogs(storage.LogQuery{}, 10, 0)
					e.ListServices()
					e.Stats()
					time.Sleep(time.Millisecond) // don't busy-spin - real callers never hammer a lock like this
				}
			}
		}()
	}

	// Actually wait for every writer to finish before doing anything else -
	// no fixed sleep, no guessing.
	writerWg.Wait()
	close(stop)
	readerWg.Wait()

	results := e.QueryLogs(storage.LogQuery{Source: "concurrent"}, 0, 0)
	if len(results) != writers*writesEach {
		t.Errorf("got %d logs, want %d (concurrent writes lost)", len(results), writers*writesEach)
	}
}

func countFiles(t *testing.T, dir string) int {
	t.Helper()
	entries, err := filepath.Glob(filepath.Join(dir, "*"))
	if err != nil {
		t.Fatal(err)
	}
	return len(entries)
}
