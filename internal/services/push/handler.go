package push

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/zentra/server/internal/middleware"
	"github.com/zentra/server/internal/models"
	"github.com/zentra/server/internal/utils"
)

// Handler exposes device registration endpoints.
type Handler struct {
	service *Service
}

func NewHandler(service *Service) *Handler {
	return &Handler{service: service}
}

// Routes returns the chi router for push device endpoints.
// Mount at /push under the authenticated group.
func (h *Handler) Routes() chi.Router {
	r := chi.NewRouter()

	r.Post("/devices", h.RegisterDevice)
	r.Delete("/devices", h.UnregisterDevice)
	r.Get("/devices", h.ListDevices)

	return r
}

type registerDeviceRequest struct {
	Platform   string `json:"platform" validate:"required,oneof=android ios"`
	Token      string `json:"token" validate:"required,max=4096"`
	DeviceName string `json:"deviceName" validate:"max=128"`
	AppVersion string `json:"appVersion" validate:"max=64"`
}

// POST /push/devices
func (h *Handler) RegisterDevice(w http.ResponseWriter, r *http.Request) {
	userID, err := middleware.RequireAuth(r.Context())
	if err != nil {
		utils.RespondError(w, http.StatusUnauthorized, "Unauthorized")
		return
	}

	var req registerDeviceRequest
	if err := utils.DecodeJSON(r, &req); err != nil {
		utils.RespondError(w, http.StatusBadRequest, "Invalid request body")
		return
	}
	if err := utils.Validate(&req); err != nil {
		utils.RespondValidationError(w, utils.FormatValidationErrors(err))
		return
	}

	err = h.service.Register(r.Context(), userID, models.PushPlatform(req.Platform), req.Token, req.DeviceName, req.AppVersion)
	if err != nil {
		utils.RespondError(w, http.StatusInternalServerError, "Failed to register device")
		return
	}

	utils.RespondSuccess(w, map[string]bool{"registered": true})
}

// DELETE /push/devices?token=...
func (h *Handler) UnregisterDevice(w http.ResponseWriter, r *http.Request) {
	userID, err := middleware.RequireAuth(r.Context())
	if err != nil {
		utils.RespondError(w, http.StatusUnauthorized, "Unauthorized")
		return
	}

	token := r.URL.Query().Get("token")
	if token == "" {
		utils.RespondError(w, http.StatusBadRequest, "Token is required")
		return
	}

	if err := h.service.Unregister(r.Context(), userID, token); err != nil {
		// An unknown token already means the device is gone, which is the state
		// the caller asked for, so it is not treated as a failure.
		if errors.Is(err, ErrNotFound) {
			utils.RespondNoContent(w)
			return
		}
		utils.RespondError(w, http.StatusInternalServerError, "Failed to unregister device")
		return
	}

	utils.RespondNoContent(w)
}

// GET /push/devices
func (h *Handler) ListDevices(w http.ResponseWriter, r *http.Request) {
	userID, err := middleware.RequireAuth(r.Context())
	if err != nil {
		utils.RespondError(w, http.StatusUnauthorized, "Unauthorized")
		return
	}

	devices, err := h.service.ListDevices(r.Context(), userID)
	if err != nil {
		utils.RespondError(w, http.StatusInternalServerError, "Failed to list devices")
		return
	}

	utils.RespondSuccess(w, map[string]any{
		"devices":     devices,
		"pushEnabled": h.service.Enabled(),
	})
}
