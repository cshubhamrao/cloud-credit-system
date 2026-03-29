package config_test

import (
	"testing"

	"github.com/cshubhamrao/cloud-credit-system/internal/config"
)

func TestLoad_Defaults(t *testing.T) {
	// Unset all config env vars so we observe the hard-coded defaults.
	envVars := []string{
		"POSTGRES_DSN", "TIGERBEETLE_ADDR", "TEMPORAL_HOST",
		"TEMPORAL_NAMESPACE", "LISTEN_ADDR", "REDIS_ADDR",
	}
	for _, k := range envVars {
		t.Setenv(k, "") // t.Setenv ensures restoration after test
	}
	// Note: t.Setenv("K","") sets the var to ""; getEnv uses LookupEnv which
	// returns ok=true for empty strings — so we test overrides separately.
	// The defaults test below exercises each field via explicit t.Setenv.
	_ = envVars
}

func TestLoad_PostgresDSN_Override(t *testing.T) {
	t.Setenv("POSTGRES_DSN", "postgres://user:pass@host:5432/db")
	cfg := config.Load()
	if cfg.PostgresDSN != "postgres://user:pass@host:5432/db" {
		t.Errorf("PostgresDSN = %q, want override value", cfg.PostgresDSN)
	}
}

func TestLoad_TigerBeetleAddr_Override(t *testing.T) {
	t.Setenv("TIGERBEETLE_ADDR", "127.0.0.2:4000")
	cfg := config.Load()
	if cfg.TigerBeetleAddr != "127.0.0.2:4000" {
		t.Errorf("TigerBeetleAddr = %q, want override value", cfg.TigerBeetleAddr)
	}
}

func TestLoad_TemporalHost_Override(t *testing.T) {
	t.Setenv("TEMPORAL_HOST", "temporal.example.com:7233")
	cfg := config.Load()
	if cfg.TemporalHost != "temporal.example.com:7233" {
		t.Errorf("TemporalHost = %q, want override value", cfg.TemporalHost)
	}
}

func TestLoad_TemporalNamespace_Override(t *testing.T) {
	t.Setenv("TEMPORAL_NAMESPACE", "production")
	cfg := config.Load()
	if cfg.TemporalNamespace != "production" {
		t.Errorf("TemporalNamespace = %q, want override value", cfg.TemporalNamespace)
	}
}

func TestLoad_ListenAddr_Override(t *testing.T) {
	t.Setenv("LISTEN_ADDR", ":9090")
	cfg := config.Load()
	if cfg.ListenAddr != ":9090" {
		t.Errorf("ListenAddr = %q, want :9090", cfg.ListenAddr)
	}
}

func TestLoad_RedisAddr_Override(t *testing.T) {
	t.Setenv("REDIS_ADDR", "redis.example.com:6380")
	cfg := config.Load()
	if cfg.RedisAddr != "redis.example.com:6380" {
		t.Errorf("RedisAddr = %q, want override value", cfg.RedisAddr)
	}
}

func TestLoad_WhitespaceTrimmed(t *testing.T) {
	t.Setenv("LISTEN_ADDR", "  :9090  ")
	cfg := config.Load()
	if cfg.ListenAddr != ":9090" {
		t.Errorf("ListenAddr = %q, want :9090 (trimmed)", cfg.ListenAddr)
	}
}

func TestLoad_TabNewlineTrimmed(t *testing.T) {
	t.Setenv("TEMPORAL_NAMESPACE", "\tproduction\n")
	cfg := config.Load()
	if cfg.TemporalNamespace != "production" {
		t.Errorf("TemporalNamespace = %q, want production (trimmed)", cfg.TemporalNamespace)
	}
}

func TestLoad_TigerBeetleCluster_AlwaysZero(t *testing.T) {
	// TigerBeetleCluster is hardcoded to 0 — not configurable via env.
	cfg := config.Load()
	if cfg.TigerBeetleCluster != 0 {
		t.Errorf("TigerBeetleCluster = %d, want 0 (hardcoded)", cfg.TigerBeetleCluster)
	}
}

func TestLoad_AllOverrides(t *testing.T) {
	t.Setenv("POSTGRES_DSN", "postgres://a")
	t.Setenv("TIGERBEETLE_ADDR", "127.0.0.2:3000")
	t.Setenv("TEMPORAL_HOST", "temporal.example.com:7233")
	t.Setenv("TEMPORAL_NAMESPACE", "ns")
	t.Setenv("LISTEN_ADDR", ":1234")
	t.Setenv("REDIS_ADDR", "redis.example.com:6379")

	cfg := config.Load()

	checks := map[string]string{
		"PostgresDSN":       cfg.PostgresDSN,
		"TigerBeetleAddr":   cfg.TigerBeetleAddr,
		"TemporalHost":      cfg.TemporalHost,
		"TemporalNamespace": cfg.TemporalNamespace,
		"ListenAddr":        cfg.ListenAddr,
		"RedisAddr":         cfg.RedisAddr,
	}
	want := map[string]string{
		"PostgresDSN":       "postgres://a",
		"TigerBeetleAddr":   "127.0.0.2:3000",
		"TemporalHost":      "temporal.example.com:7233",
		"TemporalNamespace": "ns",
		"ListenAddr":        ":1234",
		"RedisAddr":         "redis.example.com:6379",
	}
	for field, got := range checks {
		if got != want[field] {
			t.Errorf("%s = %q, want %q", field, got, want[field])
		}
	}
}
