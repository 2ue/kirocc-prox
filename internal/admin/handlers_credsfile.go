// [fork] New file added in fork. Implements the credentials-file editor
// endpoints used by the admin "认证文件" page. The page lets an operator
// view, re-upload, or download the on-disk JSON pool file without
// touching the host filesystem directly.

package admin

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/niuma/kirocc-pro/internal/pool"
)

// credsFileResp is the JSON view returned by GET /admin/credsfile.
type credsFileResp struct {
	Path         string    `json:"path"`
	Exists       bool      `json:"exists"`
	Size         int64     `json:"size"`
	LastModified time.Time `json:"last_modified"`
	Content      string    `json:"content"`
}

// credsFilePutReq accepts either the raw on-disk JSON (top-level array) or
// an envelope {"content": "..."}. The server parses, validates and writes
// atomically via pool.SaveToJSON.
type credsFilePutReq struct {
	Content string `json:"content"`
}

func (s *Server) routeCredsFile(mux *http.ServeMux) {
	mux.HandleFunc("GET /admin/credsfile", s.handleCredsFileGet)
	mux.HandleFunc("PUT /admin/credsfile", s.handleCredsFilePut)
	mux.HandleFunc("GET /admin/credsfile/download", s.handleCredsFileDownload)
}

func (s *Server) handleCredsFileGet(w http.ResponseWriter, _ *http.Request) {
	if s.credsPath == "" {
		http.Error(w, "single-account mode: no creds file is in use", http.StatusNotFound)
		return
	}
	st, err := os.Stat(s.credsPath)
	resp := credsFileResp{Path: s.credsPath}
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			resp.Exists = false
			writeJSON(w, http.StatusOK, resp)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	resp.Exists = true
	resp.Size = st.Size()
	resp.LastModified = st.ModTime()

	const maxRead = 4 * 1024 * 1024 // 4 MiB hard cap
	f, err := os.Open(s.credsPath)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer func() { _ = f.Close() }()
	body, err := io.ReadAll(io.LimitReader(f, maxRead))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	resp.Content = string(body)
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleCredsFilePut(w http.ResponseWriter, r *http.Request) {
	if !s.allowMutate(w) {
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 4*1024*1024))
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}

	// Accept both {"content": "..."} envelope and a raw JSON array body.
	content := ""
	if len(body) > 0 && body[0] == '{' {
		var env credsFilePutReq
		if err := json.Unmarshal(body, &env); err == nil && env.Content != "" {
			content = env.Content
		}
	}
	if content == "" {
		content = string(body)
	}

	// Validate the content parses as an array of CredentialFile before
	// touching disk — bad input shouldn't be allowed to corrupt the file.
	var arr []pool.CredentialFile
	if err := json.Unmarshal([]byte(content), &arr); err != nil {
		http.Error(w, "invalid JSON (expected top-level array of credentials): "+err.Error(), http.StatusBadRequest)
		return
	}
	if len(arr) == 0 {
		http.Error(w, "credentials array must contain at least one entry", http.StatusBadRequest)
		return
	}

	// Convert + verify each entry has required fields, fail before writing
	// disk if any are bad.
	creds := make([]*pool.Credential, 0, len(arr))
	for i, f := range arr {
		if f.ID == "" {
			http.Error(w, fmt.Sprintf("entry %d: id is required", i), http.StatusBadRequest)
			return
		}
		c, err := credentialFromFile(f)
		if err != nil {
			http.Error(w, fmt.Sprintf("entry %d (%s): %v", i, f.ID, err), http.StatusBadRequest)
			return
		}
		creds = append(creds, c)
	}

	// Ensure parent dir exists, then atomically replace.
	if err := os.MkdirAll(filepath.Dir(s.credsPath), 0o755); err != nil {
		slog.Error("admin: mkdir creds dir failed", "err", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := pool.SaveToJSON(s.credsPath, creds); err != nil {
		slog.Error("admin: save creds file failed", "err", err)
		http.Error(w, "save: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Reload into the in-memory scheduler so the running proxy uses the
	// new content immediately. Register preserves runtime state for any
	// IDs that survive the reload.
	s.sched.Register(creds)

	writeJSON(w, http.StatusOK, map[string]any{
		"path":  s.credsPath,
		"count": len(creds),
	})
}

func (s *Server) handleCredsFileDownload(w http.ResponseWriter, _ *http.Request) {
	if s.credsPath == "" {
		http.Error(w, "single-account mode: no creds file is in use", http.StatusNotFound)
		return
	}
	f, err := os.Open(s.credsPath)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer func() { _ = f.Close() }()
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="credentials.json"`)
	if _, err := io.Copy(w, f); err != nil {
		slog.Warn("admin: download creds file write failed", "err", err)
	}
}
