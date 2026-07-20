// Command server runs homelab-tsdb as an HTTP service: write metrics and
// logs in, query them back out. Durability comes from the WAL (every write
// is fsynced before the HTTP call returns); bounded memory comes from
// periodic flushes to segment files.
package main

import (
	"encoding/json"
	"log"
	"net/http"
	"strconv"

	"homelab-tsdb/internal/engine"
	"homelab-tsdb/internal/storage"
)

func main() {
	e, err := engine.Open("./data", 500)
	if err != nil {
		log.Fatalf("open engine: %v", err)
	}
	defer e.Close()

	mux := http.NewServeMux()
	mux.HandleFunc("POST /write/metric", writeMetricHandler(e))
	mux.HandleFunc("POST /write/log", writeLogHandler(e))
	mux.HandleFunc("GET /query/metrics", queryMetricsHandler(e))
	mux.HandleFunc("GET /query/logs", queryLogsHandler(e))
	mux.HandleFunc("POST /flush", flushHandler(e))
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	})

	addr := ":8428"
	log.Printf("homelab-tsdb listening on %s (data dir: ./data)", addr)
	log.Fatal(http.ListenAndServe(addr, mux))
}

type metricRequest struct {
	Series    string            `json:"series"`
	Tags      map[string]string `json:"tags"`
	Timestamp int64             `json:"timestamp"`
	Value     float64           `json:"value"`
}

func writeMetricHandler(e *engine.Engine) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req metricRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
			return
		}
		if req.Series == "" {
			http.Error(w, "series is required", http.StatusBadRequest)
			return
		}

		p := storage.Point{
			Series:    req.Series,
			Tags:      req.Tags,
			Timestamp: req.Timestamp,
			Value:     req.Value,
		}
		if err := e.WriteMetric(p); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	}
}

type logRequest struct {
	Source    string `json:"source"`
	Level     string `json:"level"`
	Message   string `json:"message"`
	Timestamp int64  `json:"timestamp"`
}

func writeLogHandler(e *engine.Engine) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req logRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
			return
		}
		if req.Message == "" {
			http.Error(w, "message is required", http.StatusBadRequest)
			return
		}

		entry := storage.LogEntry{
			Timestamp: req.Timestamp,
			Source:    req.Source,
			Level:     req.Level,
			Message:   req.Message,
		}
		if err := e.WriteLog(entry); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	}
}

func queryMetricsHandler(e *engine.Engine) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		series := r.URL.Query().Get("series")
		if series == "" {
			http.Error(w, "series query param is required", http.StatusBadRequest)
			return
		}
		start := parseInt64(r.URL.Query().Get("start"), 0)
		end := parseInt64(r.URL.Query().Get("end"), 0)

		points := e.QueryMetrics(series, start, end)
		writeJSON(w, points)
	}
}

func queryLogsHandler(e *engine.Engine) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := storage.LogQuery{
			Source:   r.URL.Query().Get("source"),
			Level:    r.URL.Query().Get("level"),
			Contains: r.URL.Query().Get("contains"),
			Start:    parseInt64(r.URL.Query().Get("start"), 0),
			End:      parseInt64(r.URL.Query().Get("end"), 0),
		}
		limit := int(parseInt64(r.URL.Query().Get("limit"), 100))

		entries := e.QueryLogs(q, limit)
		writeJSON(w, entries)
	}
}

func flushHandler(e *engine.Engine) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := e.Flush(); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Write([]byte("flushed"))
	}
}

func parseInt64(s string, def int64) int64 {
	if s == "" {
		return def
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return def
	}
	return n
}

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("write json: %v", err)
	}
}
