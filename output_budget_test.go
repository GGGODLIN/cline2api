package main

import "testing"

func withBudgetPool(t *testing.T, models []Model) {
	t.Helper()
	oldPool := pool
	t.Cleanup(func() { pool = oldPool })
	pool = &AccountPool{Models: models}
}

// 未设 CLINE2API_MAX_TOKENS 时，默认 32000 只补省略的输出预算。
// 合法显式值仍要留给模型硬上限判断，否则未知 ceiling 的免费模型被预先夹低。
func TestBuildUpstreamBodyKeepsExplicitBudgetWithoutEnvCap(t *testing.T) {
	t.Setenv("CLINE2API_MAX_TOKENS", "")

	cases := []struct {
		name   string
		params map[string]any
	}{
		{"max_tokens", map[string]any{"model": "custom/unknown-model", "max_tokens": 128000}},
		{"max_completion_tokens", map[string]any{"model": "custom/unknown-model", "max_completion_tokens": 128000}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := buildUpstreamBody(tc.params, false)
			if got, want := body["max_tokens"], 128000; got != want {
				t.Fatalf("max_tokens = %v, want %d", got, want)
			}
		})
	}
}

func TestBuildUpstreamBodyHonorsExplicitEnvCap(t *testing.T) {
	t.Setenv("CLINE2API_MAX_TOKENS", "32000")
	body := buildUpstreamBody(map[string]any{
		"model":      "custom/unknown-model",
		"max_tokens": 128000,
	}, false)
	if got, want := body["max_tokens"], 32000; got != want {
		t.Fatalf("max_tokens = %v, want %d", got, want)
	}
}

func TestBuildUpstreamBodyCapsExplicitGeminiBudget(t *testing.T) {
	t.Setenv("CLINE2API_MAX_TOKENS", "")
	body := buildUpstreamBody(map[string]any{
		"model":      "google/gemini-3.8-flash",
		"max_tokens": 128000,
	}, false)
	if got, want := body["max_tokens"], 65536; got != want {
		t.Fatalf("max_tokens = %v, want %d", got, want)
	}
}

func TestBuildUpstreamBodyKeepsOmittedBudgetFallback(t *testing.T) {
	t.Setenv("CLINE2API_MAX_TOKENS", "")
	cases := []struct {
		name   string
		params map[string]any
	}{
		{"absent", map[string]any{"model": "m1"}},
		{"zero", map[string]any{"model": "m1", "max_tokens": 0}},
		{"below-minimum", map[string]any{"model": "m1", "max_tokens": 8}},
		{"minimum", map[string]any{"model": "m1", "max_tokens": minUpstreamMaxTokens}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := buildUpstreamBody(tc.params, false)
			want := defaultMaxTokens
			if tc.name == "minimum" {
				want = minUpstreamMaxTokens
			}
			if got := body["max_tokens"]; got != want {
				t.Fatalf("max_tokens = %v, want %d", got, want)
			}
		})
	}
}

func TestBuildUpstreamBodyRaisesLockedFreeOutput(t *testing.T) {
	t.Setenv("CLINE2API_MAX_TOKENS", "")
	verified := Model{ID: freeModelMimo26, Provider: "cline-free", Cost: "free", Output: 131072, MetaLocked: true}
	withBudgetPool(t, []Model{
		verified,
		{ID: freeModelMuse, Provider: "cline-free", Cost: "free", Output: 65536, MetaLocked: false},
		{ID: "cline-free/unknown-output", Provider: "cline-free", Cost: "free", MetaLocked: true},
		{ID: "cline-pass/glm-5.2", Provider: "zai", Cost: "pass", Output: 131072, MetaLocked: true},
	})

	cases := []struct {
		name   string
		params map[string]any
		want   int
	}{
		{"small", map[string]any{"model": verified.ID, "max_tokens": 4096}, verified.Output},
		{"omitted", map[string]any{"model": verified.ID}, verified.Output},
		{"completion", map[string]any{"model": verified.ID, "max_completion_tokens": 4096}, verified.Output},
		{"unlocked", map[string]any{"model": freeModelMuse, "max_tokens": 4096}, 4096},
		{"unknown-output", map[string]any{"model": "cline-free/unknown-output", "max_tokens": 4096}, 4096},
		{"paid", map[string]any{"model": "cline-pass/glm-5.2", "max_tokens": 4096}, 4096},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := buildUpstreamBody(tc.params, false)
			if got := body["max_tokens"]; got != tc.want {
				t.Fatalf("max_tokens = %v, want %d", got, tc.want)
			}
		})
	}
}

func TestBuildUpstreamBodyDiagnosticKeepsCallerBudget(t *testing.T) {
	t.Setenv("CLINE2API_MAX_TOKENS", "")
	withBudgetPool(t, []Model{{
		ID: freeModelMimo26, Provider: "cline-free", Cost: "free", Output: 131072, MetaLocked: true,
	}})
	body := buildUpstreamBody(map[string]any{
		"model":      freeModelMimo26,
		"max_tokens": 64,
		"metadata": map[string]any{
			"budget_mode": "diagnostic",
			"trace":       "keep",
		},
	}, false)
	if got := body["max_tokens"]; got != 64 {
		t.Fatalf("max_tokens = %v, want 64", got)
	}
	meta, _ := body["metadata"].(map[string]any)
	if _, ok := meta["budget_mode"]; ok || meta["trace"] != "keep" {
		t.Fatalf("metadata = %#v, want only trace", meta)
	}
}

func TestBuildUpstreamBodyLockedRaiseObeysCaps(t *testing.T) {
	verified := Model{ID: freeModelMimo26, Provider: "cline-free", Cost: "free", Output: 131072, MetaLocked: true}
	withBudgetPool(t, []Model{verified})

	t.Run("env", func(t *testing.T) {
		t.Setenv("CLINE2API_MAX_TOKENS", "32000")
		body := buildUpstreamBody(map[string]any{"model": verified.ID, "max_tokens": 4096}, false)
		if got := body["max_tokens"]; got != 32000 {
			t.Fatalf("max_tokens = %v, want 32000", got)
		}
	})
	t.Run("known-model", func(t *testing.T) {
		t.Setenv("CLINE2API_MAX_TOKENS", "")
		withBudgetPool(t, []Model{{
			ID: "google/gemini-3.8-flash", Provider: "google", Cost: "free", Output: 131072, MetaLocked: true,
		}})
		body := buildUpstreamBody(map[string]any{"model": "google/gemini-3.8-flash", "max_tokens": 4096}, false)
		if got := body["max_tokens"]; got != 65536 {
			t.Fatalf("max_tokens = %v, want 65536", got)
		}
	})
}

func TestBuildUpstreamBodyDiagnosticSurvivesRebuild(t *testing.T) {
	t.Setenv("CLINE2API_MAX_TOKENS", "")
	withBudgetPool(t, []Model{{
		ID: freeModelMimo26, Provider: "cline-free", Cost: "free", Output: 131072, MetaLocked: true,
	}})
	params := map[string]any{
		"model":      freeModelMimo26,
		"max_tokens": 64,
		"metadata": map[string]any{
			"budget_mode": "diagnostic",
			"trace":       "keep",
		},
	}
	for range 2 {
		body := buildUpstreamBody(params, false)
		if got := body["max_tokens"]; got != 64 {
			t.Fatalf("max_tokens = %v, want 64", got)
		}
		meta, _ := body["metadata"].(map[string]any)
		if _, ok := meta["budget_mode"]; ok || meta["trace"] != "keep" {
			t.Fatalf("body metadata = %#v, want only trace", meta)
		}
	}
	meta := params["metadata"].(map[string]any)
	if meta["budget_mode"] != "diagnostic" || meta["trace"] != "keep" {
		t.Fatalf("caller metadata = %#v", meta)
	}
}
