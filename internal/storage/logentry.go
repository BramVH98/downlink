package storage

import (
	"bytes"
	"encoding/binary"
	"fmt"
)

/*
Logentry is one log line: tiemstamp, source, a level (e.g. INFO, ERROR), and message text
*/

type LogEntry struct {
	Timestamp int64
	Source    string
	Level     string
	Message   string
}

/*
Encode serializes a LogEntry to bytes for the WAL / on-disk segments
Format: [timestamp][sourceLen][source][levelLen][level][messageLen][message]
*/

func (l LogEntry) Encode() []byte {
	var buf bytes.Buffer

	writeString := func(s string) {
		var lenBuf [4]byte
		binary.BigEndian.PutUint32(lenBuf[:], uint32(len(s)))
		buf.Write(lenBuf[:])
		buf.WriteString(s)
	}

	var tsBuf [8]byte
	binary.BigEndian.PutUint64(tsBuf[:], uint64(l.Timestamp))
	buf.Write(tsBuf[:])

	writeString(l.Source)
	writeString(l.Level)
	writeString(l.Message)

	return buf.Bytes()
}

// DecodeLogEntry reverse Encode
func DecodeLogEntry(data []byte) (LogEntry, error) {
	r := bytes.NewReader(data)

	readString := func() (string, error) {
		var lenBuf [4]byte
		if _, err := r.Read(lenBuf[:]); err != nil {
			return "", err
		}
		n := binary.BigEndian.Uint32(lenBuf[:])
		strBuf := make([]byte, n)
		if _, err := r.Read(strBuf); err != nil {
			return "", err
		}
		return string(strBuf), nil
	}

	var tsBuf [8]byte
	if _, err := r.Read(tsBuf[:]); err != nil {
		return LogEntry{}, fmt.Errorf("decode timestamp: %w", err)
	}
	timestamp := int64(binary.BigEndian.Uint64(tsBuf[:]))

	source, err := readString()
	if err != nil {
		return LogEntry{}, fmt.Errorf("decode source: %w", err)
	}

	level, err := readString()
	if err != nil {
		return LogEntry{}, fmt.Errorf("decode level: %w", err)
	}

	message, err := readString()
	if err != nil {
		return LogEntry{}, fmt.Errorf("decode message: %w", err)
	}

	return LogEntry{
		Timestamp: timestamp,
		Source:    source,
		Level:     level,
		Message:   message,
	}, nil
}
