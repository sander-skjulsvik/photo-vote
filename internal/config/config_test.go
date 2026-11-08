package config

import (
	"strings"
	"testing"
)

func valid() map[string]string {
	return map[string]string{
		"BASE_URL":           "https://vote.example.com/",
		"OIDC_ISSUER":        "https://auth.example.com/application/o/photo-vote/",
		"OIDC_CLIENT_ID":     "photo-vote",
		"OIDC_CLIENT_SECRET": "fake",
		"VOTER_GROUP":        "voters",
		"OWNER_GROUP":        "owners",
		"VOTER_HASH_SECRET":  strings.Repeat("x", 32),
	}
}

func load(env map[string]string) (Config, error) {
	return Load(func(k string) string { return env[k] })
}

func TestValid(t *testing.T) {
	c, err := load(valid())
	if err != nil {
		t.Fatal(err)
	}
	if c.BaseURL != "https://vote.example.com" {
		t.Errorf("BaseURL = %q, want no trailing slash", c.BaseURL)
	}
	if c.OIDCIssuer != "https://auth.example.com/application/o/photo-vote/" {
		t.Errorf("issuer changed to %q; it has to match the provider exactly", c.OIDCIssuer)
	}
}

func TestRejected(t *testing.T) {
	tests := []struct {
		name, key, value, want string
	}{
		{"missing secret", "OIDC_CLIENT_SECRET", "", "OIDC_CLIENT_SECRET"},
		{"short hash secret", "VOTER_HASH_SECRET", "short", "VOTER_HASH_SECRET"},
		{"plain http", "BASE_URL", "http://vote.example.com", "https"},
		{"plain http issuer", "OIDC_ISSUER", "http://auth.example.com", "https"},
		{"same group", "OWNER_GROUP", "voters", "different"},
		{"bad limit", "MAX_STORAGE_MB", "lots", "MAX_STORAGE_MB"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := valid()
			env[tt.key] = tt.value
			_, err := load(env)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want it to mention %q", err, tt.want)
			}
		})
	}
}

func TestLocalHTTPAllowedForDevelopment(t *testing.T) {
	env := valid()
	env["BASE_URL"] = "http://localhost:8080"
	if _, err := load(env); err != nil {
		t.Error(err)
	}
}
