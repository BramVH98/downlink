/*
Command logserver is a simple self-hosted log and event server built
entirely on the custom WAL/engine storage layer: batched http ingestion
structured fields, filtered queries, a filtered query API, live tail over SSE, singe-page web UI, and operational basics
*/
package main

import (
	"bufio"
	"crypto/subtle"
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"homelab-tsdb/internal/engine"
	"homelab-tsdb/internal/storage"
	"homelab-tsdb/internal/tail"
	"homelab-tsdb/internal/webui"
)

func main() {
	dataDir := flag.String("data", "./data", "directory for the WAL and segment files")
	addr := flag.String("addr", ":8080", "HTTP listen address")
	authUser := flag.String("auth-user", "", "basic auth username (leave empty to disable auth)")
	authPass := flag.String("auth-pass", "", "basic auth password")
	flushThreshold := flag.Int("flush-threshold", 5000, "flush hot buffers to a segment after this many total points+logs")
	retention := flag.Duration("retention", 30*24*time.Hour, "how long to keep data before it's dropped")
	retentionCheck := flag.Duration("retention-check", 1*time.Hour, "how often to run the retention+compaction sweep")
	configFile := flag.String("config", "", "path to a config file (key = value per line, # for comments); any flag also passed on the command line overrides the matching config file value")
	flag.Parse()

	if *configFile != "" {
		if err := applyConfigFile(*configFile); err != nil {
			log.Fatalf("config file: %v", err)
		}
	}

	eng, err := engine.Open(*dataDir, *flushThreshold)
	if err != nil {
		log.Fatalf("open engine: %v", err)
	}
	defer eng.Close()

	if *authUser == "" {
		log.Printf("WARNING: no -auth-user set, running with NO authentication.")
	}

	broadcaster := tail.New[storage.LogEntry]()

	go runRetentionLoop(eng, *retention, *retentionCheck)

	mux := http.NewServeMux()
	mux.HandleFunc("POST /logs", handleIngestLogs(eng, broadcaster))
	mux.HandleFunc("GET /logs", handleQueryLogs(eng))
	mux.HandleFunc("GET /tail", handleTail(broadcaster))
	mux.HandleFunc("GET /services", handleServices(eng))
	mux.HandleFunc("GET /stats", handleStats(eng))
	mux.HandleFunc("GET /export/logs", handleExportLogs(eng))
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	mux.HandleFunc("GET /", handleUI())

	handler := withBasicAuth(*authUser, *authPass, mux)

	log.Printf("homelab-tsdb logserver listening on %s (data: %s, retention: %s)", *addr, *dataDir, *retention)
	log.Fatal(http.ListenAndServe(*addr, handler))
}

// --- config file ---
//
// A minimal Apache-style directive file: "key = value" per line, blank
// lines and lines starting with # are ignored. Keys must match a flag name
// exactly (e.g. "auth-user", "retention-check"). Any flag also passed on
// the command line takes precedence over the matching config file value -
// the file sets the steady-state defaults, flags are for one-off overrides.
func applyConfigFile(path string) error {
	values, err := parseConfigFile(path)
	if err != nil {
		return err
	}

	explicit := make(map[string]bool)
	flag.Visit(func(f *flag.Flag) { explicit[f.Name] = true })

	for key, val := range values {
		if explicit[key] {
			continue
		}
		if flag.Lookup(key) == nil {
			return fmt.Errorf("unknown setting %q (no matching flag)", key)
		}
		if err := flag.Set(key, val); err != nil {
			return fmt.Errorf("invalid value for %q: %w", key, err)
		}
	}
	return nil
}

func parseConfigFile(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()

	values := make(map[string]string)
	scanner := bufio.NewScanner(f)
	lineNum := 0
	for scanner.Scan() {
		lineNum++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("line %d: expected 'key = value', got %q", lineNum, line)
		}
		key := strings.TrimSpace(parts[0])
		val := strings.TrimSpace(parts[1])
		values[key] = val
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return values, nil
}

// --- auth ---

func withBasicAuth(user, pass string, next http.Handler) http.Handler {
	if user == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			next.ServeHTTP(w, r)
			return
		}
		gotUser, gotPass, ok := r.BasicAuth()
		userOK := subtle.ConstantTimeCompare([]byte(gotUser), []byte(user)) == 1
		passOK := subtle.ConstantTimeCompare([]byte(gotPass), []byte(pass)) == 1
		if !ok || !userOK || !passOK {
			w.Header().Set("WWW-Authenticate", `Basic realm="homelab-tsdb"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// --- API response shape ---

type logResponse struct {
	ID      int64           `json:"id"`
	Service string          `json:"service"`
	Level   string          `json:"level"`
	Message string          `json:"message"`
	Fields  json.RawMessage `json:"fields"`
	Ts      int64           `json:"ts"` // unix milliseconds
}

func toResponse(l storage.LogEntry) logResponse {
	fields := l.Fields
	if len(fields) == 0 {
		fields = []byte("{}")
	}
	return logResponse{
		ID: l.ID, Service: l.Source, Level: l.Level, Message: l.Message,
		Fields: json.RawMessage(fields), Ts: l.Timestamp / int64(time.Millisecond),
	}
}

// --- ingestion ---

type ingestLogRequest struct {
	Service   string          `json:"service"`
	Level     string          `json:"level"`
	Message   string          `json:"message"`
	Timestamp json.RawMessage `json:"timestamp"`
	Fields    json.RawMessage `json:"fields"`
}

func parseTimestamp(raw json.RawMessage) (int64, error) {
	if len(raw) == 0 {
		return time.Now().UnixNano(), nil
	}

	var asMillis int64
	if err := json.Unmarshal(raw, &asMillis); err == nil {
		return asMillis * int64(time.Millisecond), nil
	}

	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		if t, err := time.Parse(time.RFC3339Nano, asString); err == nil {
			return t.UnixNano(), nil
		}
		if t, err := time.Parse(time.RFC3339, asString); err == nil {
			return t.UnixNano(), nil
		}
		return 0, fmt.Errorf("timestamp string %q is not valid RFC3339", asString)
	}

	return 0, fmt.Errorf("timestamp must be a unix-millis number or an RFC3339 string")
}

func handleIngestLogs(eng *engine.Engine, broadcaster *tail.Broadcaster[storage.LogEntry]) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var raw json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
			http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
			return
		}

		var batch []ingestLogRequest
		var single ingestLogRequest
		if err := json.Unmarshal(raw, &batch); err != nil {
			if err := json.Unmarshal(raw, &single); err != nil {
				http.Error(w, "body must be a log object or an array of log objects: "+err.Error(), http.StatusBadRequest)
				return
			}
			batch = []ingestLogRequest{single}
		}
		if len(batch) == 0 {
			http.Error(w, "empty batch", http.StatusBadRequest)
			return
		}

		inserted := 0
		for _, req := range batch {
			if req.Service == "" || req.Message == "" {
				http.Error(w, "each log requires service and message", http.StatusBadRequest)
				return
			}
			if req.Level == "" {
				req.Level = "info"
			}
			ts, err := parseTimestamp(req.Timestamp)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}

			entry := storage.LogEntry{
				Timestamp: ts, Source: req.Service, Level: req.Level,
				Message: req.Message, Fields: req.Fields,
			}
			stored, err := eng.WriteLog(entry)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			broadcaster.Publish(stored)
			inserted++
		}

		w.WriteHeader(http.StatusAccepted)
		writeJSON(w, map[string]int{"inserted": inserted})
	}
}

// --- query API ---

func parseFieldParams(values []string) map[string]string {
	out := make(map[string]string)
	for _, v := range values {
		parts := strings.SplitN(v, ":", 2)
		if len(parts) == 2 {
			out[parts[0]] = parts[1]
		}
	}
	return out
}

func parseFieldParamsFloat(values []string) map[string]float64 {
	raw := parseFieldParams(values)
	out := make(map[string]float64, len(raw))
	for k, v := range raw {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			out[k] = f
		}
	}
	return out
}

func parseLogFilter(r *http.Request) (storage.LogQuery, int, int) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	offset, _ := strconv.Atoi(q.Get("offset"))
	startMillis, _ := strconv.ParseInt(q.Get("start"), 10, 64)
	endMillis, _ := strconv.ParseInt(q.Get("end"), 10, 64)

	filter := storage.LogQuery{
		Source:   q.Get("service"),
		Level:    q.Get("level"),
		Contains: q.Get("contains"),
		Start:    startMillis * int64(time.Millisecond),
		FieldEq:  parseFieldParams(q["field"]),
		FieldGT:  parseFieldParamsFloat(q["field_gt"]),
		FieldLT:  parseFieldParamsFloat(q["field_lt"]),
	}
	if endMillis != 0 {
		filter.End = endMillis * int64(time.Millisecond)
	}
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	return filter, limit, offset
}

func handleQueryLogs(eng *engine.Engine) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		filter, limit, offset := parseLogFilter(r)
		rows := eng.QueryLogs(filter, limit, offset)

		out := make([]logResponse, len(rows))
		for i, row := range rows {
			out[i] = toResponse(row)
		}
		writeJSON(w, out)
	}
}

func handleServices(eng *engine.Engine) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, eng.ListServices())
	}
}

type statsResponse struct {
	TotalLogs   int64            `json:"total_logs"`
	TotalEvents int64            `json:"total_events"`
	ByLevel     []levelCountJSON `json:"by_level"`
	ByService   []svcCountJSON   `json:"by_service"`
}
type levelCountJSON struct {
	Level string `json:"level"`
	Count int64  `json:"count"`
}
type svcCountJSON struct {
	Service string `json:"service"`
	Count   int64  `json:"count"`
}

func handleStats(eng *engine.Engine) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		st := eng.Stats()
		resp := statsResponse{TotalLogs: st.TotalLogs, TotalEvents: st.TotalEvents}
		for level, count := range st.ByLevel {
			resp.ByLevel = append(resp.ByLevel, levelCountJSON{level, count})
		}
		for svc, count := range st.ByService {
			resp.ByService = append(resp.ByService, svcCountJSON{svc, count})
		}
		writeJSON(w, resp)
	}
}

// --- live tail (SSE) ---

func handleTail(broadcaster *tail.Broadcaster[storage.LogEntry]) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.WriteHeader(http.StatusOK)
		flusher.Flush()

		ch, cancel := broadcaster.Subscribe()
		defer cancel()

		keepalive := time.NewTicker(15 * time.Second)
		defer keepalive.Stop()

		for {
			select {
			case <-r.Context().Done():
				return
			case entry, ok := <-ch:
				if !ok {
					return
				}
				data, err := json.Marshal(toResponse(entry))
				if err != nil {
					continue
				}
				fmt.Fprintf(w, "data: %s\n\n", data)
				flusher.Flush()
			case <-keepalive.C:
				fmt.Fprint(w, ": keepalive\n\n")
				flusher.Flush()
			}
		}
	}
}

// --- export ---

func handleExportLogs(eng *engine.Engine) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		filter, _, _ := parseLogFilter(r)
		rows := eng.QueryLogs(filter, 100000, 0)

		if r.URL.Query().Get("format") == "csv" {
			w.Header().Set("Content-Type", "text/csv")
			w.Header().Set("Content-Disposition", `attachment; filename="logs-export.csv"`)
			cw := csv.NewWriter(w)
			cw.Write([]string{"id", "service", "level", "message", "fields", "ts"})
			for _, row := range rows {
				r := toResponse(row)
				cw.Write([]string{
					strconv.FormatInt(r.ID, 10), r.Service, r.Level, r.Message,
					string(r.Fields), strconv.FormatInt(r.Ts, 10),
				})
			}
			cw.Flush()
			return
		}

		out := make([]logResponse, len(rows))
		for i, row := range rows {
			out[i] = toResponse(row)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Disposition", `attachment; filename="logs-export.json"`)
		writeJSON(w, out)
	}
}

// --- web UI ---

func handleUI() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(webui.IndexHTML)
	}
}

// --- retention / compaction ---

func runRetentionLoop(eng *engine.Engine, retention, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for range ticker.C {
		cutoff := time.Now().Add(-retention).UnixNano()
		points, logs, err := eng.ApplyRetention(cutoff)
		if err != nil {
			log.Printf("retention sweep failed: %v", err)
			continue
		}
		if points > 0 || logs > 0 {
			log.Printf("retention+compaction: dropped %d points, %d logs older than %s", points, logs, retention)
		}
	}
}

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("write json: %v", err)
	}
}
