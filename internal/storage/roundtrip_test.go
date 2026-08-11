package storage

import (
	"reflect"
	"testing"
)

func TestLogEntry_EncodeDecodeRoundTrip(t *testing.T) {
	tests := []struct {
		name  string
		entry LogEntry
	}{
		{
			name: "typical entry with fields",
			entry: LogEntry{
				ID: 42, Timestamp: 1234567890, Source: "auth-api",
				Level: "error", Message: "Database timeout",
				Fields: []byte(`{"user":"123","duration_ms":1500}`),
			},
		},
		{
			name:  "empty message and no fields",
			entry: LogEntry{ID: 0, Timestamp: 0, Source: "x", Level: "info", Message: "", Fields: nil},
		},
		{
			name: "unicode in message",
			entry: LogEntry{
				ID: 1, Timestamp: 100, Source: "app", Level: "info",
				Message: "user logged in: 日本語 emoji 🎉",
			},
		},
		{
			name: "negative timestamp",
			entry: LogEntry{
				ID: 5, Timestamp: -1000, Source: "old-service", Level: "warn",
				Message: "before unix epoch",
			},
		},
		{
			name: "large ID",
			entry: LogEntry{
				ID: 9223372036854775807, Timestamp: 1, Source: "s", Level: "info", Message: "m",
			},
		},
		{
			name: "message containing binary-looking bytes",
			entry: LogEntry{
				ID: 1, Timestamp: 1, Source: "s", Level: "info",
				Message: "line1\x00line2\nwith\ttabs",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			encoded := tt.entry.Encode()
			decoded, err := DecodeLogEntry(encoded)
			if err != nil {
				t.Fatalf("decode failed: %v", err)
			}

			// Fields gets normalized to "{}" when empty by Encode, so
			// account for that rather than comparing raw nil vs []byte("{}").
			want := tt.entry
			if len(want.Fields) == 0 {
				want.Fields = []byte("{}")
			}

			if decoded.ID != want.ID {
				t.Errorf("ID = %d, want %d", decoded.ID, want.ID)
			}
			if decoded.Timestamp != want.Timestamp {
				t.Errorf("Timestamp = %d, want %d", decoded.Timestamp, want.Timestamp)
			}
			if decoded.Source != want.Source {
				t.Errorf("Source = %q, want %q", decoded.Source, want.Source)
			}
			if decoded.Level != want.Level {
				t.Errorf("Level = %q, want %q", decoded.Level, want.Level)
			}
			if decoded.Message != want.Message {
				t.Errorf("Message = %q, want %q", decoded.Message, want.Message)
			}
			if string(decoded.Fields) != string(want.Fields) {
				t.Errorf("Fields = %s, want %s", decoded.Fields, want.Fields)
			}
		})
	}
}

func TestDecodeLogEntry_TruncatedData(t *testing.T) {
	full := LogEntry{ID: 1, Timestamp: 100, Source: "s", Level: "info", Message: "hello"}.Encode()

	// A crash mid-write can leave a truncated record in the WAL. Decoding
	// a truncated buffer must return an error, never panic - this is what
	// the WAL replay path relies on to detect a torn write at the tail.
	for cut := 0; cut < len(full); cut++ {
		truncated := full[:cut]
		_, err := DecodeLogEntry(truncated)
		if err == nil {
			t.Errorf("expected an error decoding %d/%d bytes (truncated), got nil", cut, len(full))
		}
	}
}

func TestPoint_EncodeDecodeRoundTrip(t *testing.T) {
	tests := []struct {
		name  string
		point Point
	}{
		{
			name: "typical point with tags",
			point: Point{
				Series: "cpu.usage", Tags: map[string]string{"host": "pi4", "core": "0"},
				Timestamp: 1234567890, Value: 42.5,
			},
		},
		{
			name:  "no tags",
			point: Point{Series: "mem.free", Tags: nil, Timestamp: 1, Value: 0},
		},
		{
			name:  "negative value",
			point: Point{Series: "temp.delta", Timestamp: 1, Value: -12.75},
		},
		{
			name:  "zero value",
			point: Point{Series: "counter", Timestamp: 1, Value: 0},
		},
		{
			name:  "very precise float",
			point: Point{Series: "precise", Timestamp: 1, Value: 3.14159265358979},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			encoded := tt.point.Encode()
			decoded, err := DecodePoint(encoded)
			if err != nil {
				t.Fatalf("decode failed: %v", err)
			}

			if decoded.Series != tt.point.Series {
				t.Errorf("Series = %q, want %q", decoded.Series, tt.point.Series)
			}
			if decoded.Timestamp != tt.point.Timestamp {
				t.Errorf("Timestamp = %d, want %d", decoded.Timestamp, tt.point.Timestamp)
			}
			if decoded.Value != tt.point.Value {
				t.Errorf("Value = %v, want %v", decoded.Value, tt.point.Value)
			}
			wantTags := tt.point.Tags
			if wantTags == nil {
				wantTags = map[string]string{}
			}
			if !reflect.DeepEqual(decoded.Tags, wantTags) {
				t.Errorf("Tags = %v, want %v", decoded.Tags, wantTags)
			}
		})
	}
}

func TestDecodePoint_TruncatedData(t *testing.T) {
	full := Point{Series: "cpu", Tags: map[string]string{"host": "x"}, Timestamp: 1, Value: 1.5}.Encode()

	for cut := 0; cut < len(full); cut++ {
		_, err := DecodePoint(full[:cut])
		if err == nil {
			t.Errorf("expected an error decoding %d/%d bytes (truncated), got nil", cut, len(full))
		}
	}
}
