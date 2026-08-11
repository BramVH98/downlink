package storage

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"math"
)

/*
Point is one time-series sample: a series name (e.g. "cpu.usage"),
a set of tags (e.g. host=pi4, container=plex), a timestamp, and a value.
*/

type Point struct {
	Series    string
	Tags      map[string]string
	Timestamp int64
	Value     float64
}

func (p Point) Encode() []byte {
	var buf bytes.Buffer

	writeString := func(s string) {
		var lenBuf [2]byte
		binary.BigEndian.PutUint16(lenBuf[:], uint16(len(s)))
		buf.Write(lenBuf[:])
		buf.WriteString(s)
	}

	writeString(p.Series)

	var tagCount [2]byte
	binary.BigEndian.PutUint16(tagCount[:], uint16(len(p.Tags)))
	buf.Write(tagCount[:])
	for k, v := range p.Tags {
		writeString(k)
		writeString(v)
	}

	var tsBuf [8]byte
	binary.BigEndian.PutUint64(tsBuf[:], uint64(p.Timestamp))
	buf.Write(tsBuf[:])

	var valBuf [8]byte
	binary.BigEndian.PutUint64(valBuf[:], math.Float64bits(p.Value))
	buf.Write(valBuf[:])

	return buf.Bytes()
}

// DecodePoint decodes a Point from a byte slice.
func DecodePoint(data []byte) (Point, error) {
	r := bytes.NewReader(data)

	readString := func() (string, error) {
		var lenBuf [2]byte
		if _, err := io.ReadFull(r, lenBuf[:]); err != nil {
			return "", err
		}
		n := binary.BigEndian.Uint16(lenBuf[:])
		strBuf := make([]byte, n)
		if _, err := io.ReadFull(r, strBuf); err != nil {
			return "", err
		}
		return string(strBuf), nil
	}

	series, err := readString()
	if err != nil {
		return Point{}, fmt.Errorf("decode series: %w", err)
	}

	var tagCountBuf [2]byte
	if _, err := io.ReadFull(r, tagCountBuf[:]); err != nil {
		return Point{}, fmt.Errorf("decode tag count: %w", err)
	}
	tagCount := binary.BigEndian.Uint16(tagCountBuf[:])

	tags := make(map[string]string, tagCount)
	for i := 0; i < int(tagCount); i++ {
		k, err := readString()
		if err != nil {
			return Point{}, fmt.Errorf("decode tag key: %w", err)
		}
		v, err := readString()
		if err != nil {
			return Point{}, fmt.Errorf("decode tag value: %w", err)
		}
		tags[k] = v
	}

	var tsBuf [8]byte
	if _, err := io.ReadFull(r, tsBuf[:]); err != nil {
		return Point{}, fmt.Errorf("decode timestamp: %w", err)
	}
	timestamp := int64(binary.BigEndian.Uint64(tsBuf[:]))

	var valBuf [8]byte
	if _, err := io.ReadFull(r, valBuf[:]); err != nil {
		return Point{}, fmt.Errorf("decode value: %w", err)
	}
	value := math.Float64frombits(binary.BigEndian.Uint64(valBuf[:]))

	return Point{
		Series: series, Tags: tags, Timestamp: timestamp, Value: value}, nil
}
