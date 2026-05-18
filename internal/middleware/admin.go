package middleware

import (
	"context"
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/zentra/server/internal/utils"
)

type contextKeyAdmin string

const IsAdminKey contextKeyAdmin = "isAdmin"

// AdminMiddleware checks that the authenticated user has admin privileges.
// Must be used after AuthMiddleware.
func AdminMiddleware(db *pgxpool.Pool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			userID, ok := GetUserID(r.Context())
			if !ok {
				utils.RespondErrorWithCode(w, http.StatusUnauthorized, "UNAUTHORIZED", "Authentication required")
				return
			}

			var isAdmin bool
			err := db.QueryRow(r.Context(),
				`SELECT is_admin FROM users WHERE id = $1 AND deleted_at IS NULL`,
				userID,
			).Scan(&isAdmin)
			if err != nil || !isAdmin {
				utils.RespondErrorWithCode(w, http.StatusForbidden, "FORBIDDEN", "Admin access required")
				return
			}

			ctx := context.WithValue(r.Context(), IsAdminKey, true)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// IsAdmin checks if the context has an admin flag
func IsAdmin(ctx context.Context) bool {
	isAdmin, ok := ctx.Value(IsAdminKey).(bool)
	return ok && isAdmin
}
