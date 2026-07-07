package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
)

type Config struct {
	// HTTP listen for admin GUI + reseller API + Kamailio control plane
	ListenAddr string

	// Postgres DSN, e.g. postgres://didstorage:pw@localhost:5432/didstorage?sslmode=disable
	DatabaseURL string

	// Redis URL, e.g. redis://localhost:6379/0
	RedisURL string

	// Shared secret Kamailio sends as X-DIDS-Auth on every /sipctl/* call.
	// (POPs use mTLS; the local Kamailio uses this token for simplicity.)
	KamailioAuthToken string

	// Public IP this central node listens on for SIP — used in Via/Contact rewrites.
	PublicIP string

	// Minimum seconds of balance required to authorize a new INVITE.
	// Below this we deny outright; otherwise we cap max_seconds at balance/rate.
	MinAuthorizeSeconds int

	// Default channel cap when assigning a DID to a user (locked rule: 2).
	DefaultChannelCount int
}

func Load() (Config, error) {
	c := Config{
		ListenAddr:          getenv("LISTEN_ADDR", ":8080"),
		DatabaseURL:         os.Getenv("DATABASE_URL"),
		RedisURL:            getenv("REDIS_URL", "redis://localhost:6379/0"),
		KamailioAuthToken:   os.Getenv("KAMAILIO_AUTH_TOKEN"),
		PublicIP:            os.Getenv("PUBLIC_IP"),
		MinAuthorizeSeconds: getenvInt("MIN_AUTH_SECONDS", 6),
		DefaultChannelCount: 2,
	}
	if c.DatabaseURL == "" {
		return c, errors.New("DATABASE_URL is required")
	}
	if c.KamailioAuthToken == "" {
		return c, errors.New("KAMAILIO_AUTH_TOKEN is required")
	}
	if c.PublicIP == "" {
		return c, errors.New("PUBLIC_IP is required")
	}
	return c, nil
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func getenvInt(k string, def int) int {
	v := os.Getenv(k)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config: invalid int for %s=%q, using default %d\n", k, v, def)
		return def
	}
	return n
}
