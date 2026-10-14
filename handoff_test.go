package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func isolateHandoff(t *testing.T) {
	t.Helper()
	oldPool, oldPath, oldTransport := pool, poolPath, httpClient.Transport
	oldPaused := handoffPaused
	pool = &AccountPool{Accounts: []*Account{}, Keys: []string{"gateway-key"}, Models: []Model{}}
	poolPath = filepath.Join(t.TempDir(), ".cline-accounts.json")
	handoffPaused = false
	t.Cleanup(func() {
		pool, poolPath, httpClient.Transport = oldPool, oldPath, oldTransport
		handoffPaused = oldPaused
	})
}

func handoffRequest(method, body, key string) *http.Request {
	req := httptest.NewRequest(method, "/admin/api/accounts/handoff", strings.NewReader(body))
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	req.RemoteAddr = "127.0.0.1:9"
	return req
}

func decodeHandoff(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode %d %s: %v", rec.Code, rec.Body.String(), err)
	}
	return out
}

func TestHandoffRejectsMissingOrWrongKey(t *testing.T) {
	isolateHandoff(t)
	mux := http.NewServeMux()
	registerAdminRoutes(mux)
	for _, key := range []string{"", "wrong"} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, handoffRequest(http.MethodGet, "", key))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("key %q status = %d, want 401", key, rec.Code)
		}
		if strings.Contains(rec.Body.String(), "accessToken") || strings.Contains(rec.Body.String(), "refreshToken") {
			t.Fatalf("unauth body leaked credentials: %s", rec.Body.String())
		}
	}
}

func TestHandoffExportRequiresPaused(t *testing.T) {
	isolateHandoff(t)
	pool.Accounts = []*Account{{
		AccountID: "a1", Email: "a@example.com", AccessToken: "workos:live",
		RefreshToken: "rt-live", ExpiresAt: 99, Status: "active", UsageCount: 7,
	}}
	mux := http.NewServeMux()
	registerAdminRoutes(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, handoffRequest(http.MethodGet, "", "gateway-key"))
	if rec.Code != http.StatusConflict && rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 or 409 while running", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "rt-live") || strings.Contains(rec.Body.String(), "workos:live") {
		t.Fatal("running export returned tokens")
	}
}

func TestHandoffPauseWaitsForInflightRefreshAndBlocksNewRefresh(t *testing.T) {
	isolateHandoff(t)
	started := make(chan struct{})
	release := make(chan struct{})
	var calls int
	httpClient.Transport = freeModelRoundTripper(func(req *http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			close(started)
			<-release
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(`{"data":{"accessToken":"new","refreshToken":"rt-new","expiresAt":4102444800000}}`)),
			Header:     make(http.Header),
			Request:    req,
		}, nil
	})
	acc := &Account{AccountID: "a1", Email: "a@example.com", RefreshToken: "rt-old", Status: "active", UsageCount: 4}
	pool.Accounts = []*Account{acc}

	done := make(chan error, 1)
	go func() { done <- refreshAccountToken(acc) }()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("inflight refresh did not reach upstream")
	}

	mux := http.NewServeMux()
	registerAdminRoutes(mux)
	paused := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, handoffRequest(http.MethodPost, `{"paused":true}`, "gateway-key"))
		paused <- rec
	}()
	select {
	case rec := <-paused:
		t.Fatalf("pause returned before inflight refresh finished: %d %s", rec.Code, rec.Body.String())
	case <-time.After(150 * time.Millisecond):
	}
	close(release)

	var rec *httptest.ResponseRecorder
	select {
	case rec = <-paused:
	case <-time.After(2 * time.Second):
		t.Fatal("pause did not return after inflight refresh")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("pause status = %d %s", rec.Code, rec.Body.String())
	}
	<-done
	if calls != 1 {
		t.Fatalf("inflight upstream calls = %d, want 1", calls)
	}

	if err := refreshAccountToken(acc); err == nil {
		t.Fatal("refresh while paused should fail")
	}
	if calls != 1 {
		t.Fatalf("paused refresh reached upstream, calls = %d", calls)
	}
	if acc.Status != "active" || acc.UsageCount != 4 {
		t.Fatalf("paused refresh mutated account: status=%s usage=%d", acc.Status, acc.UsageCount)
	}
}

func TestHandoffSyncIsStrictAndKeepsStats(t *testing.T) {
	isolateHandoff(t)
	until := time.Now().Add(time.Hour)
	pool.Accounts = []*Account{{
		AccountID: "a1", Email: "a@example.com", AccessToken: "workos:old", RefreshToken: "rt-old",
		ExpiresAt: 1, Status: "cooldown", UsageCount: 9, PromptTokens: 3,
		ModelStats:     map[string]*ModelStat{"m": {ModelID: "m", UsageCount: 2}},
		ModelCooldowns: map[string]time.Time{"m": until},
	}}
	mux := http.NewServeMux()
	registerAdminRoutes(mux)
	pause := httptest.NewRecorder()
	mux.ServeHTTP(pause, handoffRequest(http.MethodPost, `{"paused":true}`, "gateway-key"))
	if pause.Code != http.StatusOK {
		t.Fatalf("pause = %d %s", pause.Code, pause.Body.String())
	}

	bad := httptest.NewRecorder()
	mux.ServeHTTP(bad, handoffRequest(http.MethodPost, `{"credentials":[{"accountId":"other","accessToken":"x","refreshToken":"y","expiresAt":9}]}`, "gateway-key"))
	if bad.Code != http.StatusBadRequest && bad.Code != http.StatusConflict {
		t.Fatalf("mismatched ids status = %d", bad.Code)
	}
	if pool.Accounts[0].RefreshToken != "rt-old" {
		t.Fatal("rejected sync changed refresh token")
	}

	body := `{"credentials":[{"accountId":"a1","email":"changed@example.com","accessToken":"workos:fresh","refreshToken":"rt-fresh","expiresAt":42,"status":"expired"}]}`
	ok := httptest.NewRecorder()
	mux.ServeHTTP(ok, handoffRequest(http.MethodPost, body, "gateway-key"))
	if ok.Code != http.StatusOK {
		t.Fatalf("sync = %d %s", ok.Code, ok.Body.String())
	}
	got := pool.Accounts[0]
	if got.AccessToken != "workos:fresh" || got.RefreshToken != "rt-fresh" || got.ExpiresAt != 42 {
		t.Fatalf("credentials = %+v", got)
	}
	if got.Email != "a@example.com" || got.Status != "cooldown" || got.UsageCount != 9 || got.PromptTokens != 3 {
		t.Fatalf("sync changed account state: %+v", got)
	}
	if got.ModelStats["m"].UsageCount != 2 || !got.ModelCooldowns["m"].Equal(until) {
		t.Fatal("sync changed model stats or cooldown")
	}
}

func TestHandoffResumeAndPausedPersists(t *testing.T) {
	isolateHandoff(t)
	mux := http.NewServeMux()
	registerAdminRoutes(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, handoffRequest(http.MethodPost, `{"paused":true}`, "gateway-key"))
	if rec.Code != http.StatusOK {
		t.Fatalf("pause = %d", rec.Code)
	}
	raw, err := os.ReadFile(poolPath + ".handoff-paused")
	if err != nil || string(raw) != "true\n" {
		t.Fatalf("paused flag not persisted: %v %s", err, raw)
	}
	savePool()
	pool = nil
	handoffPaused = false
	if !accountsHandoffPaused() {
		t.Fatal("restart did not restore paused")
	}
	savePoolAfter := loadPool()
	poolMu.Lock()
	savePoolAfter.CurrentIdx = 0
	savePool()
	poolMu.Unlock()
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, handoffRequest(http.MethodPost, `{"paused":false}`, "gateway-key"))
	savePool()
	if rec.Code != http.StatusOK || accountsHandoffPaused() {
		t.Fatalf("resume failed: %d", rec.Code)
	}
}
