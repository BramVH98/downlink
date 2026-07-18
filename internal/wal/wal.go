/*
Package wal implements a write-ahead log: every write is appended to disk
(and fsynched) before it's considered durable. If the process crashes, Replay()
rebuilds in-memory state by reading every entry back in order

On-disk framing per entry:

[4 bytes length][4 bytes CR32 of payload][payload bytes]
*/

package wal

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"sync"
)

type WAL struct {
	mu   sync.Mutex
	file *os.File
	w    *bufio.Writer
}

/*
Open opens (or creates) a WAL file at path, ready for appends.
Existing content is preserved and new writes are appended after it.
*/

func Open(path string) (*WAL, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("wal: open %s: %w", path, err)
	}
	return &WAL{
		file: f,
		w:    bufio.NewWriter(f),
	}, nil
}

/*
Append writes one record to the log and fsyncs before returning, so a succesful
return means the data survived a crash/power loss
*/

func (l *WAL) Append(payload []byte) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	var lenBuf [4]byte
	binary.BigEndian.PutUint32(lenBuf[:], uint32(len(payload)))

	var crcBuf [4]byte
	binary.BigEndian.PutUint32(crcBuf[:], crc32.ChecksumIEEE(payload))

	if _, err := l.w.Write(lenBuf[:]); err != nil {
		return fmt.Errorf("wal: write length: %w", err)
	}

	if _, err := l.w.Write(crcBuf[:]); err != nil {
		return fmt.Errorf("wal: write crc: %w", err)
	}

	if _, err := l.w.Write(payload); err != nil {
		return fmt.Errorf("wal: write payload: %w", err)
	}

	if err := l.w.Flush(); err != nil {
		return fmt.Errorf("wal: flush: %w", err)
	}
	/*
	   fynsc is what makess this durable against a power loss,
	   not just a process crash
	*/

	if err := l.file.Sync(); err != nil {
		return fmt.Errorf("wal: fsync: %w", err)
	}
	return nil
}

/*
Replay reads every entry in the log, in order, and calls fn with each payload.
A corrupted trailing entry (e.g from a crash mid-write) is treated as the end of a log rather than a fatal error,
since a torn write at the tail is expected after a crash.
*/

func Replay(path string, fn func(payload []byte) error) error {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // nothing to return
		}
		return fmt.Errorf("wal: open for replay: %w", err)
	}
	defer f.Close()

	r := bufio.NewReader(f)

	for {
		var lenBuf, crcBuf [4]byte

		if _, err := io.ReadFull(r, lenBuf[:]); err != nil {
			if err == io.EOF {
				return nil
			}
			return nil // torn header at EOF after crash, stop, don't fail
		}
		if _, err := io.ReadFull(r, crcBuf[:]); err != nil {
			return nil // torn write, treat as end of log
		}

		length := binary.BigEndian.Uint32(lenBuf[:])
		payload := make([]byte, length)
		if _, err := io.ReadFull(r, payload); err != nil {
			return nil // torn write, treat as end of log
		}

		wantCRC := binary.BigEndian.Uint32(crcBuf[:])
		gotCRC := crc32.ChecksumIEEE(payload)
		if wantCRC != gotCRC {
			return nil // corrupted trailing entry, stop replay here
		}

		if err := fn(payload); err != nil {
			return fmt.Errorf("wal: replay callback: %w", err)
		}
	}

}

// Close flushes and closes the underlying file. After Close, the WAL is no longer usable.

func (l *WAL) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.w.Flush(); err != nil {
		return err
	}
	return l.file.Close()
}
