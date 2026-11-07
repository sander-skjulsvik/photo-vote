// Package config reads the app's settings from environment variables.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

const minSecretBytes = 32

// Config holds every setting.
type Config struct {
	ListenAddr      string
	BaseURL         string
	DBPath          string
	OIDCIssuer      string
	OIDCClientID    string
	OIDCSecret      string
	VoterGroup      string
	OwnerGroup      string
	VoterHashSecret []byte
	MaxItemsPerPoll int
	MaxStorageBytes int64
}

// Load reads settings using getenv, and reports every problem at once.
func Load(getenv func(string) string) (Config, error) {
	c := Config{
		ListenAddr:      withDefault(getenv("LISTEN_ADDR"), ":8080"),
		BaseURL:         strings.TrimRight(getenv("BASE_URL"), "/"),
		DBPath:          withDefault(getenv("DB_PATH"), "/data/photo-vote.db"),
		OIDCIssuer:      getenv("OIDC_ISSUER"),
		OIDCClientID:    getenv("OIDC_CLIENT_ID"),
		OIDCSecret:      getenv("OIDC_CLIENT_SECRET"),
		VoterGroup:      getenv("VOTER_GROUP"),
		OwnerGroup:      getenv("OWNER_GROUP"),
		VoterHashSecret: []byte(getenv("VOTER_HASH_SECRET")),
		MaxItemsPerPoll: 50,
		MaxStorageBytes: 1 << 30,
	}

	var problems []error
	require := func(name, value string) {
		if value == "" {
			problems = append(problems, fmt.Errorf("%s is required", name))
		}
	}
	require("BASE_URL", c.BaseURL)
	require("OIDC_ISSUER", c.OIDCIssuer)
	require("OIDC_CLIENT_ID", c.OIDCClientID)
	require("OIDC_CLIENT_SECRET", c.OIDCSecret)
	require("VOTER_GROUP", c.VoterGroup)
	require("OWNER_GROUP", c.OwnerGroup)

	if c.VoterGroup != "" && c.VoterGroup == c.OwnerGroup {
		problems = append(problems, errors.New("VOTER_GROUP and OWNER_GROUP must be different, or every voter would be an owner"))
	}
	if len(c.VoterHashSecret) < minSecretBytes {
		problems = append(problems, fmt.Errorf("VOTER_HASH_SECRET must be at least %d characters", minSecretBytes))
	}
	if c.BaseURL != "" {
		if err := checkURL(c.BaseURL); err != nil {
			problems = append(problems, fmt.Errorf("BASE_URL: %w", err))
		}
	}
	if c.OIDCIssuer != "" {
		if err := checkURL(c.OIDCIssuer); err != nil {
			problems = append(problems, fmt.Errorf("OIDC_ISSUER: %w", err))
		}
	}
	if v := getenv("MAX_ITEMS_PER_POLL"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			problems = append(problems, errors.New("MAX_ITEMS_PER_POLL must be a positive number"))
		}
		c.MaxItemsPerPoll = n
	}
	if v := getenv("MAX_STORAGE_MB"); v != "" {
		n, err := strconv.ParseInt(v, 10, 32)
		if err != nil || n < 1 {
			problems = append(problems, errors.New("MAX_STORAGE_MB must be a positive number"))
		}
		c.MaxStorageBytes = n << 20
	}
	return c, errors.Join(problems...)
}

// checkURL requires HTTPS, except on the local machine for development, so
// cookies and login traffic can't be set up to go unencrypted by accident.
func checkURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return errors.New("must be a full URL")
	}
	switch {
	case u.Scheme == "https":
		return nil
	case u.Scheme == "http" && (u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1"):
		return nil
	}
	return errors.New("must use https")
}

func withDefault(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}
