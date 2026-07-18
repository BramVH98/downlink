package storage

import "fmt"

/*
Kind tags what type of record a WAL/segment payload holds, since a single
WAL stream carries both metric points and log entries.
*/
type Kind byte

const (
	KindPoint Kind = 1
	KindLog   Kind = 2
)

// WrapPoint prefixes an encoded Point with its Kind byte for writing to the WAL/segment
func WrapPoint(p Point) []byte {
	return append([]byte{byte(KindPoint)}, p.Encode()...)
}

// WrapLog prefixes an encoded Point with its Kind byte, for writing to the WAL
func WrapLog(l LogEntry) []byte {
	return append([]byte{byte(KindLog)}, l.Encode()...)
}

/*
Unwrap inspects the leading Kind byte and decodes the rest accordingly
Returns exactly one of(Point, LogEntry) populated, matching the Kind
*/
func Unwrap(payload []byte) (kind Kind, point Point, entry LogEntry, err error) {
	if len(payload) == 0 {
		return 0, Point{}, LogEntry{}, fmt.Errorf("storage: empty payload")
	}

	kind = Kind(payload[0])
	body := payload[1:]

	switch kind {
	case KindPoint:
		point, err = DecodePoint(body)
	case KindLog:
		entry, err = DecodeLogEntry(body)
	default:
		err = fmt.Errorf("storage: unknown kind byte %d", payload[0])
	}
	return kind, point, entry, err
}
