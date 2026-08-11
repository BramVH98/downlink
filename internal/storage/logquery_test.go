package storage

import "testing"

func entry(source, level, message string, fields string) LogEntry {
	return LogEntry{
		Timestamp: 1000, Source: source, Level: level, Message: message,
		Fields: []byte(fields),
	}
}

func TestLogQuery_MatchesEntry_BasicFilters(t *testing.T) {
	e := entry("auth-api", "error", "Database timeout", `{}`)

	tests := []struct {
		name  string
		query LogQuery
		want  bool
	}{
		{"no filters matches everything", LogQuery{}, true},
		{"matching source", LogQuery{Source: "auth-api"}, true},
		{"non-matching source", LogQuery{Source: "other-service"}, false},
		{"matching level", LogQuery{Level: "error"}, true},
		{"non-matching level", LogQuery{Level: "info"}, false},
		{"message contains, case insensitive", LogQuery{Contains: "DATABASE"}, true},
		{"message doesn't contain", LogQuery{Contains: "nginx"}, false},
		{"combined matching filters", LogQuery{Source: "auth-api", Level: "error"}, true},
		{"combined, one filter fails", LogQuery{Source: "auth-api", Level: "info"}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.query.MatchesEntry(e)
			if got != tt.want {
				t.Errorf("MatchesEntry() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestLogQuery_MatchesEntry_TimeRange(t *testing.T) {
	e := LogEntry{Timestamp: 1000}

	tests := []struct {
		name       string
		start, end int64
		want       bool
	}{
		{"no range", 0, 0, true},
		{"within range", 500, 1500, true},
		{"exactly at start boundary (inclusive)", 1000, 0, true},
		{"exactly at end boundary (inclusive)", 0, 1000, true},
		{"before start", 1001, 0, false},
		{"after end", 0, 999, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			q := LogQuery{Start: tt.start, End: tt.end}
			got := q.MatchesEntry(e)
			if got != tt.want {
				t.Errorf("MatchesEntry() = %v, want %v (start=%d end=%d ts=%d)", got, tt.want, tt.start, tt.end, e.Timestamp)
			}
		})
	}
}

func TestLogQuery_MatchesEntry_FieldEq(t *testing.T) {
	e := entry("auth-api", "info", "login", `{"user":"123","country":"BE"}`)

	tests := []struct {
		name string
		eq   map[string]string
		want bool
	}{
		{"matching string field", map[string]string{"user": "123"}, true},
		{"non-matching value", map[string]string{"user": "456"}, false},
		{"field doesn't exist", map[string]string{"missing_field": "x"}, false},
		{"multiple fields, all match", map[string]string{"user": "123", "country": "BE"}, true},
		{"multiple fields, one fails", map[string]string{"user": "123", "country": "US"}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			q := LogQuery{FieldEq: tt.eq}
			got := q.MatchesEntry(e)
			if got != tt.want {
				t.Errorf("MatchesEntry() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestLogQuery_MatchesEntry_FieldNumericComparisons(t *testing.T) {
	e := entry("auth-api", "error", "slow request", `{"duration_ms":1500,"country":"BE"}`)

	tests := []struct {
		name string
		q    LogQuery
		want bool
	}{
		{"gt: value above threshold", LogQuery{FieldGT: map[string]float64{"duration_ms": 1000}}, true},
		{"gt: value below threshold", LogQuery{FieldGT: map[string]float64{"duration_ms": 2000}}, false},
		{"gt: value exactly at threshold (exclusive)", LogQuery{FieldGT: map[string]float64{"duration_ms": 1500}}, false},
		{"lt: value below threshold", LogQuery{FieldLT: map[string]float64{"duration_ms": 2000}}, true},
		{"lt: value above threshold", LogQuery{FieldLT: map[string]float64{"duration_ms": 1000}}, false},
		{
			"the real-world combined case: duration_ms > 1000 AND country = BE",
			LogQuery{FieldGT: map[string]float64{"duration_ms": 1000}, FieldEq: map[string]string{"country": "BE"}},
			true,
		},
		{
			"combined case, numeric passes but string field fails",
			LogQuery{FieldGT: map[string]float64{"duration_ms": 1000}, FieldEq: map[string]string{"country": "US"}},
			false,
		},
		{
			"gt against a non-numeric field value fails safely, no panic",
			LogQuery{FieldGT: map[string]float64{"country": 100}},
			false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.q.MatchesEntry(e)
			if got != tt.want {
				t.Errorf("MatchesEntry() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestLogQuery_MatchesEntry_MalformedFieldsJSON(t *testing.T) {
	// An entry with corrupted/non-JSON fields shouldn't panic a query -
	// it should just fail to match any field-based filter, since there's
	// nothing valid to check against.
	e := entry("s", "info", "m", `not valid json{{{`)

	q := LogQuery{FieldEq: map[string]string{"user": "123"}}
	got := q.MatchesEntry(e)
	if got {
		t.Error("expected no match against malformed fields JSON, got true")
	}
}

func TestLogQuery_MatchesEntry_EmptyFieldsJSON(t *testing.T) {
	e := entry("s", "info", "m", ``) // Fields is empty, not even "{}"

	q := LogQuery{FieldEq: map[string]string{"user": "123"}}
	got := q.MatchesEntry(e)
	if got {
		t.Error("expected no match when Fields is empty, got true")
	}

	// But a query with no field filters at all should still match fine -
	// the field-parsing path shouldn't even be triggered.
	q2 := LogQuery{Source: "s"}
	if !q2.MatchesEntry(e) {
		t.Error("expected match on non-field filters even when Fields is empty")
	}
}

func TestLogStore_Query_ReturnsNewestFirst(t *testing.T) {
	s := NewLogStore()
	s.Put(LogEntry{ID: 1, Timestamp: 100, Source: "s", Level: "info", Message: "first"})
	s.Put(LogEntry{ID: 2, Timestamp: 300, Source: "s", Level: "info", Message: "third"})
	s.Put(LogEntry{ID: 3, Timestamp: 200, Source: "s", Level: "info", Message: "second"})

	results := s.Query(LogQuery{}, 0)
	if len(results) != 3 {
		t.Fatalf("expected 3 results, got %d", len(results))
	}
	if results[0].Message != "third" || results[1].Message != "second" || results[2].Message != "first" {
		t.Errorf("expected newest-first order (third, second, first), got (%s, %s, %s)",
			results[0].Message, results[1].Message, results[2].Message)
	}
}

func TestLogStore_Query_RespectsLimit(t *testing.T) {
	s := NewLogStore()
	for i := 0; i < 10; i++ {
		s.Put(LogEntry{Timestamp: int64(i), Source: "s", Level: "info", Message: "m"})
	}

	results := s.Query(LogQuery{}, 3)
	if len(results) != 3 {
		t.Errorf("expected 3 results with limit=3, got %d", len(results))
	}
}
