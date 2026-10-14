package main

import "encoding/json"

// Access tokens belong in the private storage copy, not the account-list API.
func marshalPoolStorage(p *AccountPool) ([]byte, error) {
	data, err := json.Marshal(p)
	if err != nil {
		return nil, err
	}
	var stored map[string]any
	if err := json.Unmarshal(data, &stored); err != nil {
		return nil, err
	}
	rows, _ := stored["accounts"].([]any)
	for index, account := range p.Accounts {
		row := rows[index].(map[string]any)
		row["accessToken"] = account.AccessToken
		row["expiresAt"] = account.ExpiresAt
	}
	return json.MarshalIndent(stored, "", "  ")
}

func restoreStoredCredentials(data []byte, p *AccountPool) error {
	var stored struct {
		Accounts []struct {
			AccountID   string `json:"accountId"`
			AccessToken string `json:"accessToken"`
			ExpiresAt   int64  `json:"expiresAt"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(data, &stored); err != nil {
		return err
	}
	byID := make(map[string]int, len(p.Accounts))
	for index, account := range p.Accounts {
		byID[account.AccountID] = index
	}
	for _, row := range stored.Accounts {
		if index, exists := byID[row.AccountID]; exists {
			p.Accounts[index].AccessToken = row.AccessToken
			p.Accounts[index].ExpiresAt = row.ExpiresAt
		}
	}
	return nil
}
