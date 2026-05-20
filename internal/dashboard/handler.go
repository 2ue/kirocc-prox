package dashboard

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/niuma/kirocc-pro/internal/config"
	"github.com/niuma/kirocc-pro/internal/logging"
)

// ConfigPatch holds the subset of Config fields that may be changed at runtime.
type ConfigPatch struct {
	Debug         *bool `json:"debug,omitempty"`
	OTelBodyLimit *int  `json:"otel_body_limit,omitempty"`
}

// SafeConfig is the config view exposed to the dashboard (no secrets).
type SafeConfig struct {
	Port          int    `json:"port"`
	Host          string `json:"host"`
	DBPath        string `json:"db_path"`
	APIKeySet     bool   `json:"api_key_set"`
	Debug         bool   `json:"debug"`
	OTel          bool   `json:"otel"`
	OTelBodyLimit int    `json:"otel_body_limit"`
	LogFilePath   string `json:"log_file_path"`
}

// Handler serves the dashboard HTTP endpoints.
type Handler struct {
	collector *Collector
	cfg       atomic.Pointer[config.Config]
}

// NewHandler creates a Handler with the given collector and initial config.
func NewHandler(collector *Collector, cfg config.Config) *Handler {
	h := &Handler{collector: collector}
	h.cfg.Store(&cfg)
	return h
}

// UpdateConfig atomically replaces the stored config snapshot.
func (h *Handler) UpdateConfig(cfg config.Config) {
	h.cfg.Store(&cfg)
}

// RegisterRoutes registers all dashboard routes on mux.
func (h *Handler) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /dashboard", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/dashboard/", http.StatusMovedPermanently)
	})
	// Serve index.html for /dashboard/ and static assets under /dashboard/
	sub, _ := fs.Sub(staticFiles, "web")
	mux.Handle("GET /dashboard/", http.StripPrefix("/dashboard", http.FileServer(http.FS(sub))))
	mux.HandleFunc("GET /dashboard/api/stats", h.serveStats)
	mux.HandleFunc("GET /dashboard/api/logs", h.serveLogs)
	mux.HandleFunc("GET /dashboard/api/stream", h.serveStream)
	mux.HandleFunc("GET /dashboard/api/config", h.serveGetConfig)
	mux.HandleFunc("PUT /dashboard/api/config", h.serveUpdateConfig)
}

func (h *Handler) serveStats(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, h.collector.Stats())
}

func (h *Handler) serveLogs(w http.ResponseWriter, r *http.Request) {
	n := 100
	if v := r.URL.Query().Get("n"); v != "" {
		if parsed, err := strconv.Atoi(v); err == nil && parsed > 0 {
			n = parsed
		}
	}
	writeJSON(w, http.StatusOK, h.collector.RecentRecords(n))
}

func (h *Handler) serveStream(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	// Send a ping so the client knows the connection is live.
	fmt.Fprintf(w, "event: ping\ndata: {}\n\n")
	flusher.Flush()

	ch := h.collector.Subscribe()
	defer h.collector.Unsubscribe(ch)

	for {
		select {
		case rec, ok := <-ch:
			if !ok {
				return
			}
			data, err := json.Marshal(rec)
			if err != nil {
				continue
			}
			fmt.Fprintf(w, "event: request\ndata: %s\n\n", data)
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

func (h *Handler) serveGetConfig(w http.ResponseWriter, r *http.Request) {
	cfg := h.cfg.Load()
	writeJSON(w, http.StatusOK, SafeConfig{
		Port:          cfg.Port,
		Host:          cfg.Host,
		DBPath:        cfg.DBPath,
		APIKeySet:     cfg.APIKey != "",
		Debug:         cfg.Debug,
		OTel:          cfg.OTel,
		OTelBodyLimit: cfg.OTelBodyLimit,
		LogFilePath:   cfg.LogFile.Path,
	})
}

func (h *Handler) serveUpdateConfig(w http.ResponseWriter, r *http.Request) {
	var patch ConfigPatch
	if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}

	// Load current config, apply patch, store back.
	old := h.cfg.Load()
	updated := *old

	if patch.Debug != nil {
		updated.Debug = *patch.Debug
		// Apply to the global logger immediately.
		handler, _ := logging.NewHandler(updated.Debug, updated.LogFile)
		slog.SetDefault(slog.New(handler))
	}
	if patch.OTelBodyLimit != nil {
		if *patch.OTelBodyLimit < 0 {
			writeError(w, http.StatusBadRequest, "otel_body_limit must be >= 0")
			return
		}
		updated.OTelBodyLimit = *patch.OTelBodyLimit
	}

	h.cfg.Store(&updated)
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// isDashboardPath reports whether the request path is under /dashboard/.
func IsDashboardPath(path string) bool {
	return path == "/dashboard" || strings.HasPrefix(path, "/dashboard/")
}
