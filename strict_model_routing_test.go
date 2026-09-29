package main

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestExplicitModelStaysStrictAcrossProtocols(t *testing.T) {
	const requestedModel = "cline-free/requested-model"
	const fallbackModel = "cline-free/fallback-model"

	for _, tc := range []struct {
		name string
		path string
		body string
	}{
		{
			name: "OpenAI chat completions",
			path: "/v1/chat/completions",
			body: `{"model":"cline-free/requested-model","messages":[{"role":"user","content":"hello"}]}`,
		},
		{
			name: "OpenAI Responses",
			path: "/v1/responses",
			body: `{"model":"cline-free/requested-model","input":"hello"}`,
		},
		{
			name: "Anthropic Messages",
			path: "/v1/messages",
			body: `{"model":"cline-free/requested-model","max_tokens":32,"messages":[{"role":"user","content":"hello"}]}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			oldPool := pool
			oldConfig := getProxyConfig()
			oldTransport := httpClient.Transport
			t.Cleanup(func() {
				pool = oldPool
				setProxyConfig(oldConfig)
				httpClient.Transport = oldTransport
			})

			pool = &AccountPool{Accounts: []*Account{{
				AccountID: "strict-protocol", Email: "strict@example.com", AccessToken: "strict-token",
				ExpiresAt: time.Now().Add(time.Hour).UnixMilli(), Status: "active",
			}}}
			config := defaultProxyConfig()
			config.ModelChain = []string{fallbackModel}
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
				model, _ := params["model"].(string)
				models = append(models, model)
				if model == requestedModel {
					return &http.Response{
						StatusCode: http.StatusTooManyRequests,
						Body:       io.NopCloser(strings.NewReader(`{"error":"quota","message":"Try again in 1h"}`)),
						Header:     make(http.Header),
						Request:    req,
					}, nil
				}
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(strings.NewReader(`{"id":"unexpected-fallback","model":"cline-free/fallback-model","choices":[{"message":{"role":"assistant","content":"unexpected fallback"},"finish_reason":"stop"}]}`)),
					Header:     make(http.Header),
					Request:    req,
				}, nil
			})

			baseURL := protocolTestServer(t)
			req, err := http.NewRequest(http.MethodPost, baseURL+tc.path, strings.NewReader(tc.body))
			if err != nil {
				t.Fatalf("create request: %v", err)
			}
			resp, err := (&http.Client{Transport: &http.Transport{}, Timeout: 2 * time.Second}).Do(req)
			if err != nil {
				t.Fatalf("send request: %v", err)
			}
			defer resp.Body.Close()
			responseBody, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("read response: %v", err)
			}
			if resp.StatusCode != http.StatusTooManyRequests {
				t.Fatalf("response status = %d, want %d: %s", resp.StatusCode, http.StatusTooManyRequests, responseBody)
			}
			if got, want := strings.Join(models, ","), requestedModel; got != want {
				t.Fatalf("upstream models = %q, want %q", got, want)
			}
		})
	}
}

func TestAnthropicExplicitStreamRetriesNextAccountOnRequestedModel(t *testing.T) {
	const requestedModel = "cline-free/requested-model"
	const fallbackModel = "cline-free/fallback-model"

	oldPool := pool
	oldConfig := getProxyConfig()
	oldTransport := httpClient.Transport
	t.Cleanup(func() {
		pool = oldPool
		setProxyConfig(oldConfig)
		httpClient.Transport = oldTransport
	})
	t.Setenv("CLINE2API_FIRST_CONTENT_TIMEOUT_MS", "1")
	t.Setenv("CLINE2API_LAST_ATTEMPT_TIMEOUT_MS", "1")

	pool = &AccountPool{Accounts: []*Account{
		{AccountID: "strict-stream-one", Email: "one@example.com", AccessToken: "token-one", ExpiresAt: time.Now().Add(time.Hour).UnixMilli(), Status: "active"},
		{AccountID: "strict-stream-two", Email: "two@example.com", AccessToken: "token-two", ExpiresAt: time.Now().Add(time.Hour).UnixMilli(), Status: "active"},
	}}
	config := defaultProxyConfig()
	config.ModelChain = []string{fallbackModel}
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
		model, _ := params["model"].(string)
		models = append(models, model)
		token := strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer ")
		streamBody := "data: [DONE]\n\n"
		if model == requestedModel && token == "token-two" {
			streamBody = "data: {\"model\":\"" + model + "\",\"choices\":[{\"delta\":{\"content\":\"same model success\"}}]}\n\ndata: [DONE]\n\n"
		} else if model != requestedModel {
			streamBody = "data: {\"model\":\"" + model + "\",\"choices\":[{\"delta\":{\"content\":\"unexpected fallback\"}}]}\n\ndata: [DONE]\n\n"
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(streamBody)),
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Request:    req,
		}, nil
	})

	baseURL := protocolTestServer(t)
	req, err := http.NewRequest(http.MethodPost, baseURL+"/v1/messages", strings.NewReader(`{"model":"cline-free/requested-model","stream":true,"max_tokens":32,"messages":[{"role":"user","content":"hello"}]}`))
	if err != nil {
		t.Fatalf("create request: %v", err)
	}
	resp, err := (&http.Client{Transport: &http.Transport{}, Timeout: 2 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("send request: %v", err)
	}
	defer resp.Body.Close()
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if strings.Contains(string(responseBody), "unexpected fallback") {
		t.Fatalf("explicit stream crossed models: %s", responseBody)
	}
	if !strings.Contains(string(responseBody), "same model success") {
		t.Fatalf("response body = %q, want same-model account retry success", responseBody)
	}
	if got, want := strings.Join(models, ","), requestedModel+","+requestedModel; got != want {
		t.Fatalf("upstream models = %q, want %q", got, want)
	}
}

func TestAnthropicExplicitStreamDoesNotRetryAnotherModel(t *testing.T) {
	const requestedModel = "cline-free/requested-model"
	const fallbackModel = "cline-free/fallback-model"

	oldPool := pool
	oldConfig := getProxyConfig()
	oldTransport := httpClient.Transport
	t.Cleanup(func() {
		pool = oldPool
		setProxyConfig(oldConfig)
		httpClient.Transport = oldTransport
	})
	t.Setenv("CLINE2API_FIRST_CONTENT_TIMEOUT_MS", "1")
	t.Setenv("CLINE2API_LAST_ATTEMPT_TIMEOUT_MS", "1")

	pool = &AccountPool{Accounts: []*Account{{
		AccountID: "strict-stream", Email: "strict@example.com", AccessToken: "strict-token",
		ExpiresAt: time.Now().Add(time.Hour).UnixMilli(), Status: "active",
	}}}
	config := defaultProxyConfig()
	config.ModelChain = []string{fallbackModel}
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
		model, _ := params["model"].(string)
		models = append(models, model)
		streamBody := "data: [DONE]\n\n"
		if model != requestedModel {
			streamBody = "data: {\"model\":\"" + model + "\",\"choices\":[{\"delta\":{\"content\":\"unexpected fallback\"}}]}\n\ndata: [DONE]\n\n"
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(streamBody)),
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Request:    req,
		}, nil
	})

	baseURL := protocolTestServer(t)
	req, err := http.NewRequest(http.MethodPost, baseURL+"/v1/messages", strings.NewReader(`{"model":"cline-free/requested-model","stream":true,"max_tokens":32,"messages":[{"role":"user","content":"hello"}]}`))
	if err != nil {
		t.Fatalf("create request: %v", err)
	}
	resp, err := (&http.Client{Transport: &http.Transport{}, Timeout: 2 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("send request: %v", err)
	}
	defer resp.Body.Close()
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if strings.Contains(string(responseBody), "unexpected fallback") {
		t.Fatalf("explicit stream crossed models: %s", responseBody)
	}
	if !strings.Contains(string(responseBody), "event: error") {
		t.Fatalf("response body = %q, want terminal error event", responseBody)
	}
	if got, want := strings.Join(models, ","), requestedModel; got != want {
		t.Fatalf("upstream models = %q, want %q", got, want)
	}
}

func TestAnthropicFreeStreamKeepsModelChainRetry(t *testing.T) {
	oldPool := pool
	oldConfig := getProxyConfig()
	oldTransport := httpClient.Transport
	t.Cleanup(func() {
		pool = oldPool
		setProxyConfig(oldConfig)
		httpClient.Transport = oldTransport
	})
	t.Setenv("CLINE2API_FIRST_CONTENT_TIMEOUT_MS", "1")
	t.Setenv("CLINE2API_LAST_ATTEMPT_TIMEOUT_MS", "1")

	pool = &AccountPool{Accounts: []*Account{{
		AccountID: "free-stream", Email: "free@example.com", AccessToken: "free-token",
		ExpiresAt: time.Now().Add(time.Hour).UnixMilli(), Status: "active",
	}}}
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
		model, _ := params["model"].(string)
		models = append(models, model)
		streamBody := "data: [DONE]\n\n"
		if model == freeModelFallback {
			streamBody = "data: {\"model\":\"" + model + "\",\"choices\":[{\"delta\":{\"content\":\"free fallback\"}}]}\n\ndata: [DONE]\n\n"
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(streamBody)),
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Request:    req,
		}, nil
	})

	baseURL := protocolTestServer(t)
	req, err := http.NewRequest(http.MethodPost, baseURL+"/v1/messages", strings.NewReader(`{"model":"free","stream":true,"max_tokens":32,"messages":[{"role":"user","content":"hello"}]}`))
	if err != nil {
		t.Fatalf("create request: %v", err)
	}
	resp, err := (&http.Client{Transport: &http.Transport{}, Timeout: 2 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("send request: %v", err)
	}
	defer resp.Body.Close()
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if !strings.Contains(string(responseBody), "free fallback") {
		t.Fatalf("response body = %q, want free-chain fallback content", responseBody)
	}
	if got, want := strings.Join(models, ","), freeModelPrimary+","+freeModelFallback; got != want {
		t.Fatalf("upstream models = %q, want %q", got, want)
	}
}
