package storage

import (
	"bytes"
	"compress/gzip"
	"encoding/gob"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func sampleSegment(n int) Segment {
	seg := Segment{}
	for i := 0; i < n; i++ {
		seg.Points = append(seg.Points, Point{
			Series: "cpu.usage", Tags: map[string]string{"host": "pi4"},
			Timestamp: int64(i), Value: 42.5,
		})
		seg.Logs = append(seg.Logs, LogEntry{
			ID: int64(i), Timestamp: int64(i), Source: "auth-api", Level: "info",
			Message: "user login successful, session established normally",
			Fields:  []byte(`{"user":"123","duration_ms":42}`),
		})
	}
	return seg
}

func TestWriteReadSegment_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.seg")

	original := sampleSegment(50)
	if err := WriteSegment(path, original); err != nil {
		t.Fatalf("WriteSegment: %v", err)
	}

	got, err := ReadSegment(path)
	if err != nil {
		t.Fatalf("ReadSegment: %v", err)
	}

	if len(got.Points) != len(original.Points) {
		t.Fatalf("got %d points, want %d", len(got.Points), len(original.Points))
	}
	if len(got.Logs) != len(original.Logs) {
		t.Fatalf("got %d logs, want %d", len(got.Logs), len(original.Logs))
	}
	if got.Logs[10].Message != original.Logs[10].Message {
		t.Errorf("log message mismatch: got %q, want %q", got.Logs[10].Message, original.Logs[10].Message)
	}
	if got.Points[10].Series != original.Points[10].Series {
		t.Errorf("point series mismatch: got %q, want %q", got.Points[10].Series, original.Points[10].Series)
	}
}

func TestWriteSegment_ActuallyCompresses(t *testing.T) {
	dir := t.TempDir()
	compressedPath := filepath.Join(dir, "compressed.seg")
	rawPath := filepath.Join(dir, "raw.gob")

	seg := sampleSegment(500)

	if err := WriteSegment(compressedPath, seg); err != nil {
		t.Fatalf("WriteSegment: %v", err)
	}
	compressedInfo, err := os.Stat(compressedPath)
	if err != nil {
		t.Fatal(err)
	}

	rawFile, err := os.Create(rawPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := gob.NewEncoder(rawFile).Encode(seg); err != nil {
		t.Fatal(err)
	}
	rawFile.Close()
	rawInfo, err := os.Stat(rawPath)
	if err != nil {
		t.Fatal(err)
	}

	t.Logf("uncompressed: %d bytes, compressed: %d bytes (%.1f%% of original)",
		rawInfo.Size(), compressedInfo.Size(), float64(compressedInfo.Size())/float64(rawInfo.Size())*100)

	if compressedInfo.Size() >= rawInfo.Size() {
		t.Errorf("compressed segment (%d bytes) is not smaller than uncompressed (%d bytes)",
			compressedInfo.Size(), rawInfo.Size())
	}
}

func TestReadSegment_BackwardCompatibleWithUncompressedFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "old-format.seg")

	original := sampleSegment(20)

	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := gob.NewEncoder(f).Encode(original); err != nil {
		t.Fatal(err)
	}
	f.Close()

	got, err := ReadSegment(path)
	if err != nil {
		t.Fatalf("ReadSegment should transparently handle an old uncompressed segment, got error: %v", err)
	}

	if len(got.Points) != len(original.Points) || len(got.Logs) != len(original.Logs) {
		t.Errorf("got %d points/%d logs, want %d/%d", len(got.Points), len(got.Logs), len(original.Points), len(original.Logs))
	}
}

func TestReadSegment_EmptySegment(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "empty.seg")

	if err := WriteSegment(path, Segment{}); err != nil {
		t.Fatal(err)
	}

	got, err := ReadSegment(path)
	if err != nil {
		t.Fatalf("ReadSegment on an empty segment: %v", err)
	}
	if len(got.Points) != 0 || len(got.Logs) != 0 {
		t.Errorf("expected an empty segment, got %d points, %d logs", len(got.Points), len(got.Logs))
	}
}

func TestReadSegment_CorruptedFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "corrupt.seg")

	if err := WriteSegment(path, sampleSegment(5)); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	mid := len(data) / 2
	data[mid] ^= 0xFF
	data[mid+1] ^= 0xFF
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}

	_, err = ReadSegment(path)
	if err == nil {
		t.Error("expected an error reading a corrupted segment, got nil")
	}
}

func TestReadSegment_NonexistentFile(t *testing.T) {
	_, err := ReadSegment("/nonexistent/path/segment.seg")
	if err == nil {
		t.Error("expected an error for a nonexistent file, got nil")
	}
}

func TestWriteSegment_AtomicRename_NoPartialFileOnFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.seg")

	if err := WriteSegment(path, sampleSegment(5)); err != nil {
		t.Fatal(err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("leftover temp file after a successful write: %s", e.Name())
		}
	}
}

func TestGzipMagicDetection(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	gz.Write([]byte("test"))
	gz.Close()

	data := buf.Bytes()
	if len(data) < 2 || data[0] != gzipMagic[0] || data[1] != gzipMagic[1] {
		t.Errorf("gzip output doesn't start with expected magic bytes: got %v", data[:2])
	}
}
