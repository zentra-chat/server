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
