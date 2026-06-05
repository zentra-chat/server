package admin

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/zentra/server/internal/middleware"
	"github.com/zentra/server/internal/utils"
)

type Handler struct {
	service *Service
}

func NewHandler(service *Service) *Handler {
	return &Handler{service: service}
}

func (h *Handler) Routes() chi.Router {
	r := chi.NewRouter()

	r.Get("/dashboard", h.GetDashboard)
	r.Get("/analytics", h.GetAnalytics)
	r.Get("/admins", h.ListAdmins)
	r.Post("/admins", h.AddAdmin)
	r.Delete("/admins/{userId}", h.RemoveAdmin)

	r.Get("/users", h.ListUsers)
	r.Get("/users/{userId}", h.GetUser)
	r.Patch("/users/{userId}", h.UpdateUser)
	r.Delete("/users/{userId}", h.DeleteUser)
	r.Post("/users/{userId}/restore", h.RestoreUser)

	r.Route("/server", func(r chi.Router) {
		r.Get("/info", h.GetServerInfo)
		r.Get("/config", h.GetServerConfig)
		r.Patch("/config", h.UpdateServerConfig)
	})

	return r
}

func (h *Handler) GetDashboard(w http.ResponseWriter, r *http.Request) {
	stats, err := h.service.GetDashboard(r.Context())
	if err != nil {
		utils.RespondError(w, http.StatusInternalServerError, "Failed to fetch dashboard stats")
		return
	}

	utils.RespondSuccess(w, stats)
}

func (h *Handler) GetAnalytics(w http.ResponseWriter, r *http.Request) {
	stats, err := h.service.GetAnalytics(r.Context())
	if err != nil {
		utils.RespondError(w, http.StatusInternalServerError, "Failed to fetch analytics stats")
		return
	}

	utils.RespondSuccess(w, stats)
}

func (h *Handler) ListAdmins(w http.ResponseWriter, r *http.Request) {
	admins, err := h.service.GetAdmins(r.Context())
	if err != nil {
		utils.RespondError(w, http.StatusInternalServerError, "Failed to fetch admins")
		return
	}

	utils.RespondSuccess(w, admins)
}

func (h *Handler) AddAdmin(w http.ResponseWriter, r *http.Request) {
	userID, err := middleware.RequireAuth(r.Context())
	if err != nil {
		utils.RespondError(w, http.StatusUnauthorized, "Unauthorized")
		return
	}

	var req struct {
		UserID string `json:"userId" validate:"required,uuid"`
	}
	if err := utils.DecodeJSON(r, &req); err != nil {
		utils.RespondError(w, http.StatusBadRequest, "Invalid request body")
		return
	}

	targetID, err := uuid.Parse(req.UserID)
	if err != nil {
		utils.RespondError(w, http.StatusBadRequest, "Invalid user ID")
		return
	}

	if err := h.service.AddAdmin(r.Context(), userID, targetID); err != nil {
		switch err {
		case ErrUserNotFound:
			utils.RespondErrorWithCode(w, http.StatusNotFound, "USER_NOT_FOUND", "User not found")
		case ErrAlreadyAdmin:
			utils.RespondErrorWithCode(w, http.StatusConflict, "ALREADY_ADMIN", "User is already an admin")
		default:
			utils.RespondError(w, http.StatusInternalServerError, "Failed to add admin")
		}
		return
	}

	utils.RespondJSON(w, http.StatusOK, map[string]string{"message": "Admin added successfully"})
}

func (h *Handler) RemoveAdmin(w http.ResponseWriter, r *http.Request) {
	userID, err := middleware.RequireAuth(r.Context())
	if err != nil {
		utils.RespondError(w, http.StatusUnauthorized, "Unauthorized")
		return
	}

	targetIDStr := chi.URLParam(r, "userId")
	targetID, err := uuid.Parse(targetIDStr)
	if err != nil {
		utils.RespondError(w, http.StatusBadRequest, "Invalid user ID")
		return
	}

	if err := h.service.RemoveAdmin(r.Context(), userID, targetID); err != nil {
		switch err {
		case ErrUserNotFound:
			utils.RespondErrorWithCode(w, http.StatusNotFound, "USER_NOT_FOUND", "User not found")
		case ErrNotAdmin:
			utils.RespondErrorWithCode(w, http.StatusBadRequest, "NOT_ADMIN", "User is not an admin")
		case ErrCannotRemoveSelf:
			utils.RespondErrorWithCode(w, http.StatusBadRequest, "CANNOT_REMOVE_SELF", "You cannot remove yourself as admin")
		case ErrLastAdmin:
			utils.RespondErrorWithCode(w, http.StatusBadRequest, "LAST_ADMIN", "Cannot remove the last admin")
		default:
			utils.RespondError(w, http.StatusInternalServerError, "Failed to remove admin")
		}
		return
	}

	utils.RespondJSON(w, http.StatusOK, map[string]string{"message": "Admin removed successfully"})
}

func (h *Handler) ListUsers(w http.ResponseWriter, r *http.Request) {
	page := utils.GetQueryInt(r, "page", 1)
	pageSize := utils.GetQueryInt(r, "pageSize", 20)
	query := utils.GetQueryString(r, "q", "")
	status := utils.GetQueryString(r, "status", "")

	if page < 1 {
		page = 1
	}
	if pageSize < 1 || pageSize > 100 {
		pageSize = 20
	}

	users, total, err := h.service.ListUsers(r.Context(), page, pageSize, query, status)
	if err != nil {
		utils.RespondError(w, http.StatusInternalServerError, "Failed to list users")
		return
	}

	utils.RespondPaginated(w, users, total, page, pageSize)
}

func (h *Handler) GetUser(w http.ResponseWriter, r *http.Request) {
	targetIDStr := chi.URLParam(r, "userId")
	targetID, err := uuid.Parse(targetIDStr)
	if err != nil {
		utils.RespondError(w, http.StatusBadRequest, "Invalid user ID")
		return
	}

	user, err := h.service.GetUser(r.Context(), targetID)
	if err != nil {
		switch err {
		case ErrUserNotFound:
			utils.RespondErrorWithCode(w, http.StatusNotFound, "USER_NOT_FOUND", "User not found")
		default:
			utils.RespondError(w, http.StatusInternalServerError, "Failed to get user")
		}
		return
	}

	utils.RespondSuccess(w, user)
}

func (h *Handler) UpdateUser(w http.ResponseWriter, r *http.Request) {
	userID, err := middleware.RequireAuth(r.Context())
	if err != nil {
		utils.RespondError(w, http.StatusUnauthorized, "Unauthorized")
		return
	}

	targetIDStr := chi.URLParam(r, "userId")
	targetID, err := uuid.Parse(targetIDStr)
	if err != nil {
		utils.RespondError(w, http.StatusBadRequest, "Invalid user ID")
		return
	}

	var req UpdateUserRequest
	if err := utils.DecodeJSON(r, &req); err != nil {
		utils.RespondError(w, http.StatusBadRequest, "Invalid request body")
		return
	}

	user, err := h.service.UpdateUser(r.Context(), userID, targetID, &req)
	if err != nil {
		switch err {
		case ErrUserNotFound:
			utils.RespondErrorWithCode(w, http.StatusNotFound, "USER_NOT_FOUND", "User not found")
		case ErrUsernameTaken:
			utils.RespondErrorWithCode(w, http.StatusConflict, "USERNAME_TAKEN", "Username is already taken")
		case ErrEmailTaken:
			utils.RespondErrorWithCode(w, http.StatusConflict, "EMAIL_TAKEN", "Email is already taken")
		default:
			utils.RespondError(w, http.StatusInternalServerError, "Failed to update user")
		}
		return
	}

	utils.RespondSuccess(w, user)
}

func (h *Handler) DeleteUser(w http.ResponseWriter, r *http.Request) {
	userID, err := middleware.RequireAuth(r.Context())
	if err != nil {
		utils.RespondError(w, http.StatusUnauthorized, "Unauthorized")
		return
	}

	targetIDStr := chi.URLParam(r, "userId")
	targetID, err := uuid.Parse(targetIDStr)
	if err != nil {
		utils.RespondError(w, http.StatusBadRequest, "Invalid user ID")
		return
	}

	if err := h.service.DeleteUser(r.Context(), userID, targetID); err != nil {
		switch err {
		case ErrUserNotFound:
			utils.RespondErrorWithCode(w, http.StatusNotFound, "USER_NOT_FOUND", "User not found")
		case ErrCannotDeleteSelf:
			utils.RespondErrorWithCode(w, http.StatusBadRequest, "CANNOT_DELETE_SELF", "You cannot delete yourself")
		case ErrCannotDeleteAdmin:
			utils.RespondErrorWithCode(w, http.StatusBadRequest, "CANNOT_DELETE_ADMIN", "Cannot delete an admin user. Remove admin status first.")
		default:
			utils.RespondError(w, http.StatusInternalServerError, "Failed to delete user")
		}
		return
	}

	utils.RespondJSON(w, http.StatusOK, map[string]string{"message": "User deleted successfully"})
}

func (h *Handler) RestoreUser(w http.ResponseWriter, r *http.Request) {
	targetIDStr := chi.URLParam(r, "userId")
	targetID, err := uuid.Parse(targetIDStr)
	if err != nil {
		utils.RespondError(w, http.StatusBadRequest, "Invalid user ID")
		return
	}

	if err := h.service.RestoreUser(r.Context(), targetID); err != nil {
		switch err {
		case ErrUserNotFound:
			utils.RespondErrorWithCode(w, http.StatusNotFound, "USER_NOT_FOUND", "User not found")
		case ErrUserNotDeleted:
			utils.RespondErrorWithCode(w, http.StatusBadRequest, "USER_NOT_DELETED", "User is not deleted")
		default:
			utils.RespondError(w, http.StatusInternalServerError, "Failed to restore user")
		}
		return
	}

	utils.RespondJSON(w, http.StatusOK, map[string]string{"message": "User restored successfully"})
}

func (h *Handler) GetServerInfo(w http.ResponseWriter, r *http.Request) {
	info := h.service.GetServerInfo(r.Context())
	utils.RespondSuccess(w, info)
}

func (h *Handler) GetServerConfig(w http.ResponseWriter, r *http.Request) {
	cfg, err := h.service.GetServerConfig(r.Context())
	if err != nil {
		utils.RespondError(w, http.StatusInternalServerError, "Failed to fetch server config")
		return
	}

	utils.RespondSuccess(w, cfg)
}

func (h *Handler) UpdateServerConfig(w http.ResponseWriter, r *http.Request) {
	var req UpdateServerConfigRequest
	if err := utils.DecodeJSON(r, &req); err != nil {
		utils.RespondError(w, http.StatusBadRequest, "Invalid request body")
		return
	}

	cfg, err := h.service.UpdateServerConfig(r.Context(), &req)
	if err != nil {
		utils.RespondError(w, http.StatusInternalServerError, "Failed to update server config")
		return
	}

	utils.RespondSuccess(w, cfg)
}
