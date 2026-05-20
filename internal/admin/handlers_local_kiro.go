// [fork] Local-machine import for the Kiro CLI SQLite database. We
// already use this path in single-account mode (auth.NewAuthManager +
// GetToken). The local-import flow lets a user click "从本机 Kiro 导入"
// in the modal, the server reads the SQLite, builds a *pool.Credential
// and registers it. Subsequent token refresh follows the same flow as
// for any other multi-account credential.

package admin

import (
	"encoding/json/v2"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/niuma/kirocc-pro/internal/auth"
	"github.com/niuma/kirocc-pro/internal/config"
	"github.com/niuma/kirocc-pro/internal/pool"
)

type localKiroReq struct {
	DBPath string `json:"db_path,omitempty"` // optional override
	ID     string `json:"id,omitempty"`      // optional credential ID
	Label  string `json:"label,omitempty"`   // optional label
}

type localKiroResp struct {
	ID         string `json:"id"`
	Label      string `json:"label"`
	ProfileARN string `json:"profile_arn"`
	Region     string `json:"region"`
	AuthType   string `json:"auth_type"`
}

func (s *Server) handleLocalKiroImport(w http.ResponseWriter, r *http.Request) {
	if !s.allowMutate(w) {
		return
	}
	var body localKiroReq
	if r.ContentLength > 0 {
		if err := json.UnmarshalRead(r.Body, &body); err != nil {
			http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
			return
		}
	}
	dbPath := strings.TrimSpace(body.DBPath)
	if dbPath == "" {
		dbPath = config.DefaultDBPath()
	}
	if dbPath == "" {
		http.Error(w, "could not resolve default Kiro CLI database path", http.StatusBadRequest)
		return
	}

	db, err := auth.OpenDB(dbPath)
	if err != nil {
		http.Error(w, "open Kiro CLI DB: "+err.Error(), http.StatusBadRequest)
		return
	}
	defer func() { _ = db.Close() }()

	creds, err := auth.ReadCredentials(db)
	if err != nil {
		http.Error(w, "read credentials: "+err.Error(), http.StatusBadRequest)
		return
	}
	if creds.AccessToken == "" {
		http.Error(w, "Kiro CLI DB has no access token (please log in via kiro CLI first)", http.StatusBadRequest)
		return
	}

	id := strings.TrimSpace(body.ID)
	if id == "" {
		id = fmt.Sprintf("kiro-local-%d", time.Now().UnixNano()%1_000_000_000)
	}
	label := strings.TrimSpace(body.Label)
	if label == "" {
		label = "Kiro (本机导入)"
	}

	cred := &pool.Credential{
		ID:          id,
		Label:       label,
		Provider:    "kiro",
		Priority:    100,
		Credentials: *creds,
	}
	if err := s.sched.Add(cred); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	if s.credsPath != "" {
		if err := s.persistCreds(); err != nil {
			_ = s.sched.Remove(cred.ID)
			http.Error(w, "persist: "+err.Error(), http.StatusInternalServerError)
			return
		}
	}
	writeJSON(w, http.StatusOK, localKiroResp{
		ID:         cred.ID,
		Label:      cred.Label,
		ProfileARN: creds.ProfileARN,
		Region:     creds.Region,
		AuthType:   creds.AuthType,
	})
}
