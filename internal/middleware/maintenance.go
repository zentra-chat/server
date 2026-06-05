package middleware

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"github.com/rs/zerolog/log"
	"github.com/zentra/server/internal/utils"
)

var maintenancePaths = []string{
	"/api/v1/auth/login",
	"/api/v1/auth/register",
	"/api/v1/auth/refresh",
}

type maintenanceData struct {
	Enabled bool   `json:"enabled"`
	Message string `json:"message"`
}

func MaintenanceMiddleware(rdb *redis.Client, db *pgxpool.Pool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/health" || r.URL.Path == "/ws" {
				next.ServeHTTP(w, r)
				return
			}

			for _, p := range maintenancePaths {
				if strings.HasPrefix(r.URL.Path, p) {
					next.ServeHTTP(w, r)
					return
				}
			}

			maintenance := getMaintenanceMode(r.Context(), rdb)
			if maintenance == nil || !maintenance.Enabled {
				next.ServeHTTP(w, r)
				return
			}

			if userID, ok := GetUserID(r.Context()); ok {
				var isAdmin bool
				err := db.QueryRow(r.Context(),
					`SELECT is_admin FROM users WHERE id = $1 AND deleted_at IS NULL`,
					userID,
				).Scan(&isAdmin)
				if err == nil && isAdmin {
					next.ServeHTTP(w, r)
					return
				}
			}

			message := maintenance.Message
			if message == "" {
				message = "This server is currently under maintenance. Please check back later."
			}

			utils.RespondJSON(w, http.StatusServiceUnavailable, utils.ErrorResponse{
				Error: message,
				Code:  "MAINTENANCE_MODE",
			})
		})
	}
}

func getMaintenanceMode(ctx context.Context, rdb *redis.Client) *maintenanceData {
	data, err := rdb.Get(ctx, "zentra:maintenance").Bytes()
	if err != nil {
		if err != redis.Nil {
			log.Warn().Err(err).Msg("Failed to get maintenance mode from Redis")
		}
		return nil
	}

	var m maintenanceData
	if err := json.Unmarshal(data, &m); err != nil {
		log.Warn().Err(err).Msg("Failed to unmarshal maintenance data")
		return nil
	}

	return &m
}
