// Package health exposes a service-agnostic /healthz handler.
//
// The handler pings the supplied database pool with a short timeout. A nil db
// produces a liveness-only handler that always reports "ok".
package health

import (
	"context"
	"net/http"
	"time"

	"gorm.io/gorm"

	"github.com/Robustrade/wallet-transfer-assignment/common/middleware"
)

// PingTimeout caps the per-request DB liveness check.
const PingTimeout = 1 * time.Second

// ReadinessTimeout caps the combined readiness checks (DB + Kafka, etc.).
const ReadinessTimeout = 2 * time.Second

// Check is a named readiness probe for a dependency.
type Check struct {
	Name  string
	Probe func(ctx context.Context) error
}

// DBCheck pings the database pool.
func DBCheck(db *gorm.DB) Check {
	return Check{Name: "postgres", Probe: func(ctx context.Context) error {
		sqlDB, err := db.DB()
		if err != nil {
			return err
		}
		return sqlDB.PingContext(ctx)
	}}
}

// Readiness returns a GET /readyz handler that runs every check; any failure is
// 503 (so a load balancer stops routing). Use this to gate dependency health,
// distinct from liveness (Handler).
func Readiness(checks ...Check) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), ReadinessTimeout)
		defer cancel()
		for _, c := range checks {
			if err := c.Probe(ctx); err != nil {
				middleware.WriteJSON(w, http.StatusServiceUnavailable, map[string]string{
					"status": "not ready",
					"check":  c.Name,
				})
				return
			}
		}
		middleware.WriteJSON(w, http.StatusOK, map[string]string{"status": "ready"})
	}
}

// Handler returns an http.HandlerFunc serving GET /healthz.
//
// When db is non-nil, the handler issues a Ping against the underlying
// *sql.DB pool with a short timeout; a failure surfaces as 503
// ServiceUnavailable. When db is nil, the handler is liveness-only and
// always returns 200.
func Handler(db *gorm.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if db != nil {
			ctx, cancel := context.WithTimeout(r.Context(), PingTimeout)
			defer cancel()
			sqlDB, err := db.DB()
			if err == nil {
				err = sqlDB.PingContext(ctx)
			}
			if err != nil {
				middleware.WriteJSON(w, http.StatusServiceUnavailable, map[string]string{
					"status": "down",
					"reason": "database not reachable",
				})
				return
			}
		}
		middleware.WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	}
}
