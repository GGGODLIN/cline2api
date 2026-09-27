package main

import "testing"

func TestBaseModelID(t *testing.T) {
	for input, want := range map[string]string{
		"cline-free/gemini-3.8-flash": "gemini-3.8-flash",
		"cline-pass/gemini-3.8-flash": "gemini-3.8-flash",
		"google/gemini-3.8-flash":     "gemini-3.8-flash",
		"custom/model":                "custom/model",
	} {
		if got := baseModelID(input); got != want {
			t.Errorf("baseModelID(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestSyncedRemoteModelUsesKnownMetadataFallback(t *testing.T) {
	model := syncedRemoteModel(clineRemoteModel{
		ID: "cline-free/gemini-3.8-flash",
	}, true)
	if model.Context != 1048576 || model.Output != 65536 {
		t.Fatalf("model metadata = context %d output %d", model.Context, model.Output)
	}
	if model.Cost != "free" || model.Source != "remote" {
		t.Fatalf("model classification = cost %q source %q", model.Cost, model.Source)
	}
}

func TestSyncedRemoteModelPrefersUpstreamMetadata(t *testing.T) {
	model := syncedRemoteModel(clineRemoteModel{
		ID:         "cline-free/gemini-3.8-flash",
		ContextWin: 200000,
		MaxTokens:  32000,
	}, true)
	if model.Context != 200000 || model.Output != 32000 {
		t.Fatalf("model metadata = context %d output %d", model.Context, model.Output)
	}
}

func TestModelMaxOutputLimitUsesKnownFallback(t *testing.T) {
	oldPool := pool
	pool = &AccountPool{}
	t.Cleanup(func() { pool = oldPool })

	if got, want := modelMaxOutputLimit("cline-free/gemini-3.8-flash"), 65536; got != want {
		t.Fatalf("modelMaxOutputLimit = %d, want %d", got, want)
	}
}

func TestModelMaxOutputLimitPrefersPoolMetadata(t *testing.T) {
	oldPool := pool
	pool = &AccountPool{Models: []Model{{
		ID:     "cline-free/gemini-3.8-flash",
		Output: 32768,
	}}}
	t.Cleanup(func() { pool = oldPool })

	if got, want := modelMaxOutputLimit("cline-free/gemini-3.8-flash"), 32768; got != want {
		t.Fatalf("modelMaxOutputLimit = %d, want %d", got, want)
	}
}

func TestModelMaxOutputLimitIgnoresValueBelowUpstreamFloor(t *testing.T) {
	oldPool := pool
	pool = &AccountPool{Models: []Model{{
		ID:     "custom/model",
		Output: 8,
	}}}
	t.Cleanup(func() { pool = oldPool })

	if got := modelMaxOutputLimit("custom/model"); got != 0 {
		t.Fatalf("modelMaxOutputLimit = %d, want no limit", got)
	}
}
