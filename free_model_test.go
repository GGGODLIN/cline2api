package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type freeModelRoundTripper func(*http.Request) (*http.Response, error)

func (f freeModelRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
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
	if first.Status != "expired" {
		t.Fatalf("first account status = %q, want expired", first.Status)
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
		if token == "token-one" && model == "z-ai/glm-5.3-flash" || token == "token-two" {
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
	if got, want := strings.Join(models, ","), "z-ai/glm-5.3-flash,z-ai/glm-5.3-flash,deepseek/deepseek-v4-flash"; got != want {
		t.Fatalf("models = %q, want %q", got, want)
	}
	if got, want := params["model"], "deepseek/deepseek-v4-flash"; got != want {
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

func TestCallClineAPIDirectModelsKeepExactIDWithoutFallback(t *testing.T) {
	oldPool := pool
	oldConfig := getProxyConfig()
	oldTransport := httpClient.Transport
	t.Cleanup(func() {
		pool = oldPool
		setProxyConfig(oldConfig)
		httpClient.Transport = oldTransport
	})

	for _, model := range []string{"z-ai/glm-5.3-flash", "deepseek/deepseek-v4-flash"} {
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

			calls := 0
			var upstreamModel string
			httpClient.Transport = freeModelRoundTripper(func(req *http.Request) (*http.Response, error) {
				calls++
				body, err := io.ReadAll(req.Body)
				if err != nil {
					return nil, err
				}
				var params map[string]any
				if err := json.Unmarshal(body, &params); err != nil {
					return nil, err
				}
				upstreamModel, _ = params["model"].(string)
				return &http.Response{
					StatusCode: http.StatusTooManyRequests,
					Body:       io.NopCloser(strings.NewReader(`{"error":"quota"}`)),
					Header:     make(http.Header),
					Request:    req,
				}, nil
			})

			params := map[string]any{"model": model}
			_, _, err := callClineAPI(params, false)
			if err == nil {
				t.Fatal("direct model request should return upstream quota error")
			}
			if calls != 1 {
				t.Fatalf("upstream calls = %d, want 1", calls)
			}
			if upstreamModel != model {
				t.Fatalf("upstream model = %q, want %q", upstreamModel, model)
			}
			if params["model"] != model {
				t.Fatalf("request model changed to %v", params["model"])
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

func TestCallClineAPIFreeUsesOnlyGLMThenDS(t *testing.T) {
	oldPool := pool
	oldConfig := getProxyConfig()
	oldTransport := httpClient.Transport
	t.Cleanup(func() {
		pool = oldPool
		setProxyConfig(oldConfig)
		httpClient.Transport = oldTransport
	})

	first := &Account{
		AccountID:   "free-chain-one",
		Email:       "free-chain-one@example.com",
		AccessToken: "free-chain-one-token",
		ExpiresAt:   time.Now().Add(time.Hour).UnixMilli(),
		Status:      "active",
	}
	second := &Account{
		AccountID:   "free-chain-two",
		Email:       "free-chain-two@example.com",
		AccessToken: "free-chain-two-token",
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
		"model":    "free",
		"messages": []any{map[string]any{"role": "user", "content": "hello"}},
	}
	_, _, err := callClineAPI(params, false)
	if err == nil {
		t.Fatal("callClineAPI should fail when both free pools are cooling")
	}
	if got, want := strings.Join(attempts, ","), "free-chain-one-token,free-chain-two-token,free-chain-one-token,free-chain-two-token"; got != want {
		t.Fatalf("attempts = %q, want %q", got, want)
	}
	if got, want := strings.Join(models, ","), freeModelPrimary+","+freeModelPrimary+","+freeModelFallback+","+freeModelFallback; got != want {
		t.Fatalf("models = %q, want %q", got, want)
	}
	if got, want := params["model"], freeModelFallback; got != want {
		t.Fatalf("effective model = %v, want %q", got, want)
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

func TestCallClineAPIFreeStopsOnNonQuotaAPIError(t *testing.T) {
	oldPool := pool
	oldConfig := getProxyConfig()
	oldTransport := httpClient.Transport
	t.Cleanup(func() {
		pool = oldPool
		setProxyConfig(oldConfig)
		httpClient.Transport = oldTransport
	})

	account := &Account{
		AccountID:   "non-quota-error",
		Email:       "non-quota-error@example.com",
		AccessToken: "non-quota-error-token",
		ExpiresAt:   time.Now().Add(time.Hour).UnixMilli(),
		Status:      "active",
	}
	pool = &AccountPool{Accounts: []*Account{account}}
	setProxyConfig(defaultProxyConfig())

	calls := 0
	httpClient.Transport = freeModelRoundTripper(func(req *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{
			StatusCode: http.StatusBadGateway,
			Body:       io.NopCloser(strings.NewReader(`{"error":"upstream failure"}`)),
			Header:     make(http.Header),
			Request:    req,
		}, nil
	})

	params := map[string]any{
		"model":    "free",
		"messages": []any{map[string]any{"role": "user", "content": "hello"}},
	}
	_, _, err := callClineAPI(params, false)
	if err == nil {
		t.Fatal("callClineAPI should return the non-quota API error")
	}
	apiErr, ok := err.(*clineAPIError)
	if !ok {
		t.Fatalf("error type = %T, want *clineAPIError", err)
	}
	if apiErr.statusCode != http.StatusBadGateway {
		t.Fatalf("error status = %d, want %d", apiErr.statusCode, http.StatusBadGateway)
	}
	if calls != 1 {
		t.Fatalf("upstream calls = %d, want 1", calls)
	}
	if got, want := params["model"], freeModelPrimary; got != want {
		t.Fatalf("effective model = %v, want %q", got, want)
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

func TestHandleAnthropicStreamMarksSSEErrorIncomplete(t *testing.T) {
	isolateRequestLogs(t)
	recorder := httptest.NewRecorder()
	upstream := &http.Response{
		StatusCode: http.StatusOK,
		Body: io.NopCloser(strings.NewReader(
			"data: {\"error\":{\"code\":\"stream_initialization_failed\",\"message\":\"invalid effort\"}}\n\n",
		)),
	}
	reqLog := RequestLog{ID: "anthropic-sse-error", Model: freeModelMuse, StartedAt: time.Now()}

	handleAnthropicStream(recorder, upstream, nil, &reqLog)

	if reqLog.Completed {
		t.Fatal("SSE error should not be marked completed")
	}
	if !strings.Contains(reqLog.Error, "stream_initialization_failed") {
		t.Fatalf("request log error = %q", reqLog.Error)
	}
	body := recorder.Body.String()
	if !strings.Contains(body, "event: error") {
		t.Fatalf("response body = %q", body)
	}
	if strings.Contains(body, "event: message_stop") {
		t.Fatalf("error stream should not emit message_stop: %q", body)
	}
}
