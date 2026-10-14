package main

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

var handoffMu sync.Mutex
var handoffControlMu sync.Mutex
var handoffPaused bool
var handoffInflight sync.WaitGroup
var errHandoffPaused = errors.New("account handoff paused")

type handoffCredential struct {
	AccountID    string `json:"accountId"`
	Email        string `json:"email,omitempty"`
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	ExpiresAt    int64  `json:"expiresAt"`
}

func accountsHandoffPaused() bool {
	handoffMu.Lock()
	defer handoffMu.Unlock()
	return handoffPaused || storedHandoffPaused()
}

func beginAccountRefresh() error {
	handoffMu.Lock()
	defer handoffMu.Unlock()
	if handoffPaused || storedHandoffPaused() {
		return errHandoffPaused
	}
	handoffInflight.Add(1)
	return nil
}

func persistHandoffLocked(p *AccountPool) error {
	data, err := marshalPoolStorage(p)
	if err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(poolPath), ".cline-handoff-*")
	if err != nil {
		return err
	}
	defer os.Remove(temporary.Name())
	if _, err = temporary.Write(data); err != nil {
		temporary.Close()
		return err
	}
	if err = temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err = temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporary.Name(), poolPath)
}

func storedHandoffPaused() bool {
	data, err := os.ReadFile(poolPath + ".handoff-paused")
	if errors.Is(err, os.ErrNotExist) {
		return false
	}
	return err != nil || string(data) != "false\n"
}

func setAccountsHandoffPaused(paused bool) error {
	handoffMu.Lock()
	previous := handoffPaused
	if paused {
		handoffPaused = true
	}
	handoffMu.Unlock()
	if paused {
		handoffInflight.Wait()
	}
	body := "false\n"
	if paused {
		body = "true\n"
	}
	temporary, err := os.CreateTemp(filepath.Dir(poolPath), ".cline-pause-*")
	if err == nil {
		defer os.Remove(temporary.Name())
		_, err = temporary.WriteString(body)
		if err == nil {
			err = temporary.Sync()
		}
		closeErr := temporary.Close()
		if err == nil {
			err = closeErr
		}
		if err == nil {
			err = os.Rename(temporary.Name(), poolPath+".handoff-paused")
		}
	}
	handoffMu.Lock()
	if err != nil {
		handoffPaused = previous
	} else {
		handoffPaused = paused
	}
	handoffMu.Unlock()
	return err
}

func handoffAuthorized(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return false
	}
	values := r.Header.Values("Authorization")
	if len(values) != 1 || !strings.HasPrefix(values[0], "Bearer ") {
		return false
	}
	candidate := strings.TrimPrefix(values[0], "Bearer ")
	if candidate == "" {
		return false
	}
	p := loadPool()
	poolMu.Lock()
	defer poolMu.Unlock()
	for _, key := range p.Keys {
		if key != "" && subtle.ConstantTimeCompare([]byte(candidate), []byte(key)) == 1 {
			return true
		}
	}
	return false
}

func handleAccountsHandoff(w http.ResponseWriter, r *http.Request) {
	if !handoffAuthorized(r) {
		writeAPI(w, http.StatusUnauthorized, apiResponse{Error: "invalid handoff key"})
		return
	}
	handoffControlMu.Lock()
	defer handoffControlMu.Unlock()
	if r.Method == http.MethodGet {
		if !accountsHandoffPaused() {
			writeAPI(w, http.StatusConflict, apiResponse{Error: "pause accounts before exporting credentials"})
			return
		}
		p := loadPool()
		poolMu.Lock()
		credentials := make([]handoffCredential, 0, len(p.Accounts))
		for _, account := range p.Accounts {
			credentials = append(credentials, handoffCredential{account.AccountID, account.Email, account.AccessToken, account.RefreshToken, account.ExpiresAt})
		}
		poolMu.Unlock()
		w.Header().Set("Cache-Control", "no-store")
		writeAPI(w, http.StatusOK, apiResponse{Success: true, Data: map[string]any{"paused": true, "accounts": credentials}})
		return
	}
	if r.Method != http.MethodPost {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	var payload struct {
		Paused      *bool                `json:"paused"`
		Credentials *[]handoffCredential `json:"credentials"`
	}
	decoder := json.NewDecoder(io.LimitReader(r.Body, 1024*1024))
	if err := decoder.Decode(&payload); err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "invalid handoff payload"})
		return
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF || (payload.Paused == nil) == (payload.Credentials == nil) {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "provide paused or credentials"})
		return
	}
	if payload.Paused != nil {
		if err := setAccountsHandoffPaused(*payload.Paused); err != nil {
			writeAPI(w, http.StatusInternalServerError, apiResponse{Error: "cannot persist handoff state"})
			return
		}
		writeAPI(w, http.StatusOK, apiResponse{Success: true, Data: map[string]any{"paused": *payload.Paused}})
		return
	}
	if !accountsHandoffPaused() {
		writeAPI(w, http.StatusConflict, apiResponse{Error: "pause accounts before updating credentials"})
		return
	}
	p := loadPool()
	poolMu.Lock()
	defer poolMu.Unlock()
	incoming := make(map[string]handoffCredential)
	for _, item := range *payload.Credentials {
		if item.AccountID == "" || strings.TrimSpace(item.AccessToken) == "" || strings.TrimSpace(item.RefreshToken) == "" || item.ExpiresAt <= 0 {
			writeAPI(w, http.StatusBadRequest, apiResponse{Error: "incomplete credentials"})
			return
		}
		if _, duplicate := incoming[item.AccountID]; duplicate {
			writeAPI(w, http.StatusBadRequest, apiResponse{Error: "duplicate account"})
			return
		}
		incoming[item.AccountID] = item
	}
	if len(incoming) != len(p.Accounts) {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "account set mismatch"})
		return
	}
	previous := make([]handoffCredential, 0, len(p.Accounts))
	for _, account := range p.Accounts {
		if _, exists := incoming[account.AccountID]; !exists {
			writeAPI(w, http.StatusBadRequest, apiResponse{Error: "account set mismatch"})
			return
		}
		previous = append(previous, handoffCredential{AccountID: account.AccountID, AccessToken: account.AccessToken, RefreshToken: account.RefreshToken, ExpiresAt: account.ExpiresAt})
	}
	for _, account := range p.Accounts {
		item := incoming[account.AccountID]
		account.AccessToken, account.RefreshToken, account.ExpiresAt = item.AccessToken, item.RefreshToken, item.ExpiresAt
	}
	if err := persistHandoffLocked(p); err != nil {
		for _, account := range p.Accounts {
			for _, item := range previous {
				if item.AccountID == account.AccountID {
					account.AccessToken, account.RefreshToken, account.ExpiresAt = item.AccessToken, item.RefreshToken, item.ExpiresAt
				}
			}
		}
		writeAPI(w, http.StatusInternalServerError, apiResponse{Error: "cannot persist credentials"})
		return
	}
	writeAPI(w, http.StatusOK, apiResponse{Success: true, Data: map[string]any{"paused": true, "updated": len(incoming)}})
}
