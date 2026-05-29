package config

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"gorm.io/gorm"

	"github.com/Robustrade/wallet-transfer-assignment/config/infra"
)

// This file adds the configuration the event-sourced wallet needs on top of the
// original LoadConfig: Kafka settings, and reusable building blocks so each
// run-mode can wire only what it uses (e.g. the command-processor needs Kafka
// but not Postgres).

// KafkaConfig holds the broker list and topic/runtime settings.
type KafkaConfig struct {
	Brokers            []string
	Partitions         int           // partition (shard) count when creating topics
	ReplicationFactor  int           // 1 for local dev; 3 in production
	GatewayWaitTimeout time.Duration // push-model deadline before the gateway returns 202
}

// LoadKafka reads Kafka configuration from the environment.
func LoadKafka() (KafkaConfig, error) {
	partitions, err := envIntOrDefault("KAFKA_PARTITIONS", 12)
	if err != nil {
		return KafkaConfig{}, err
	}
	replication, err := envIntOrDefault("KAFKA_REPLICATION_FACTOR", 1)
	if err != nil {
		return KafkaConfig{}, err
	}
	waitTimeout, err := envDurationOrDefault("GATEWAY_WAIT_TIMEOUT", 5*time.Second)
	if err != nil {
		return KafkaConfig{}, err
	}
	brokers := splitCSV(envOrDefault("KAFKA_BROKERS", "localhost:9092"))

	// Fail fast on misconfiguration rather than at first Kafka use.
	if len(brokers) == 0 {
		return KafkaConfig{}, fmt.Errorf("config: KAFKA_BROKERS must list at least one broker")
	}
	if partitions <= 0 {
		return KafkaConfig{}, fmt.Errorf("config: KAFKA_PARTITIONS must be > 0")
	}
	if replication <= 0 {
		return KafkaConfig{}, fmt.Errorf("config: KAFKA_REPLICATION_FACTOR must be > 0")
	}

	return KafkaConfig{
		Brokers:            brokers,
		Partitions:         partitions,
		ReplicationFactor:  replication,
		GatewayWaitTimeout: waitTimeout,
	}, nil
}

// NewLogger builds the process logger from LOG_LEVEL / LOG_JSON.
func NewLogger() (*slog.Logger, error) {
	logJSON, err := envBool("LOG_JSON", true)
	if err != nil {
		return nil, err
	}
	return infra.InitLogger(infra.LoggerConfig{Level: os.Getenv("LOG_LEVEL"), JSON: logJSON}), nil
}

// OpenPostgres opens the Postgres pool from DATABASE_URL with the same tuning
// knobs as LoadConfig. Used by the DB-backed run-modes (gateway, saga,
// projector); the command-processor does not call this.
func OpenPostgres(ctx context.Context) (*gorm.DB, error) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return nil, fmt.Errorf("config: DATABASE_URL is required")
	}
	maxOpen, err := envIntOrDefault("DB_MAX_OPEN_CONNS", 10)
	if err != nil {
		return nil, err
	}
	maxIdle, err := envIntOrDefault("DB_MAX_IDLE_CONNS", 5)
	if err != nil {
		return nil, err
	}
	connMaxLifetime, err := envDurationOrDefault("DB_CONN_MAX_LIFETIME", 30*time.Minute)
	if err != nil {
		return nil, err
	}
	pingTimeout, err := envDurationOrDefault("DB_PING_TIMEOUT", 5*time.Second)
	if err != nil {
		return nil, err
	}
	return infra.InitPostgres(ctx, infra.PostgresConfig{
		DSN:             dsn,
		MaxOpenConns:    maxOpen,
		MaxIdleConns:    maxIdle,
		ConnMaxLifetime: connMaxLifetime,
		PingTimeout:     pingTimeout,
	})
}

// Port returns the HTTP port for the gateway.
func Port() string { return envOrDefault("PORT", "8080") }

// ShutdownTimeoutOrDefault returns the graceful-shutdown timeout.
func ShutdownTimeoutOrDefault() (time.Duration, error) {
	return envDurationOrDefault("SHUTDOWN_TIMEOUT", 10*time.Second)
}

func splitCSV(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
