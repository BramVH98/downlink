package wal

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestAppendReplay_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.wal")

	w, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	records := [][]byte{
		[]byte("first record"),
		[]byte("second record"),
		[]byte(""), // empty payload should round-trip too
		[]byte("record with\x00null bytes\nand newlines"),
	}
	for _, rec := range records {
		if err := w.Append(rec); err != nil {
			t.Fatalf("Append(%q): %v", rec, err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	var recovered [][]byte
	err = Replay(path, func(payload []byte) error {
		cp := make([]byte, len(payload))
		copy(cp, payload)
		recovered = append(recovered, cp)
		return nil
	})
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}

	if len(recovered) != len(records) {
		t.Fatalf("recovered %d records, want %d", len(recovered), len(records))
	}
	for i := range records {
		if string(recovered[i]) != string(records[i]) {
			t.Errorf("record %d = %q, want %q", i, recovered[i], records[i])
		}
	}
}

func TestReplay_NonexistentFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "does-not-exist.wal")

	called := false
	err := Replay(path, func(payload []byte) error {
		called = true
		return nil
	})
	if err != nil {
		t.Fatalf("expected no error for a nonexistent WAL file, got: %v", err)
	}
	if called {
		t.Error("callback should never be called for a nonexistent file")
	}
}

func TestReplay_EmptyFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "empty.wal")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	err := Replay(path, func(payload []byte) error {
		t.Error("callback should not be called for an empty file")
		return nil
	})
	if err != nil {
		t.Fatalf("expected no error for an empty file, got: %v", err)
	}
}

// TestReplay_TruncatedTail is the actual crash-recovery contract: if the
// process dies mid-write, the WAL file ends with a partial (torn) record.
// Replay must recover every complete record before the tear and stop
// cleanly there - not error out, and not lose the earlier good records.
func TestReplay_TruncatedTail(t *testing.T) {
	dir := t.TempDir()
	setupPath := filepath.Join(dir, "setup.wal")

	w, err := Open(setupPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Append([]byte("complete record one")); err != nil {
		t.Fatal(err)
	}
	if err := w.Append([]byte("complete record two")); err != nil {
		t.Fatal(err)
	}

	fullSizeTwoRecords, err := fileSize(setupPath)
	if err != nil {
		t.Fatal(err)
	}

	if err := w.Append([]byte("this record will be torn off")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	full, err := os.ReadFile(setupPath)
	if err != nil {
		t.Fatal(err)
	}

	// For each possible cut point partway through the third record, write
	// a FRESH file containing exactly that many bytes.
	for cut := fullSizeTwoRecords; cut < int64(len(full)); cut++ {
		path := filepath.Join(dir, fmt.Sprintf("cut-%d.wal", cut))
		if err := os.WriteFile(path, full[:cut], 0o644); err != nil {
			t.Fatal(err)
		}

		var recovered []string
		err := Replay(path, func(payload []byte) error {
			recovered = append(recovered, string(payload))
			return nil
		})
		if err != nil {
			t.Errorf("cut=%d: Replay returned an error instead of stopping cleanly: %v", cut, err)
		}
		if len(recovered) != 2 {
			t.Errorf("cut=%d: recovered %d records, want exactly 2 (the complete ones before the tear)", cut, len(recovered))
			continue
		}
		if recovered[0] != "complete record one" || recovered[1] != "complete record two" {
			t.Errorf("cut=%d: recovered %v, want the two original complete records", cut, recovered)
		}
	}
}

// TestReplay_CorruptedCRC confirms a bit-flipped (corrupted, not just
// truncated) record is detected via checksum mismatch and replay stops
// there, rather than returning corrupted data as if it were valid.
func TestReplay_CorruptedCRC(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.wal")

	w, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Append([]byte("good record")); err != nil {
		t.Fatal(err)
	}
	if err := w.Append([]byte("this one gets corrupted")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	corruptAt := len(data) - 5
	data[corruptAt] ^= 0xFF
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}

	var recovered []string
	err = Replay(path, func(payload []byte) error {
		recovered = append(recovered, string(payload))
		return nil
	})
	if err != nil {
		t.Fatalf("Replay should stop cleanly on CRC mismatch, not error: %v", err)
	}
	if len(recovered) != 1 || recovered[0] != "good record" {
		t.Errorf("recovered %v, want exactly [\"good record\"] (corrupted record must be dropped, not returned)", recovered)
	}
}

func TestAppend_SurvivesReopen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.wal")

	w1, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := w1.Append([]byte("before reopen")); err != nil {
		t.Fatal(err)
	}
	if err := w1.Close(); err != nil {
		t.Fatal(err)
	}

	w2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := w2.Append([]byte("after reopen")); err != nil {
		t.Fatal(err)
	}
	if err := w2.Close(); err != nil {
		t.Fatal(err)
	}

	var recovered []string
	Replay(path, func(payload []byte) error {
		recovered = append(recovered, string(payload))
		return nil
	})

	if len(recovered) != 2 {
		t.Fatalf("recovered %d records, want 2 (reopen must append, not overwrite)", len(recovered))
	}
	if recovered[0] != "before reopen" || recovered[1] != "after reopen" {
		t.Errorf("recovered %v, want [\"before reopen\", \"after reopen\"]", recovered)
	}
}

// TestAppend_ConcurrentWrites confirms multiple goroutines appending at
// once never corrupt each other's records - run with -race to actually
// verify the locking, not just the outcome.
func TestAppend_ConcurrentWrites(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.wal")

	w, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}

	const goroutines = 20
	const writesEach = 25

	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := 0; i < writesEach; i++ {
				payload := []byte{byte(id), byte(i)}
				if err := w.Append(payload); err != nil {
					t.Errorf("goroutine %d write %d: %v", id, i, err)
				}
			}
		}(g)
	}
	wg.Wait()

	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	count := 0
	err = Replay(path, func(payload []byte) error {
		if len(payload) != 2 {
			t.Errorf("corrupted record found: %v", payload)
		}
		count++
		return nil
	})
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if count != goroutines*writesEach {
		t.Errorf("recovered %d records, want %d (concurrent writes lost or corrupted data)", count, goroutines*writesEach)
	}
}

func fileSize(path string) (int64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}
