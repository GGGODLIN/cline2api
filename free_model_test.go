package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type freeModelRoundTripper func(*http.Request) (*http.Response, error)

func (f freeModelRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

type flushSignalRecorder struct {
	*httptest.ResponseRecorder
	flushed chan struct{}
	once    sync.Once
}

func newFlushSignalRecorder() *flushSignalRecorder {
	return &flushSignalRecorder{
		ResponseRecorder: httptest.NewRecorder(),
		flushed:          make(chan struct{}),
	}
}

func (r *flushSignalRecorder) Flush() {
	r.ResponseRecorder.Flush()
	r.once.Do(func() { close(r.flushed) })
}

func TestCallClineAPIRefreshRetryReplaysRequestBody(t *testing.T) {
	oldConfig := getProxyConfig()
	oldTransport := httpClient.Transport
	t.Cleanup(func() {
		setProxyConfig(oldConfig)
		httpClient.Transport = oldTransport
	})

	account := &Account{
		AccountID:    "refresh-account",
		Email:        "refresh@example.com",
		RefreshToken: "refresh-token",
		AccessToken:  "old-access-token",
		ExpiresAt:    time.Now().Add(time.Hour).UnixMilli(),
		Status:       "active",
	}
	setProxyConfig(defaultProxyConfig())

	var requestBodies []map[string]any
	refreshCalls := 0
	httpClient.Transport = freeModelRoundTripper(func(req *http.Request) (*http.Response, error) {
		switch req.URL.Path {
		case "/api/v1/auth/refresh":
			refreshCalls++
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(`{"data":{"accessToken":"new-access-token","refreshToken":"new-refresh-token","expiresAt":4102444800000}}`)),
				Header:     make(http.Header),
				Request:    req,
			}, nil
		case "/api/v1/chat/completions":
			body, err := io.ReadAll(req.Body)
			if err != nil {
				return nil, err
			}
			var payload map[string]any
			if err := json.Unmarshal(body, &payload); err != nil {
				return nil, err
			}
			requestBodies = append(requestBodies, payload)
			if len(requestBodies) == 1 {
				return &http.Response{
					StatusCode: http.StatusUnauthorized,
					Body:       io.NopCloser(strings.NewReader(`{"error":"unauthorized"}`)),
					Header:     make(http.Header),
					Request:    req,
				}, nil
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(`{"id":"ok","choices":[]}`)),
				Header:     make(http.Header),
				Request:    req,
			}, nil
		default:
			return nil, fmt.Errorf("unexpected request path %s", req.URL.Path)
		}
	})

	params := map[string]any{
		"model":    freeModelPrimary,
		"messages": []any{map[string]any{"role": "user", "content": "hello"}},
	}
	resp, _, err := callClineAPIWithAccount(account, params, false)
	if err != nil {
		t.Fatalf("callClineAPIWithAccount returned error: %v", err)
	}
	resp.Body.Close()
	if refreshCalls != 1 {
		t.Fatalf("refresh calls = %d, want 1", refreshCalls)
	}
	if len(requestBodies) != 2 {
		t.Fatalf("Cline request count = %d, want 2", len(requestBodies))
	}
	for index, body := range requestBodies {
		if body["model"] != freeModelPrimary {
			t.Fatalf("request %d model = %v, want %q", index, body["model"], freeModelPrimary)
		}
		messages, ok := body["messages"].([]any)
		if !ok || len(messages) == 0 {
			t.Fatalf("request %d messages = %v, want non-empty body", index, body["messages"])
		}
	}
}

func TestCallClineAPIFreeRetriesNextGLMAccountAfterTokenRefreshFailure(t *testing.T) {
	oldPool := pool
	oldConfig := getProxyConfig()
	oldTransport := httpClient.Transport
	t.Cleanup(func() {
		pool = oldPool
		setProxyConfig(oldConfig)
		httpClient.Transport = oldTransport
	})

	first := &Account{
		AccountID:    "refresh-failed",
		Email:        "refresh-failed@example.com",
		RefreshToken: "refresh-one",
		ExpiresAt:    time.Now().Add(-time.Hour).UnixMilli(),
		Status:       "active",
	}
	second := &Account{
		AccountID:   "glm-two",
		Email:       "two@example.com",
		AccessToken: "token-two",
		ExpiresAt:   time.Now().Add(time.Hour).UnixMilli(),
		Status:      "active",
	}
	pool = &AccountPool{Accounts: []*Account{first, second}}
	setProxyConfig(defaultProxyConfig())

	var paths []string
	var attempts []string
	var models []string
	httpClient.Transport = freeModelRoundTripper(func(req *http.Request) (*http.Response, error) {
		paths = append(paths, req.URL.Path)
		switch req.URL.Path {
		case "/api/v1/auth/refresh":
			return &http.Response{
				StatusCode: http.StatusInternalServerError,
				Body:       io.NopCloser(strings.NewReader(`{"error":"refresh failed"}`)),
				Header:     make(http.Header),
				Request:    req,
			}, nil
		case "/api/v1/chat/completions":
			token := strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer ")
			attempts = append(attempts, token)
			body, err := io.ReadAll(req.Body)
			if err != nil {
				return nil, err
			}
			var upstream map[string]any
			if err := json.Unmarshal(body, &upstream); err != nil {
				return nil, err
			}
			model, _ := upstream["model"].(string)
			models = append(models, model)
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(`{"id":"ok","choices":[]}`)),
				Header:     make(http.Header),
				Request:    req,
			}, nil
		default:
			return nil, fmt.Errorf("unexpected request path %s", req.URL.Path)
		}
	})

	params := map[string]any{
		"model":    "free",
		"messages": []any{map[string]any{"role": "user", "content": "hello"}},
	}
	resp, acc, err := callClineAPI(params, false)
	if err != nil {
		t.Fatalf("callClineAPI returned error: %v", err)
	}
	resp.Body.Close()
	if acc != second {
		t.Fatalf("selected account = %v, want second GLM account", acc)
	}
	if got, want := strings.Join(paths, ","), "/api/v1/auth/refresh,/api/v1/chat/completions"; got != want {
		t.Fatalf("request paths = %q, want %q", got, want)
	}
	if got, want := strings.Join(attempts, ","), "token-two"; got != want {
		t.Fatalf("Cline attempts = %q, want %q", got, want)
	}
	if got, want := strings.Join(models, ","), freeModelPrimary; got != want {
		t.Fatalf("models = %q, want %q", got, want)
	}
	if first.Status != "cooldown" {
		t.Fatalf("first account status = %q, want cooldown (a 500 from /auth/refresh is transient)", first.Status)
	}
}

func TestCallClineAPIFreeRetriesNextGLMAccountAfterTransportFailure(t *testing.T) {
	oldPool := pool
	oldConfig := getProxyConfig()
	oldTransport := httpClient.Transport
	t.Cleanup(func() {
		pool = oldPool
		setProxyConfig(oldConfig)
		httpClient.Transport = oldTransport
	})

	first := &Account{
		AccountID:   "transport-failed",
		Email:       "transport-failed@example.com",
		AccessToken: "token-one",
		ExpiresAt:   time.Now().Add(time.Hour).UnixMilli(),
		Status:      "active",
	}
	second := &Account{
		AccountID:   "glm-two",
		Email:       "two@example.com",
		AccessToken: "token-two",
		ExpiresAt:   time.Now().Add(time.Hour).UnixMilli(),
		Status:      "active",
	}
	pool = &AccountPool{Accounts: []*Account{first, second}}
	setProxyConfig(defaultProxyConfig())

	var attempts []string
	var models []string
	httpClient.Transport = freeModelRoundTripper(func(req *http.Request) (*http.Response, error) {
		token := strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer ")
		attempts = append(attempts, token)
		if token == "token-one" {
			return nil, fmt.Errorf("connection reset")
		}
		body, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		var upstream map[string]any
		if err := json.Unmarshal(body, &upstream); err != nil {
			return nil, err
		}
		model, _ := upstream["model"].(string)
		models = append(models, model)
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(`{"id":"ok","choices":[]}`)),
			Header:     make(http.Header),
			Request:    req,
		}, nil
	})

	params := map[string]any{
		"model":    "free",
		"messages": []any{map[string]any{"role": "user", "content": "hello"}},
	}
	resp, acc, err := callClineAPI(params, false)
	if err != nil {
		t.Fatalf("callClineAPI returned error: %v", err)
	}
	resp.Body.Close()
	if acc != second {
		t.Fatalf("selected account = %v, want second GLM account", acc)
	}
	if got, want := strings.Join(attempts, ","), "token-one,token-two"; got != want {
		t.Fatalf("Cline attempts = %q, want %q", got, want)
	}
	if got, want := strings.Join(models, ","), freeModelPrimary; got != want {
		t.Fatalf("models = %q, want %q", got, want)
	}
	if first.Status != "cooldown" {
		t.Fatalf("first account status = %q, want cooldown", first.Status)
	}
}

func TestCallClineAPIFreeRetriesNextGLMAccountAfterQuota429(t *testing.T) {
	oldPool := pool
	oldConfig := getProxyConfig()
	oldTransport := httpClient.Transport
	t.Cleanup(func() {
		pool = oldPool
		setProxyConfig(oldConfig)
		httpClient.Transport = oldTransport
	})

	first := &Account{
		AccountID:   "glm-one",
		Email:       "one@example.com",
		AccessToken: "token-one",
		ExpiresAt:   time.Now().Add(time.Hour).UnixMilli(),
		Status:      "active",
	}
	second := &Account{
		AccountID:   "glm-two",
		Email:       "two@example.com",
		AccessToken: "token-two",
		ExpiresAt:   time.Now().Add(time.Hour).UnixMilli(),
		Status:      "active",
	}
	pool = &AccountPool{Accounts: []*Account{first, second}}
	setProxyConfig(defaultProxyConfig())

	var attempts []string
	var models []string
	httpClient.Transport = freeModelRoundTripper(func(req *http.Request) (*http.Response, error) {
		token := strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer ")
		attempts = append(attempts, token)
		body, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		var upstream map[string]any
		if err := json.Unmarshal(body, &upstream); err != nil {
			return nil, err
		}
		model, _ := upstream["model"].(string)
		models = append(models, model)
		if token == "token-one" {
			return &http.Response{
				StatusCode: http.StatusTooManyRequests,
				Body:       io.NopCloser(strings.NewReader(`{"error":"quota","message":"Try again in 1h"}`)),
				Header:     make(http.Header),
				Request:    req,
			}, nil
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(`{"id":"ok","choices":[]}`)),
			Header:     make(http.Header),
			Request:    req,
		}, nil
	})

	params := map[string]any{
		"model":    "free",
		"messages": []any{map[string]any{"role": "user", "content": "hello"}},
	}
	resp, acc, err := callClineAPI(params, false)
	if err != nil {
		t.Fatalf("callClineAPI returned error: %v", err)
	}
	if resp == nil {
		t.Fatal("callClineAPI returned nil response")
	}
	resp.Body.Close()

	if acc != second {
		t.Fatalf("selected account = %v, want second GLM account", acc)
	}
	if got, want := strings.Join(attempts, ","), "token-one,token-two"; got != want {
		t.Fatalf("attempts = %q, want %q", got, want)
	}
	for index, model := range models {
		if model != freeModelPrimary {
			t.Fatalf("attempt %d model = %q, want %q", index, model, freeModelPrimary)
		}
	}
	if got, want := params["model"], freeModelPrimary; got != want {
		t.Fatalf("effective model = %v, want %q", got, want)
	}
}

func TestCallClineAPIFreeFallsBackToDSAfterAllGLMAccountsUnavailable(t *testing.T) {
	oldPool := pool
	oldConfig := getProxyConfig()
	oldTransport := httpClient.Transport
	t.Cleanup(func() {
		pool = oldPool
		setProxyConfig(oldConfig)
		httpClient.Transport = oldTransport
	})

	first := &Account{
		AccountID:   "glm-one",
		Email:       "one@example.com",
		AccessToken: "token-one",
		ExpiresAt:   time.Now().Add(time.Hour).UnixMilli(),
		Status:      "active",
	}
	second := &Account{
		AccountID:   "glm-two",
		Email:       "two@example.com",
		AccessToken: "token-two",
		ExpiresAt:   time.Now().Add(time.Hour).UnixMilli(),
		Status:      "active",
	}
	pool = &AccountPool{Accounts: []*Account{first, second}}
	setProxyConfig(defaultProxyConfig())

	var attempts []string
	var models []string
	httpClient.Transport = freeModelRoundTripper(func(req *http.Request) (*http.Response, error) {
		token := strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer ")
		attempts = append(attempts, token)
		body, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		var upstream map[string]any
		if err := json.Unmarshal(body, &upstream); err != nil {
			return nil, err
		}
		model, _ := upstream["model"].(string)
		models = append(models, model)
		if token == "token-one" && model == freeModelPrimary || token == "token-two" {
			return &http.Response{
				StatusCode: http.StatusTooManyRequests,
				Body:       io.NopCloser(strings.NewReader(`{"error":"quota","message":"Try again in 1h"}`)),
				Header:     make(http.Header),
				Request:    req,
			}, nil
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(`{"id":"ok","choices":[]}`)),
			Header:     make(http.Header),
			Request:    req,
		}, nil
	})

	params := map[string]any{
		"model":    "free",
		"messages": []any{map[string]any{"role": "user", "content": "hello"}},
	}
	resp, acc, err := callClineAPI(params, false)
	if err != nil {
		t.Fatalf("callClineAPI returned error: %v", err)
	}
	if resp == nil {
		t.Fatal("callClineAPI returned nil response")
	}
	resp.Body.Close()

	if acc != first {
		t.Fatalf("selected account = %v, want first DS account", acc)
	}
	if got, want := strings.Join(attempts, ","), "token-one,token-two,token-one"; got != want {
		t.Fatalf("attempts = %q, want %q", got, want)
	}
	if got, want := strings.Join(models, ","), freeModelPrimary+","+freeModelPrimary+","+freeModelFallback; got != want {
		t.Fatalf("models = %q, want %q", got, want)
	}
	if got, want := params["model"], freeModelFallback; got != want {
		t.Fatalf("effective model = %v, want %q", got, want)
	}
}

func TestCallClineAPIFreeRetriesNextDSAccountAfterQuota429(t *testing.T) {
	oldPool := pool
	oldConfig := getProxyConfig()
	oldTransport := httpClient.Transport
	t.Cleanup(func() {
		pool = oldPool
		setProxyConfig(oldConfig)
		httpClient.Transport = oldTransport
	})

	first := &Account{
		AccountID:   "ds-one",
		Email:       "one@example.com",
		AccessToken: "token-one",
		ExpiresAt:   time.Now().Add(time.Hour).UnixMilli(),
		Status:      "active",
		ModelCooldowns: map[string]time.Time{
			freeModelPrimary: time.Now().Add(time.Hour),
		},
	}
	second := &Account{
		AccountID:   "ds-two",
		Email:       "two@example.com",
		AccessToken: "token-two",
		ExpiresAt:   time.Now().Add(time.Hour).UnixMilli(),
		Status:      "active",
		ModelCooldowns: map[string]time.Time{
			freeModelPrimary: time.Now().Add(time.Hour),
		},
	}
	pool = &AccountPool{Accounts: []*Account{first, second}}
	setProxyConfig(defaultProxyConfig())

	var attempts []string
	var models []string
	httpClient.Transport = freeModelRoundTripper(func(req *http.Request) (*http.Response, error) {
		token := strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer ")
		attempts = append(attempts, token)
		body, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		var upstream map[string]any
		if err := json.Unmarshal(body, &upstream); err != nil {
			return nil, err
		}
		model, _ := upstream["model"].(string)
		models = append(models, model)
		if token == "token-one" {
			return &http.Response{
				StatusCode: http.StatusTooManyRequests,
				Body:       io.NopCloser(strings.NewReader(`{"error":"quota","message":"Try again in 1h"}`)),
				Header:     make(http.Header),
				Request:    req,
			}, nil
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(`{"id":"ok","choices":[]}`)),
			Header:     make(http.Header),
			Request:    req,
		}, nil
	})

	params := map[string]any{
		"model":    "free",
		"messages": []any{map[string]any{"role": "user", "content": "hello"}},
	}
	resp, acc, err := callClineAPI(params, false)
	if err != nil {
		t.Fatalf("callClineAPI returned error: %v", err)
	}
	if resp == nil {
		t.Fatal("callClineAPI returned nil response")
	}
	resp.Body.Close()

	if acc != second {
		t.Fatalf("selected account = %v, want second DS account", acc)
	}
	if got, want := strings.Join(attempts, ","), "token-one,token-two"; got != want {
		t.Fatalf("attempts = %q, want %q", got, want)
	}
	if got, want := strings.Join(models, ","), "deepseek/deepseek-v4-flash,deepseek/deepseek-v4-flash"; got != want {
		t.Fatalf("models = %q, want %q", got, want)
	}
	if got, want := params["model"], freeModelFallback; got != want {
		t.Fatalf("effective model = %v, want %q", got, want)
	}
	if !modelCooldownActive(first, freeModelFallback) {
		t.Fatal("first DS account should retain its DS cooldown")
	}
	if !modelCooldownActive(first, freeModelPrimary) {
		t.Fatal("first DS account should retain its GLM cooldown")
	}
}

func TestCallClineAPIFreeReturnsToGLMAfterCooldownExpires(t *testing.T) {
	oldPool := pool
	oldConfig := getProxyConfig()
	oldTransport := httpClient.Transport
	t.Cleanup(func() {
		pool = oldPool
		setProxyConfig(oldConfig)
		httpClient.Transport = oldTransport
	})

	account := &Account{
		AccountID:   "recovery-account",
		Email:       "recovery@example.com",
		AccessToken: "recovery-token",
		ExpiresAt:   time.Now().Add(time.Hour).UnixMilli(),
		Status:      "active",
		ModelCooldowns: map[string]time.Time{
			freeModelPrimary: time.Now().Add(time.Hour),
		},
	}
	pool = &AccountPool{Accounts: []*Account{account}}
	setProxyConfig(defaultProxyConfig())

	var models []string
	httpClient.Transport = freeModelRoundTripper(func(req *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		var upstream map[string]any
		if err := json.Unmarshal(body, &upstream); err != nil {
			return nil, err
		}
		model, _ := upstream["model"].(string)
		models = append(models, model)
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(`{"id":"ok","choices":[]}`)),
			Header:     make(http.Header),
			Request:    req,
		}, nil
	})

	firstResp, _, err := callClineAPI(map[string]any{"model": "free"}, false)
	if err != nil {
		t.Fatalf("first callClineAPI returned error: %v", err)
	}
	firstResp.Body.Close()
	if got, want := models[0], freeModelFallback; got != want {
		t.Fatalf("first effective model = %q, want %q", got, want)
	}

	account.ModelCooldowns[freeModelPrimary] = time.Now().Add(-time.Minute)
	secondResp, _, err := callClineAPI(map[string]any{"model": "free"}, false)
	if err != nil {
		t.Fatalf("second callClineAPI returned error: %v", err)
	}
	secondResp.Body.Close()
	if got, want := models[1], freeModelPrimary; got != want {
		t.Fatalf("second effective model = %q, want %q", got, want)
	}
}

func TestCallClineAPIFreeKeepsModelCooldownsIndependent(t *testing.T) {
	oldPool := pool
	oldConfig := getProxyConfig()
	oldTransport := httpClient.Transport
	t.Cleanup(func() {
		pool = oldPool
		setProxyConfig(oldConfig)
		httpClient.Transport = oldTransport
	})

	for _, test := range []struct {
		name          string
		cooldownModel string
		wantModel     string
	}{
		{name: "GLM cooldown allows DS", cooldownModel: freeModelPrimary, wantModel: freeModelFallback},
		{name: "DS cooldown allows GLM", cooldownModel: freeModelFallback, wantModel: freeModelPrimary},
	} {
		t.Run(test.name, func(t *testing.T) {
			account := &Account{
				AccountID:   "independent-account",
				Email:       "independent@example.com",
				AccessToken: "independent-token",
				ExpiresAt:   time.Now().Add(time.Hour).UnixMilli(),
				Status:      "active",
				ModelCooldowns: map[string]time.Time{
					test.cooldownModel: time.Now().Add(time.Hour),
				},
			}
			pool = &AccountPool{Accounts: []*Account{account}}
			setProxyConfig(defaultProxyConfig())

			var upstreamModel string
			httpClient.Transport = freeModelRoundTripper(func(req *http.Request) (*http.Response, error) {
				body, err := io.ReadAll(req.Body)
				if err != nil {
					return nil, err
				}
				var upstream map[string]any
				if err := json.Unmarshal(body, &upstream); err != nil {
					return nil, err
				}
				upstreamModel, _ = upstream["model"].(string)
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(strings.NewReader(`{"id":"ok","choices":[]}`)),
					Header:     make(http.Header),
					Request:    req,
				}, nil
			})

			resp, _, err := callClineAPI(map[string]any{"model": "free"}, false)
			if err != nil {
				t.Fatalf("callClineAPI returned error: %v", err)
			}
			resp.Body.Close()
			if upstreamModel != test.wantModel {
				t.Fatalf("upstream model = %q, want %q", upstreamModel, test.wantModel)
			}
			if !modelCooldownActive(account, test.cooldownModel) {
				t.Fatalf("cooldown for %s was lost", test.cooldownModel)
			}
			if modelCooldownActive(account, test.wantModel) {
				t.Fatalf("cooldown for %s unexpectedly blocked", test.wantModel)
			}
		})
	}
}

func TestCallClineAPIFreeDoesNotPickCoolingAccount(t *testing.T) {
	oldPool := pool
	oldConfig := getProxyConfig()
	oldTransport := httpClient.Transport
	t.Cleanup(func() {
		pool = oldPool
		setProxyConfig(oldConfig)
		httpClient.Transport = oldTransport
	})

	account := &Account{
		AccountID:      "glm-cooling",
		Email:          "cooling@example.com",
		AccessToken:    "token-cooling",
		ExpiresAt:      time.Now().Add(time.Hour).UnixMilli(),
		Status:         "active",
		ModelCooldowns: map[string]time.Time{},
	}
	for _, model := range freeModelChain {
		account.ModelCooldowns[model] = time.Now().Add(time.Hour)
	}
	pool = &AccountPool{Accounts: []*Account{account}}
	setProxyConfig(defaultProxyConfig())

	calls := 0
	httpClient.Transport = freeModelRoundTripper(func(req *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(`{"id":"unexpected","choices":[]}`)),
			Header:     make(http.Header),
			Request:    req,
		}, nil
	})

	_, _, err := callClineAPI(map[string]any{"model": "free"}, false)
	if err == nil {
		t.Fatal("callClineAPI should fail when every GLM account is cooling")
	}
	if calls != 0 {
		t.Fatalf("upstream calls = %d, want 0", calls)
	}
}

func TestPickAccountForModelStrictPreservesStrategy(t *testing.T) {
	oldPool := pool
	oldConfig := getProxyConfig()
	t.Cleanup(func() {
		pool = oldPool
		setProxyConfig(oldConfig)
	})

	first := &Account{AccountID: "first", Status: "active"}
	second := &Account{AccountID: "second", Status: "active"}
	pool = &AccountPool{Accounts: []*Account{first, second}}

	for _, strategy := range []string{"fill", "round_robin", "random"} {
		t.Run(strategy, func(t *testing.T) {
			pool.CurrentIdx = 0
			cfg := defaultProxyConfig()
			cfg.Strategy = strategy
			setProxyConfig(cfg)

			firstPick := pickAccountForModelStrict(freeModelPrimary)
			if firstPick != first && firstPick != second {
				t.Fatalf("first pick = %v, want an active account", firstPick)
			}
			if strategy == "fill" && firstPick != first {
				t.Fatalf("fill first pick = %v, want first account", firstPick)
			}
			if strategy == "round_robin" {
				secondPick := pickAccountForModelStrict(freeModelPrimary)
				if firstPick != first || secondPick != second {
					t.Fatalf("round_robin picks = %v, %v, want first, second", firstPick, secondPick)
				}
			}
		})
	}
}

func TestHandleResponsesFreeReturnsTooManyRequestsWhenBothPoolsUnavailable(t *testing.T) {
	oldPool := pool
	oldConfig := getProxyConfig()
	oldTransport := httpClient.Transport
	requestLogsMu.Lock()
	oldLogs := requestLogs
	requestLogs = nil
	requestLogsMu.Unlock()
	t.Cleanup(func() {
		pool = oldPool
		setProxyConfig(oldConfig)
		httpClient.Transport = oldTransport
		requestLogsMu.Lock()
		requestLogs = oldLogs
		requestLogsMu.Unlock()
	})

	account := &Account{
		AccountID:   "quota-account",
		Email:       "quota@example.com",
		AccessToken: "quota-token",
		ExpiresAt:   time.Now().Add(time.Hour).UnixMilli(),
		Status:      "active",
	}
	pool = &AccountPool{Accounts: []*Account{account}}
	setProxyConfig(defaultProxyConfig())

	calls := 0
	httpClient.Transport = freeModelRoundTripper(func(req *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{
			StatusCode: http.StatusTooManyRequests,
			Body:       io.NopCloser(strings.NewReader(`{"error":"quota","message":"Try again in 1h"}`)),
			Header:     make(http.Header),
			Request:    req,
		}, nil
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"free","input":"hello"}`))
	recorder := httptest.NewRecorder()
	handleResponses(recorder, req)
	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("response status = %d, want %d: %s", recorder.Code, http.StatusTooManyRequests, recorder.Body.String())
	}
	if calls != len(freeModelChain) {
		t.Fatalf("upstream calls = %d, want one attempt per model (%d)", calls, len(freeModelChain))
	}

	requestLogsMu.Lock()
	defer requestLogsMu.Unlock()
	if len(requestLogs) != 1 {
		t.Fatalf("request log count = %d, want 1", len(requestLogs))
	}
	lastModel := freeModelChain[len(freeModelChain)-1]
	if requestLogs[0].Model != lastModel {
		t.Fatalf("request log model = %q, want %q", requestLogs[0].Model, lastModel)
	}
}

func TestCallClineAPIFreeDSExhaustsOnlyDSPool(t *testing.T) {
	oldPool := pool
	oldConfig := getProxyConfig()
	oldTransport := httpClient.Transport
	t.Cleanup(func() {
		pool = oldPool
		setProxyConfig(oldConfig)
		httpClient.Transport = oldTransport
	})

	first := &Account{
		AccountID:   "free-ds-one",
		Email:       "free-ds-one@example.com",
		AccessToken: "free-ds-one-token",
		ExpiresAt:   time.Now().Add(time.Hour).UnixMilli(),
		Status:      "active",
	}
	second := &Account{
		AccountID:   "free-ds-two",
		Email:       "free-ds-two@example.com",
		AccessToken: "free-ds-two-token",
		ExpiresAt:   time.Now().Add(time.Hour).UnixMilli(),
		Status:      "active",
	}
	pool = &AccountPool{Accounts: []*Account{first, second}}
	config := defaultProxyConfig()
	config.Strategy = "fill"
	setProxyConfig(config)

	var attempts []string
	var models []string
	httpClient.Transport = freeModelRoundTripper(func(req *http.Request) (*http.Response, error) {
		token := strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer ")
		attempts = append(attempts, token)
		body, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		var params map[string]any
		if err := json.Unmarshal(body, &params); err != nil {
			return nil, err
		}
		model, _ := params["model"].(string)
		models = append(models, model)
		return &http.Response{
			StatusCode: http.StatusTooManyRequests,
			Body:       io.NopCloser(strings.NewReader(`{"error":"quota","message":"Try again in 1h"}`)),
			Header:     make(http.Header),
			Request:    req,
		}, nil
	})

	params := map[string]any{
		"model":    "free-ds",
		"messages": []any{map[string]any{"role": "user", "content": "hello"}},
	}
	_, _, err := callClineAPI(params, false)
	if err == nil {
		t.Fatal("callClineAPI should fail when every DS account is cooling")
	}
	if got, want := strings.Join(attempts, ","), "free-ds-one-token,free-ds-two-token"; got != want {
		t.Fatalf("attempts = %q, want %q", got, want)
	}
	if got, want := strings.Join(models, ","), freeModelFallback+","+freeModelFallback; got != want {
		t.Fatalf("models = %q, want %q", got, want)
	}
	if got, want := params["model"], freeModelFallback; got != want {
		t.Fatalf("effective model = %v, want %q", got, want)
	}
}

func TestCallClineAPIFreeMuseExhaustsOnlyMusePool(t *testing.T) {
	oldPool := pool
	oldConfig := getProxyConfig()
	oldTransport := httpClient.Transport
	t.Cleanup(func() {
		pool = oldPool
		setProxyConfig(oldConfig)
		httpClient.Transport = oldTransport
	})

	first := &Account{
		AccountID:   "free-muse-one",
		Email:       "free-muse-one@example.com",
		AccessToken: "free-muse-one-token",
		ExpiresAt:   time.Now().Add(time.Hour).UnixMilli(),
		Status:      "active",
	}
	second := &Account{
		AccountID:   "free-muse-two",
		Email:       "free-muse-two@example.com",
		AccessToken: "free-muse-two-token",
		ExpiresAt:   time.Now().Add(time.Hour).UnixMilli(),
		Status:      "active",
	}
	pool = &AccountPool{Accounts: []*Account{first, second}}
	config := defaultProxyConfig()
	config.Strategy = "fill"
	setProxyConfig(config)

	var attempts []string
	var models []string
	httpClient.Transport = freeModelRoundTripper(func(req *http.Request) (*http.Response, error) {
		token := strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer ")
		attempts = append(attempts, token)
		body, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		var params map[string]any
		if err := json.Unmarshal(body, &params); err != nil {
			return nil, err
		}
		model, _ := params["model"].(string)
		models = append(models, model)
		return &http.Response{
			StatusCode: http.StatusTooManyRequests,
			Body:       io.NopCloser(strings.NewReader(`{"error":"quota","message":"Try again in 1h"}`)),
			Header:     make(http.Header),
			Request:    req,
		}, nil
	})

	params := map[string]any{
		"model":    freeModelMuseAlias,
		"messages": []any{map[string]any{"role": "user", "content": "hello"}},
	}
	_, _, err := callClineAPI(params, false)
	if err == nil {
		t.Fatal("callClineAPI should fail when every muse account is cooling")
	}
	if got, want := strings.Join(attempts, ","), "free-muse-one-token,free-muse-two-token"; got != want {
		t.Fatalf("attempts = %q, want %q", got, want)
	}
	if got, want := strings.Join(models, ","), freeModelMuse+","+freeModelMuse; got != want {
		t.Fatalf("models = %q, want %q", got, want)
	}
	if got, want := params["model"], freeModelMuse; got != want {
		t.Fatalf("effective model = %v, want %q", got, want)
	}
	if _, cooling := first.ModelCooldowns[freeModelMuse]; !cooling {
		t.Fatal("first account should be cooling down for the muse model")
	}
	if _, cooling := first.ModelCooldowns[freeModelPrimary]; cooling {
		t.Fatal("muse exhaustion must not cool down the GLM pool")
	}
}

func TestCallClineAPIFreeV41RetriesNextAccountAfterInsufficientCredits(t *testing.T) {
	oldPool := pool
	oldConfig := getProxyConfig()
	oldTransport := httpClient.Transport
	t.Cleanup(func() {
		pool = oldPool
		setProxyConfig(oldConfig)
		httpClient.Transport = oldTransport
	})

	first := &Account{
		AccountID:   "v41-one",
		Email:       "v41-one@example.com",
		AccessToken: "v41-one-token",
		ExpiresAt:   time.Now().Add(time.Hour).UnixMilli(),
		Status:      "active",
	}
	second := &Account{
		AccountID:   "v41-two",
		Email:       "v41-two@example.com",
		AccessToken: "v41-two-token",
		ExpiresAt:   time.Now().Add(time.Hour).UnixMilli(),
		Status:      "active",
	}
	pool = &AccountPool{Accounts: []*Account{first, second}}
	config := defaultProxyConfig()
	config.Strategy = "fill"
	setProxyConfig(config)

	const upstreamModel = freeModelV41
	var attempts []string
	var models []string
	httpClient.Transport = freeModelRoundTripper(func(req *http.Request) (*http.Response, error) {
		token := strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer ")
		attempts = append(attempts, token)
		body, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		var params map[string]any
		if err := json.Unmarshal(body, &params); err != nil {
			return nil, err
		}
		model, _ := params["model"].(string)
		models = append(models, model)
		if token == "v41-one-token" {
			return &http.Response{
				StatusCode: http.StatusPaymentRequired,
				Body:       io.NopCloser(strings.NewReader(`{"error":{"code":"insufficient_credits"},"balance":-0.02}`)),
				Header:     make(http.Header),
				Request:    req,
			}, nil
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(`{"id":"ok","choices":[]}`)),
			Header:     make(http.Header),
			Request:    req,
		}, nil
	})

	params := map[string]any{
		"model":    "free-v41",
		"messages": []any{map[string]any{"role": "user", "content": "hello"}},
	}
	resp, acc, err := callClineAPI(params, false)
	if err != nil {
		t.Fatalf("callClineAPI returned error: %v", err)
	}
	if resp == nil {
		t.Fatal("callClineAPI returned nil response")
	}
	resp.Body.Close()
	if acc != second {
		t.Fatalf("selected account = %v, want second V4.1 account", acc)
	}
	if got, want := strings.Join(attempts, ","), "v41-one-token,v41-two-token"; got != want {
		t.Fatalf("attempts = %q, want %q", got, want)
	}
	if got, want := strings.Join(models, ","), upstreamModel+","+upstreamModel; got != want {
		t.Fatalf("models = %q, want %q", got, want)
	}
	if got, want := params["model"], upstreamModel; got != want {
		t.Fatalf("effective model = %v, want %q", got, want)
	}
	if first.Status != "active" {
		t.Fatalf("first account status = %q, want active", first.Status)
	}
	if !modelCooldownActive(first, upstreamModel) {
		t.Fatal("first account should have a V4.1 model cooldown")
	}
	if time.Until(first.ModelCooldowns[upstreamModel]) < 23*time.Hour {
		t.Fatalf("V4.1 cooldown = %s, want at least 23h", time.Until(first.ModelCooldowns[upstreamModel]))
	}
	if modelCooldownActive(first, freeModelPrimary) {
		t.Fatal("first account should not have a GLM model cooldown")
	}
	if !first.CooldownUntil.IsZero() {
		t.Fatalf("account cooldown = %s, want zero", first.CooldownUntil)
	}
}

func TestCallClineAPIFreeV41DoesNotFailoverOnOtherPaymentRequired(t *testing.T) {
	oldPool := pool
	oldConfig := getProxyConfig()
	oldTransport := httpClient.Transport
	t.Cleanup(func() {
		pool = oldPool
		setProxyConfig(oldConfig)
		httpClient.Transport = oldTransport
	})

	first := &Account{
		AccountID:   "v41-payment-one",
		Email:       "v41-payment-one@example.com",
		AccessToken: "v41-payment-one-token",
		ExpiresAt:   time.Now().Add(time.Hour).UnixMilli(),
		Status:      "active",
	}
	second := &Account{
		AccountID:   "v41-payment-two",
		Email:       "v41-payment-two@example.com",
		AccessToken: "v41-payment-two-token",
		ExpiresAt:   time.Now().Add(time.Hour).UnixMilli(),
		Status:      "active",
	}
	pool = &AccountPool{Accounts: []*Account{first, second}}
	config := defaultProxyConfig()
	config.Strategy = "fill"
	setProxyConfig(config)

	calls := 0
	httpClient.Transport = freeModelRoundTripper(func(req *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{
			StatusCode: http.StatusPaymentRequired,
			Body:       io.NopCloser(strings.NewReader(`{"error":{"code":"payment_required"}}`)),
			Header:     make(http.Header),
			Request:    req,
		}, nil
	})

	params := map[string]any{
		"model":    "free-v41",
		"messages": []any{map[string]any{"role": "user", "content": "hello"}},
	}
	_, acc, err := callClineAPI(params, false)
	if err == nil {
		t.Fatal("callClineAPI should return the payment-required error")
	}
	apiErr, ok := err.(*clineAPIError)
	if !ok {
		t.Fatalf("error type = %T, want *clineAPIError", err)
	}
	if apiErr.statusCode != http.StatusPaymentRequired {
		t.Fatalf("error status = %d, want %d", apiErr.statusCode, http.StatusPaymentRequired)
	}
	if acc != first {
		t.Fatalf("selected account = %v, want first V4.1 account", acc)
	}
	if calls != 1 {
		t.Fatalf("upstream calls = %d, want 1", calls)
	}
	if first.Status != "active" {
		t.Fatalf("first account status = %q, want active", first.Status)
	}
	if modelCooldownActive(first, freeModelV41) {
		t.Fatal("other payment-required errors should not set a V4.1 model cooldown")
	}
	if !first.CooldownUntil.IsZero() {
		t.Fatalf("account cooldown = %s, want zero", first.CooldownUntil)
	}
}

func TestCallClineAPIFreeUsesConfiguredChainOnly(t *testing.T) {
	oldPool := pool
	oldConfig := getProxyConfig()
	oldTransport := httpClient.Transport
	t.Cleanup(func() {
		pool = oldPool
		setProxyConfig(oldConfig)
		httpClient.Transport = oldTransport
	})

	first := &Account{
		AccountID: "chain-one", Email: "chain-one@example.com", AccessToken: "chain-one-token",
		ExpiresAt: time.Now().Add(time.Hour).UnixMilli(), Status: "active",
	}
	second := &Account{
		AccountID: "chain-two", Email: "chain-two@example.com", AccessToken: "chain-two-token",
		ExpiresAt: time.Now().Add(time.Hour).UnixMilli(), Status: "active",
	}
	pool = &AccountPool{Accounts: []*Account{first, second}}
	config := defaultProxyConfig()
	config.ModelChain = []string{freeModelFallback}
	setProxyConfig(config)

	var attempts []string
	var models []string
	httpClient.Transport = freeModelRoundTripper(func(req *http.Request) (*http.Response, error) {
		attempts = append(attempts, strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer "))
		body, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		var params map[string]any
		if err := json.Unmarshal(body, &params); err != nil {
			return nil, err
		}
		models = append(models, params["model"].(string))
		return &http.Response{
			StatusCode: http.StatusTooManyRequests,
			Body:       io.NopCloser(strings.NewReader(`{"error":"quota"}`)),
			Header:     make(http.Header),
			Request:    req,
		}, nil
	})

	params := map[string]any{"model": "free"}
	_, _, err := callFreeClineAPI(params, false)
	if err == nil {
		t.Fatal("callFreeClineAPI should fail when the configured chain is exhausted")
	}
	if got, want := strings.Join(attempts, ","), "chain-one-token,chain-two-token"; got != want {
		t.Fatalf("attempts = %q, want %q", got, want)
	}
	if got, want := strings.Join(models, ","), freeModelFallback+","+freeModelFallback; got != want {
		t.Fatalf("models = %q, want %q", got, want)
	}
	if got := params["model"]; got != freeModelFallback {
		t.Fatalf("final model = %v, want %q", got, freeModelFallback)
	}
}

func TestCallClineAPIFreeFallsBackAfterServerError(t *testing.T) {
	oldPool := pool
	oldConfig := getProxyConfig()
	oldTransport := httpClient.Transport
	t.Cleanup(func() {
		pool = oldPool
		setProxyConfig(oldConfig)
		httpClient.Transport = oldTransport
	})

	account := &Account{
		AccountID: "server-error", Email: "server-error@example.com", AccessToken: "server-error-token",
		ExpiresAt: time.Now().Add(time.Hour).UnixMilli(), Status: "active",
	}
	pool = &AccountPool{Accounts: []*Account{account}}
	config := defaultProxyConfig()
	config.ModelChain = []string{freeModelPrimary, freeModelFallback}
	setProxyConfig(config)

	var models []string
	httpClient.Transport = freeModelRoundTripper(func(req *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		var params map[string]any
		if err := json.Unmarshal(body, &params); err != nil {
			return nil, err
		}
		model := params["model"].(string)
		models = append(models, model)
		if model == freeModelPrimary {
			return &http.Response{
				StatusCode: http.StatusInternalServerError,
				Body:       io.NopCloser(strings.NewReader(`{"error":"boom"}`)),
				Header:     make(http.Header),
				Request:    req,
			}, nil
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(`{"id":"ok","choices":[]}`)),
			Header:     make(http.Header),
			Request:    req,
		}, nil
	})

	resp, _, err := callFreeClineAPI(map[string]any{"model": "free"}, false)
	if err != nil {
		t.Fatalf("callFreeClineAPI returned error: %v", err)
	}
	resp.Body.Close()
	if got, want := strings.Join(models, ","), freeModelPrimary+","+freeModelFallback; got != want {
		t.Fatalf("models = %q, want %q", got, want)
	}
}

func TestBuildUpstreamBodyNormalizesMuseMaxEffort(t *testing.T) {
	for _, key := range []string{"reasoning_effort", "reasoningEffort"} {
		t.Run(key, func(t *testing.T) {
			body := buildUpstreamBody(map[string]any{
				"model": freeModelMuse,
				key:     "max",
			}, false)
			if got, want := body["reasoning_effort"], defaultReasoningEffort; got != want {
				t.Fatalf("reasoning_effort = %v, want %q", got, want)
			}
		})
	}
}

func TestBuildUpstreamBodyPreservesMaxEffortForOtherModels(t *testing.T) {
	body := buildUpstreamBody(map[string]any{
		"model":            freeModelPrimary,
		"reasoning_effort": "max",
	}, false)
	if got, want := body["reasoning_effort"], "max"; got != want {
		t.Fatalf("reasoning_effort = %v, want %q", got, want)
	}
}

func TestBuildUpstreamBodyAcceptsIntegerMaxTokens(t *testing.T) {
	t.Setenv("CLINE2API_MAX_TOKENS", "65536")
	body := buildUpstreamBody(map[string]any{
		"model":      freeModelPrimary,
		"max_tokens": 65536,
	}, false)
	if got, want := body["max_tokens"], 65536; got != want {
		t.Fatalf("max_tokens = %v, want %d", got, want)
	}
}

func TestBuildUpstreamBodyClampsGeminiOutputLimit(t *testing.T) {
	t.Setenv("CLINE2API_MAX_TOKENS", "128000")
	for _, maxTokens := range []any{float64(128000), 128000} {
		body := buildUpstreamBody(map[string]any{
			"model":      "cline-free/gemini-3.8-flash",
			"max_tokens": maxTokens,
		}, false)
		if got, want := body["max_tokens"], 65536; got != want {
			t.Fatalf("max_tokens = %v, want %d", got, want)
		}
	}
}

func TestBuildUpstreamBodyPreservesUnknownModelOutputLimit(t *testing.T) {
	t.Setenv("CLINE2API_MAX_TOKENS", "128000")
	body := buildUpstreamBody(map[string]any{
		"model":      "custom/unknown-model",
		"max_tokens": 128000,
	}, false)
	if got, want := body["max_tokens"], 128000; got != want {
		t.Fatalf("max_tokens = %v, want %d", got, want)
	}
}

func isolateRequestLogs(t *testing.T) {
	t.Helper()
	oldPath := requestLogsPath
	requestLogsMu.Lock()
	oldLogs := requestLogs
	requestLogs = nil
	requestLogsPath = t.TempDir() + "/request-logs.json"
	requestLogsMu.Unlock()
	t.Cleanup(func() {
		requestLogsMu.Lock()
		requestLogs = oldLogs
		requestLogsPath = oldPath
		requestLogsMu.Unlock()
	})
}

func TestHandleStreamResponseMarksSSEErrorIncomplete(t *testing.T) {
	isolateRequestLogs(t)
	recorder := httptest.NewRecorder()
	upstream := &http.Response{
		StatusCode: http.StatusOK,
		Body: io.NopCloser(strings.NewReader(
			"data: {\"error\":{\"code\":\"stream_initialization_failed\",\"message\":\"invalid effort\"}}\n\n",
		)),
	}
	reqLog := RequestLog{ID: "openai-sse-error", Model: freeModelMuse, StartedAt: time.Now()}

	handleStreamResponse(recorder, upstream, nil, &reqLog)

	if reqLog.Completed {
		t.Fatal("SSE error should not be marked completed")
	}
	if !strings.Contains(reqLog.Error, "stream_initialization_failed") {
		t.Fatalf("request log error = %q", reqLog.Error)
	}
	if !strings.Contains(recorder.Body.String(), "stream_initialization_failed") {
		t.Fatalf("response body = %q", recorder.Body.String())
	}
}

func TestHandleStreamResponseIgnoresNullErrorField(t *testing.T) {
	isolateRequestLogs(t)
	recorder := httptest.NewRecorder()
	upstream := &http.Response{
		StatusCode: http.StatusOK,
		Body: io.NopCloser(strings.NewReader(
			"data: {\"error\":null,\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n" +
				"data: {\"choices\":[{\"delta\":{\"content\":\" there\"}}]}\n\n",
		)),
	}
	reqLog := RequestLog{ID: "openai-null-error", Model: freeModelMuse, StartedAt: time.Now()}

	handleStreamResponse(recorder, upstream, nil, &reqLog)

	if !reqLog.Completed {
		t.Fatalf("normal stream should be completed: %q", reqLog.Error)
	}
	if !strings.Contains(recorder.Body.String(), " there") {
		t.Fatalf("response body = %q", recorder.Body.String())
	}
}

func TestHandleAnthropicStreamReturnsUncommittedOnSSEError(t *testing.T) {
	isolateRequestLogs(t)
	recorder := httptest.NewRecorder()
	upstream := &http.Response{
		StatusCode: http.StatusOK,
		Body: io.NopCloser(strings.NewReader(
			"data: {\"error\":{\"code\":\"stream_initialization_failed\",\"message\":\"invalid effort\"}}\n\n",
		)),
	}
	reqLog := RequestLog{ID: "anthropic-sse-error", Model: freeModelMuse, StartedAt: time.Now()}

	committed := handleAnthropicStream(context.Background(), newSSEWriter(recorder), upstream, nil, &reqLog, 5*time.Second)

	if committed {
		t.Fatal("SSE error before content should remain retryable")
	}
	if reqLog.Completed {
		t.Fatal("SSE error should not be marked completed")
	}
	if strings.Contains(recorder.Body.String(), "event: message_stop") {
		t.Fatalf("error stream should not emit message_stop: %q", recorder.Body.String())
	}
}

func TestHandleAnthropicStreamEmitsToolInputJSONDelta(t *testing.T) {
	isolateRequestLogs(t)
	recorder := httptest.NewRecorder()
	upstream := &http.Response{
		StatusCode: http.StatusOK,
		Body: io.NopCloser(strings.NewReader(
			"data: {\"model\":\"google/gemini-3.8-flash\",\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"function\":{\"name\":\"get_weather\",\"arguments\":\"{\\\"city\\\":\\\"\"}}]}}]}\n\n" +
				"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"Taipei\\\"}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\n" +
				"data: [DONE]\n\n",
		)),
	}
	reqLog := RequestLog{ID: "anthropic-tool-input", Model: freeModelPrimary, StartedAt: time.Now()}

	handleAnthropicStream(context.Background(), newSSEWriter(recorder), upstream, nil, &reqLog, 5*time.Second)

	body := recorder.Body.String()
	for _, want := range []string{
		`"model":"` + freeModelPrimary + `"`,
		`event: content_block_start`,
		`"type":"input_json_delta"`,
		`"partial_json":"{\"city\":\"Taipei\"}"`,
		`event: content_block_stop`,
		`"stop_reason":"tool_use"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("response body missing %q: %s", want, body)
		}
	}
}

func TestHandleAnthropicStreamEmitsEmptyToolInput(t *testing.T) {
	isolateRequestLogs(t)
	recorder := httptest.NewRecorder()
	upstream := &http.Response{
		StatusCode: http.StatusOK,
		Body: io.NopCloser(strings.NewReader(
			"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"function\":{\"name\":\"ping\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\n" +
				"data: [DONE]\n\n",
		)),
	}
	reqLog := RequestLog{ID: "anthropic-empty-tool-input", Model: freeModelPrimary, StartedAt: time.Now()}

	handleAnthropicStream(context.Background(), newSSEWriter(recorder), upstream, nil, &reqLog, 5*time.Second)

	body := recorder.Body.String()
	if !strings.Contains(body, `"partial_json":"{}"`) {
		t.Fatalf("response body missing empty tool input: %s", body)
	}
}

func TestCallClineAPIDirectModelsFallBackOnModelCooldown(t *testing.T) {
	oldPool := pool
	oldConfig := getProxyConfig()
	oldTransport := httpClient.Transport
	t.Cleanup(func() {
		pool = oldPool
		setProxyConfig(oldConfig)
		httpClient.Transport = oldTransport
	})

	for _, model := range []string{freeModelPrimary, freeModelFallback} {
		t.Run(model, func(t *testing.T) {
			account := &Account{
				AccountID:   "direct-account",
				Email:       "direct@example.com",
				AccessToken: "direct-token",
				ExpiresAt:   time.Now().Add(time.Hour).UnixMilli(),
				Status:      "active",
			}
			pool = &AccountPool{Accounts: []*Account{account}}
			setProxyConfig(defaultProxyConfig())

			var attempted []string
			httpClient.Transport = freeModelRoundTripper(func(req *http.Request) (*http.Response, error) {
				body, err := io.ReadAll(req.Body)
				if err != nil {
					return nil, err
				}
				var params map[string]any
				if err := json.Unmarshal(body, &params); err != nil {
					return nil, err
				}
				upstreamModel, _ := params["model"].(string)
				attempted = append(attempted, upstreamModel)
				if upstreamModel == model {
					return &http.Response{
						StatusCode: http.StatusTooManyRequests,
						Body:       io.NopCloser(strings.NewReader(`{"error":"quota"}`)),
						Header:     make(http.Header),
						Request:    req,
					}, nil
				}
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(strings.NewReader(`{"id":"ok","choices":[{"message":{"role":"assistant","content":"hi"}}]}`)),
					Header:     make(http.Header),
					Request:    req,
				}, nil
			})

			params := map[string]any{"model": model}
			resp, _, err := callClineAPI(params, false)
			if err != nil {
				t.Fatalf("expected fallback success, got %v", err)
			}
			defer resp.Body.Close()
			if len(attempted) < 2 {
				t.Fatalf("expected fallback attempts after cooling model, got %v", attempted)
			}
			if attempted[0] != model {
				t.Fatalf("first attempt = %q, want requested %q", attempted[0], model)
			}
			if attempted[len(attempted)-1] == model {
				t.Fatal("fallback should not retry the cooling model")
			}
			if servedModel, _ := params["model"].(string); servedModel != attempted[len(attempted)-1] {
				t.Fatalf("params model after fallback = %q, want last attempted %q", servedModel, attempted[len(attempted)-1])
			}
		})
	}
}

func TestCallClineAPIDirectModelsNoFallbackOnServerError(t *testing.T) {
	oldPool := pool
	oldConfig := getProxyConfig()
	oldTransport := httpClient.Transport
	t.Cleanup(func() {
		pool = oldPool
		setProxyConfig(oldConfig)
		httpClient.Transport = oldTransport
	})

	model := freeModelPrimary
	account := &Account{
		AccountID:   "direct-account",
		Email:       "direct@example.com",
		AccessToken: "direct-token",
		ExpiresAt:   time.Now().Add(time.Hour).UnixMilli(),
		Status:      "active",
	}
	pool = &AccountPool{Accounts: []*Account{account}}
	setProxyConfig(defaultProxyConfig())

	calls := 0
	httpClient.Transport = freeModelRoundTripper(func(req *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{
			StatusCode: http.StatusInternalServerError,
			Body:       io.NopCloser(strings.NewReader(`{"error":"boom"}`)),
			Header:     make(http.Header),
			Request:    req,
		}, nil
	})

	_, _, err := callClineAPI(map[string]any{"model": model}, false)
	if err == nil {
		t.Fatal("expected error after exhausting the fallback chain")
	}
	if calls != len(freeModelChain) {
		t.Fatalf("upstream calls = %d, want %d", calls, len(freeModelChain))
	}
}

func TestPickAccountForModelLeastUsedSpreadsUsage(t *testing.T) {
	oldPool := pool
	t.Cleanup(func() { pool = oldPool })

	heavy := &Account{
		AccountID:   "heavy",
		Email:       "heavy@example.com",
		AccessToken: "token-heavy",
		ExpiresAt:   time.Now().Add(time.Hour).UnixMilli(),
		Status:      "active",
		ModelStats: map[string]*ModelStat{
			freeModelPrimary: {ModelID: freeModelPrimary, UsageCount: 100},
		},
	}
	light := &Account{
		AccountID:   "light",
		Email:       "light@example.com",
		AccessToken: "token-light",
		ExpiresAt:   time.Now().Add(time.Hour).UnixMilli(),
		Status:      "active",
		ModelStats: map[string]*ModelStat{
			freeModelPrimary: {ModelID: freeModelPrimary, UsageCount: 2},
		},
	}
	pool = &AccountPool{Accounts: []*Account{heavy, light}}

	for i := 0; i < 5; i++ {
		acc := pickAccountForModelLeastUsed(freeModelPrimary)
		if acc == nil {
			t.Fatal("pickAccountForModelLeastUsed returned nil with eligible accounts")
		}
		if acc.AccountID != "light" {
			t.Fatalf("pick %d: got account %q, want light", i+1, acc.AccountID)
		}
	}
}

func TestIsFreeModelEntry(t *testing.T) {
	cases := []struct {
		m    Model
		want bool
	}{
		{Model{ID: freeModelPrimary, Cost: "free"}, true},
		{Model{ID: "cline-pass/glm-5.2", Cost: "pass"}, false},
		{Model{ID: "mimo-v2.6-flash-free", Cost: "pass", Source: "zen"}, true},
		{Model{ID: "claude-opus-5", Cost: "pass", Source: "zen"}, false},
		{Model{ID: "some-model", Cost: "pass", Source: "remote"}, false},
	}
	for _, c := range cases {
		if got := isFreeModelEntry(c.m); got != c.want {
			t.Errorf("isFreeModelEntry(%+v) = %v, want %v", c.m, got, c.want)
		}
	}
}

func TestCallClineAPIFreePicksLeastUsedAccount(t *testing.T) {
	oldPool := pool
	oldConfig := getProxyConfig()
	oldTransport := httpClient.Transport
	t.Cleanup(func() {
		pool = oldPool
		setProxyConfig(oldConfig)
		httpClient.Transport = oldTransport
	})

	first := &Account{
		AccountID:   "acc-first",
		Email:       "first@example.com",
		AccessToken: "token-first",
		ExpiresAt:   time.Now().Add(time.Hour).UnixMilli(),
		Status:      "active",
		ModelStats: map[string]*ModelStat{
			freeModelPrimary: {ModelID: freeModelPrimary, UsageCount: 50},
		},
	}
	second := &Account{
		AccountID:   "acc-second",
		Email:       "second@example.com",
		AccessToken: "token-second",
		ExpiresAt:   time.Now().Add(time.Hour).UnixMilli(),
		Status:      "active",
	}
	pool = &AccountPool{Accounts: []*Account{first, second}}
	setProxyConfig(defaultProxyConfig())

	var chosen []string
	httpClient.Transport = freeModelRoundTripper(func(req *http.Request) (*http.Response, error) {
		token := strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer ")
		chosen = append(chosen, token)
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(`{"id":"ok","choices":[]}`)),
			Header:     make(http.Header),
			Request:    req,
		}, nil
	})

	for i := 0; i < 3; i++ {
		resp, _, err := callClineAPI(map[string]any{"model": "free"}, false)
		if err != nil || resp == nil {
			t.Fatalf("call %d failed: %v", i+1, err)
		}
		resp.Body.Close()
		if len(chosen) == 0 || chosen[len(chosen)-1] != "token-second" {
			t.Fatalf("call %d served by %q, want token-second", i+1, chosen[len(chosen)-1])
		}
	}
}

func TestSortModelsByAvailabilityPrefersAvailableThenLeastUsed(t *testing.T) {
	oldPool := pool
	t.Cleanup(func() { pool = oldPool })

	acc := &Account{
		AccountID:      "acc-one",
		Email:          "one@example.com",
		AccessToken:    "token",
		ExpiresAt:      time.Now().Add(time.Hour).UnixMilli(),
		Status:         "active",
		ModelCooldowns: map[string]time.Time{},
		ModelStats: map[string]*ModelStat{
			freeModelPrimary:  {ModelID: freeModelPrimary, UsageCount: 90},
			freeModelFallback: {ModelID: freeModelFallback, UsageCount: 3},
		},
	}
	pool = &AccountPool{Accounts: []*Account{acc}}

	got := sortModelsByAvailability([]string{freeModelPrimary, freeModelFallback})
	if got[0] != freeModelFallback {
		t.Fatalf("first model = %q, want %q", got[0], freeModelFallback)
	}
	if len(got) != 2 {
		t.Fatalf("chain length = %d, want 2", len(got))
	}
}

// 信封提前发送架构下 message_start.model 取 reqLog.Model（fetch 成功时已从别名
// 更新为实际请求上游的模型），不再发空串 —— 客户端不丢失流式消息的模型身份。
// Anthropic 协议 message_start 每条消息只发一次，上游 payload 的 model 不会回写。
func TestHandleAnthropicStreamPreservesUpstreamModel(t *testing.T) {
	requestLogsMu.Lock()
	oldLogs := requestLogs
	requestLogs = nil
	requestLogsMu.Unlock()
	t.Cleanup(func() {
		requestLogsMu.Lock()
		requestLogs = oldLogs
		requestLogsMu.Unlock()
	})

	recorder := httptest.NewRecorder()
	upstream := &http.Response{
		StatusCode: http.StatusOK,
		Body: io.NopCloser(strings.NewReader(
			"data: {\"model\":\"google/gemini-3.8-flash\",\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\n" +
				"data: [DONE]\n\n",
		)),
	}
	// 模拟别名 cline-free/gemini-3.8-flash 在 fetch 时解析为实际上游模型后的 reqLog
	reqLog := RequestLog{ID: "anthropic-stream-model", Model: "google/gemini-3.8-flash", StartedAt: time.Now()}

	sw := newSSEWriter(recorder)
	handleAnthropicStream(context.Background(), sw, upstream, nil, &reqLog, 5*time.Second)

	body := recorder.Body.String()
	if !strings.Contains(body, `"model":"google/gemini-3.8-flash"`) {
		t.Fatalf("response body missing upstream model: %s", body)
	}
}

// message_start 只发一次、先于内容块，且携带请求的服务模型（reqLog.Model），
// 后续上游 payload 的 model 元数据不回写已发出的信封。
func TestHandleAnthropicStreamFallsBackOnFirstPayloadWithoutModel(t *testing.T) {
	requestLogsMu.Lock()
	oldLogs := requestLogs
	requestLogs = nil
	requestLogsMu.Unlock()
	t.Cleanup(func() {
		requestLogsMu.Lock()
		requestLogs = oldLogs
		requestLogsMu.Unlock()
	})

	recorder := httptest.NewRecorder()
	upstream := &http.Response{
		StatusCode: http.StatusOK,
		Body: io.NopCloser(strings.NewReader(
			"data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n" +
				"data: {\"model\":\"actual-model\",\"choices\":[{\"delta\":{\"content\":\"more\"},\"finish_reason\":\"stop\"}]}\n\n" +
				"data: [DONE]\n\n",
		)),
	}
	reqLog := RequestLog{ID: "anthropic-stream-fallback", Model: "requested-model", StartedAt: time.Now()}

	sw := newSSEWriter(recorder)
	handleAnthropicStream(context.Background(), sw, upstream, nil, &reqLog, 5*time.Second)

	body := recorder.Body.String()
	if strings.Count(body, "event: message_start") != 1 {
		t.Fatalf("message_start count = %d, want 1: %s", strings.Count(body, "event: message_start"), body)
	}
	if !strings.Contains(body, `"model":"requested-model"`) {
		t.Fatalf("response body missing requested-model fallback: %s", body)
	}
	if start, content := strings.Index(body, "event: message_start"), strings.Index(body, "event: content_block_start"); start < 0 || content < 0 || start > content {
		t.Fatalf("message_start must precede content blocks: %s", body)
	}
}

// 上游首条 payload 迟迟不来时，响应头（含信封 + ping）必须先于等待刷出去。
func TestHandleAnthropicStreamFlushesHeadersBeforeFirstPayload(t *testing.T) {
	requestLogsMu.Lock()
	oldLogs := requestLogs
	requestLogs = nil
	requestLogsMu.Unlock()
	t.Cleanup(func() {
		requestLogsMu.Lock()
		requestLogs = oldLogs
		requestLogsMu.Unlock()
	})

	reader, writer := io.Pipe()
	recorder := newFlushSignalRecorder()
	upstream := &http.Response{StatusCode: http.StatusOK, Body: reader}
	reqLog := RequestLog{ID: "anthropic-slow-first-payload", Model: "requested-model", StartedAt: time.Now()}
	done := make(chan struct{})
	go func() {
		sw := newSSEWriter(recorder)
		handleAnthropicStream(context.Background(), sw, upstream, nil, &reqLog, 5*time.Second)
		close(done)
	}()

	select {
	case <-recorder.flushed:
	case <-time.After(250 * time.Millisecond):
		writer.Close()
		<-done
		t.Fatal("response headers were not flushed before the first upstream payload")
	}

	if _, err := writer.Write([]byte("data: {\"model\":\"actual-model\",\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\n")); err != nil {
		t.Fatalf("write upstream payload: %v", err)
	}
	writer.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("stream handler did not finish after upstream closed")
	}
}

func TestOnlyFreeConfigRoundTrip(t *testing.T) {
	oldConfig := getProxyConfig()
	t.Cleanup(func() { setProxyConfig(oldConfig) })

	setProxyConfig(defaultProxyConfig())
	if getProxyConfig().OnlyFree {
		t.Fatal("default OnlyFree = true, want false")
	}

	cfg := getProxyConfig()
	cfg.OnlyFree = true
	setProxyConfig(cfg)
	if !getProxyConfig().OnlyFree {
		t.Fatal("OnlyFree not persisted after setProxyConfig")
	}
}
