package storage

import (
	"bufio"
	"compress/gzip"
	"encoding/gob"
	"fmt"
	"io"
	"os"
)

type Segment struct {
	Points []Point
	Logs   []LogEntry
}

var gzipMagic = [2]byte{0x1f, 0x8b}

func WriteSegment(path string, seg Segment) error {
	tmpPath := path + ".tmp"

	f, err := os.Create(tmpPath)
	if err != nil {
		return fmt.Errorf("segment: create %s: %w", tmpPath, err)
	}

	gz := gzip.NewWriter(f)
	if err := gob.NewEncoder(gz).Encode(seg); err != nil {
		gz.Close()
		f.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("segment: encode: %w", err)
	}

	if err := gz.Close(); err != nil {
		f.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("segment: close gzip writer: %w", err)
	}

	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("segment: fsync: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("segment: close: %w", err)
	}

	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("segment: rename into place: %w", err)
	}
	return nil
}

func ReadSegment(path string) (Segment, error) {
	f, err := os.Open(path)
	if err != nil {
		return Segment{}, fmt.Errorf("segment: open %s: %w", path, err)
	}
	defer f.Close()

	br := bufio.NewReader(f)
	magic, err := br.Peek(2)
	if err != nil && err != io.EOF {
		return Segment{}, fmt.Errorf("segment: read header of %s: %w", path, err)
	}

	var r io.Reader = br
	if len(magic) == 2 && magic[0] == gzipMagic[0] && magic[1] == gzipMagic[1] {
		gz, err := gzip.NewReader(br)
		if err != nil {
			return Segment{}, fmt.Errorf("segment: open gzip reader for %s: %w", path, err)
		}
		defer gz.Close()
		r = gz
	}
	var seg Segment
	if err := gob.NewDecoder(r).Decode(&seg); err != nil {
		return Segment{}, fmt.Errorf("segment: decode %s: %w", path, err)
	}
	return seg, nil
}
