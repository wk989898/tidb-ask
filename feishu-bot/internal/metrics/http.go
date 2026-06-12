package metrics

import (
	"embed"
	"encoding/json"
	"io/fs"
	"net/http"
	"strconv"
	"strings"
	"time"
)

//go:embed web/*
var webFS embed.FS

func RegisterHandlers(mux *http.ServeMux, recorder *Recorder) {
	if mux == nil {
		return
	}

	// Redirect /metrics -> /metrics/ so relative asset paths work.
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		if r == nil || r.URL == nil || r.URL.Path != "/metrics" {
			http.NotFound(w, r)
			return
		}
		http.Redirect(w, r, "/metrics/", http.StatusTemporaryRedirect)
	})

	// JSON API for time series.
	mux.HandleFunc("/metrics/api", func(w http.ResponseWriter, r *http.Request) {
		if recorder == nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("metrics disabled"))
			return
		}
		handleAPI(w, r, recorder)
	})

	// Live metrics (real-time gauges).
	mux.HandleFunc("/metrics/live", func(w http.ResponseWriter, r *http.Request) {
		if recorder == nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("metrics disabled"))
			return
		}
		handleLive(w, r, recorder)
	})

	sub, err := fs.Sub(webFS, "web")
	if err == nil {
		mux.Handle("/metrics/", http.StripPrefix("/metrics/", http.FileServer(http.FS(sub))))
	}
}

func handleAPI(w http.ResponseWriter, r *http.Request, recorder *Recorder) {
	q := r.URL.Query()

	now := time.Now()
	from, to := parseTimeRange(q, now)

	stepSec := int64(600)
	if raw := strings.TrimSpace(q.Get("step_sec")); raw != "" {
		if n, err := strconv.ParseInt(raw, 10, 64); err == nil && n > 0 {
			stepSec = n
		}
	}
	step := time.Duration(stepSec) * time.Second

	pending := recorder.SnapshotPending()
	res, err := QueryFile(recorder.filePath, from, to, step, pending)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(err.Error()))
		return
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(res)
}

func handleLive(w http.ResponseWriter, r *http.Request, recorder *Recorder) {
	type live struct {
		TSMS     int64 `json:"ts_ms"`
		InFlight int64 `json:"in_flight"`
	}
	resp := live{
		TSMS:     time.Now().UnixMilli(),
		InFlight: recorder.InFlight(),
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(resp)
}

func parseTimeRange(q map[string][]string, now time.Time) (time.Time, time.Time) {
	// Priority:
	// 1) from/to params (unix seconds or RFC3339)
	// 2) range param (duration like 24h, 7d)
	// 3) default last 24h

	get := func(key string) string {
		v := q[key]
		if len(v) == 0 {
			return ""
		}
		return strings.TrimSpace(v[len(v)-1])
	}

	parseOne := func(raw string) (time.Time, bool) {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			return time.Time{}, false
		}
		if n, err := strconv.ParseInt(raw, 10, 64); err == nil {
			// Accept seconds or milliseconds.
			if n > 1_000_000_000_000 {
				return time.Unix(0, n*int64(time.Millisecond)).UTC(), true
			}
			return time.Unix(n, 0).UTC(), true
		}
		if t, err := time.Parse(time.RFC3339, raw); err == nil {
			return t.UTC(), true
		}
		return time.Time{}, false
	}

	if f, ok := parseOne(get("from")); ok {
		if t, ok2 := parseOne(get("to")); ok2 {
			return f, t
		}
		return f, now.UTC()
	}

	rng := get("range")
	if rng == "" {
		rng = "24h"
	}
	d := parseDurationWithDays(rng)
	if d <= 0 {
		d = 24 * time.Hour
	}
	return now.Add(-d).UTC(), now.UTC()
}

func parseDurationWithDays(s string) time.Duration {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" {
		return 0
	}
	// Support a simple "Xd" suffix.
	if strings.HasSuffix(s, "d") {
		n, err := strconv.Atoi(strings.TrimSuffix(s, "d"))
		if err == nil && n > 0 {
			return time.Duration(n) * 24 * time.Hour
		}
	}
	d, _ := time.ParseDuration(s)
	return d
}
