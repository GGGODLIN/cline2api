package main

import (
	"encoding/json"
	"os"
	"testing"
)

func TestHandoffPrivateTokensSurviveRestartWithoutPublicLeak(t *testing.T) {
	isolateHandoff(t)
	pool.Accounts = []*Account{{AccountID: "a1", AccessToken: "workos:private-access", RefreshToken: "private-refresh", ExpiresAt: 4102444800000, Status: "active"}}
	savePool()
	raw, err := os.ReadFile(poolPath)
	if err != nil {
		t.Fatal(err)
	}
	var stored map[string]any
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatal(err)
	}
	row := stored["accounts"].([]any)[0].(map[string]any)
	if row["accessToken"] != "workos:private-access" {
		t.Fatal("private storage dropped access token")
	}
	pool = nil
	acc := loadPool().Accounts[0]
	if acc.AccessToken != "workos:private-access" || acc.ExpiresAt != 4102444800000 {
		t.Fatal("restart lost credentials")
	}
	public, err := json.Marshal(acc)
	if err != nil {
		t.Fatal(err)
	}
	var view map[string]any
	if err := json.Unmarshal(public, &view); err != nil {
		t.Fatal(err)
	}
	if _, exists := view["accessToken"]; exists {
		t.Fatal("public account includes access token")
	}
	if _, exists := view["expiresAt"]; exists {
		t.Fatal("public account includes token expiry")
	}
}
