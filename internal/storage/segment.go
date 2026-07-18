/*
Segments are immutable on-disk files written during a flush.
Once memtable/logstore grows past a certain threshold, contents get written out
here and the in-mem buffers are cleared this is supposed to keep RAM usage
bounded instead of growing forever. And what makes the data durable in a
queryable form beyond just "replay everything from the start"
*/

package storage

import (
	"encoding/gob"
	"fmt"
	"os"
)

/*
Segment is one immutable flush snapshot: whatever points and logs were
buffered in mem at flushtime
*/

type Segment struct {
	Points []Point
	Logs   []LogEntry
}

/*
WriteSegment gob-encodes a Segment to path, writing to a temp file firsr
and renaming into placem so a partial write never corrupts an existing segment,
readers only ever see the file after rename
*/
func WriteSegment(path string, seg Segment) error {
	tempPath := path + ".tmp"

	f, err := os.Create(tempPath)
	if err != nil {
		return fmt.Errorf("segment: create %s: %w", tempPath, err)
	}

	if err := gob.NewEncoder(f).Encode(seg); err != nil {
		f.Close()
		os.Remove(tempPath)
		return fmt.Errorf("segment: fsync: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tempPath)
		return fmt.Errorf("segment: close: %w", err)
	}

	if err := os.Rename(tempPath, path); err != nil {
		return fmt.Errorf("segment: rename into place %w", err)
	}
	return nil
}

// ReadSegment loads a  segment file back into mem
func ReadSegment(path string) (Segment, error) {
	f, err := os.Open(path)
	if err != nil {
		return Segment{}, fmt.Errorf("segment: open %s: %w", path, err)
	}
	defer f.Close()

	var seg Segment
	if err := gob.NewDecoder(f).Decode(&seg); err != nil {
		return Segment{}, fmt.Errorf("segment: decode %s: %w", path, err)
	}
	return seg, nil
}
