package storage

import (
	"bytes"
	"encoding/binary"
	"fmt"
)

/*
LogEntry is one log line: an ID(assigned by the engine,
consistently increasing), a timestamp, a source/service name, a level,
the message text, and arbitrary structured fields as
raw JSON({"user":"123","duration_ms":42}) so callers can query on them
*/
type LogEntry struct {
	ID        int64
	Timestamp int64 //Unix nanoseconds
	Source    string
	Level     string
	Message   string
	Fields    []byte //Raw JSON object
}

/*
Encode serializes a LogEntry to bytes for the WAL/on-disk segments
*/
func (l LogEntry) Encode() []byte {
	var buf bytes.Buffer

	WriteString := func(s string) {
		var lenBuf [4]byte
		binary.BigEndian.PutUint32(lenBuf[:], uint32(len(s)))
		buf.Write(lenBuf[:])
		buf.WriteString(s)
	}

	writeBytes := func(b []byte) {
		var lenBuf [4]byte
		binary.BigEndian.PutUint32(lenBuf[:], uint32(len(b)))
		buf.Write(lenBuf[:])
		buf.Write(b)
	}

	var idBuf [8]byte
	binary.BigEndian.PutUint64(idBuf[:], uint64(l.ID))
	buf.Write(idBuf[:])

	var tsBuf [8]byte
	binary.BigEndian.PutUint64(tsBuf[:], uint64(l.Timestamp))
	buf.Write(tsBuf[:])

	WriteString(l.Source)
	WriteString(l.Level)
	WriteString(l.Message)

	fields := l.Fields
	if len(fields) == 0 {
		fields = []byte("{}")
	}
	writeBytes(fields)

	return buf.Bytes()
}

// Reverse encode
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
	readBytes := func() ([]byte, error) {
		var lenBuf [4]byte
		if _, err := r.Read(lenBuf[:]); err != nil {
			return nil, err
		}
		n := binary.BigEndian.Uint32(lenBuf[:])
		b := make([]byte, n)
		if _, err := r.Read(b); err != nil {
			return nil, err
		}
		return b, nil
	}

	var idBuf [8]byte
	if _, err := r.Read(idBuf[:]); err != nil {
		return LogEntry{}, fmt.Errorf("decode id: %w", err)
	}
	id := int64(binary.BigEndian.Uint64(idBuf[:]))

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
	fields, err := readBytes()
	if err != nil {
		return LogEntry{}, fmt.Errorf("decode fields: %w", err)
	}

	return LogEntry{
		ID: id, Timestamp: timestamp, Source: source, Level: level,
		Message: message, Fields: fields,
	}, nil
}
