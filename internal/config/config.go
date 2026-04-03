package config

import (
	"os"
	"strings"
)

type Config struct {
	PostgresDSN        string
	TigerBeetleAddr    string
	TigerBeetleCluster uint32
	TemporalHost       string
	TemporalNamespace  string
	TemporalAPIKey     string
	ListenAddr         string
	RedisAddr          string
}

func Load() Config {
	return Config{
		PostgresDSN:        getEnv("POSTGRES_DSN", "postgres://postgres:postgres@localhost:5432/creditdb?sslmode=disable"),
		TigerBeetleAddr:    getEnv("TIGERBEETLE_ADDR", "127.0.0.1:3000"),
		TigerBeetleCluster: 0,
		TemporalHost:       getEnv("TEMPORAL_HOST", "localhost:7233"),
		TemporalNamespace:  getEnv("TEMPORAL_NAMESPACE", "default"),
		TemporalAPIKey:     getEnv("TEMPORAL_API_KEY", ""),
		ListenAddr:         getEnv("LISTEN_ADDR", ":8080"),
		RedisAddr:          getEnv("REDIS_ADDR", "localhost:6379"),
	}
}

func getEnv(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok {
		return strings.TrimSpace(v)
	}
	return fallback
}
