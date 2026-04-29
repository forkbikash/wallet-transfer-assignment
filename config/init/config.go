// Package config exposes LoadConfig, which reads service configuration
// from environment variables and constructs ready-to-use infrastructure clients.
package config

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"gorm.io/gorm"

	"github.com/Robustrade/wallet-transfer-assignment/config/infra"
)

// CommonConfig holds the cross-cutting service settings.
type CommonConfig struct {
	Port            string
	ShutdownTimeout time.Duration
}

// ServiceInstances aggregates the runtime instances passed to service initializers.
type ServiceInstances struct {
	Common CommonConfig
	Logger *slog.Logger
	DB     *gorm.DB
}

// LoadConfig reads environment variables, opens a database connection, and
// returns the wired ServiceInstances. The returned instance carries a Close
// method for graceful shutdown of the DB pool.
func LoadConfig(ctx context.Context) (*ServiceInstances, error) {
	common := CommonConfig{
		Port:            envOrDefault("PORT", "8080"),
		ShutdownTimeout: envDurationOrDefault("SHUTDOWN_TIMEOUT", 10*time.Second),
	}

	logger := infra.InitLogger(infra.LoggerConfig{
		Level: os.Getenv("LOG_LEVEL"),
		JSON:  envBool("LOG_JSON", true),
	})

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return nil, fmt.Errorf("config: DATABASE_URL is required")
	}

	db, err := infra.InitPostgres(ctx, infra.PostgresConfig{
		DSN:             dsn,
		MaxOpenConns:    envIntOrDefault("DB_MAX_OPEN_CONNS", 10),
		MaxIdleConns:    envIntOrDefault("DB_MAX_IDLE_CONNS", 5),
		ConnMaxLifetime: envDurationOrDefault("DB_CONN_MAX_LIFETIME", 30*time.Minute),
		PingTimeout:     envDurationOrDefault("DB_PING_TIMEOUT", 5*time.Second),
	})
	if err != nil {
		return nil, err
	}

	return &ServiceInstances{
		Common: common,
		Logger: logger,
		DB:     db,
	}, nil
}

// Close releases resources held by ServiceInstances.
func (s *ServiceInstances) Close() error {
	if s == nil {
		return nil
	}
	return infra.ClosePostgres(s.DB)
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); strings.TrimSpace(v) != "" {
		return v
	}
	return def
}

func envIntOrDefault(key string, def int) int {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	out, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return out
}

func envDurationOrDefault(key string, def time.Duration) time.Duration {
	v := os.Getenv(key)
	if strings.TrimSpace(v) == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return def
	}
	return d
}

func envBool(key string, def bool) bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	switch v {
	case "":
		return def
	case "1", "true", "yes", "y", "on":
		return true
	case "0", "false", "no", "n", "off":
		return false
	default:
		return def
	}
}
