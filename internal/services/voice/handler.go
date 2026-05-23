package voice

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/zentra/server/internal/middleware"
	"github.com/zentra/server/internal/models"
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

	// Voice channel state routes
	r.Route("/channels/{channelId}", func(r chi.Router) {
		r.Get("/states", h.GetChannelVoiceStates)
		r.Post("/join", h.JoinChannel)
		r.Post("/leave", h.LeaveChannel)
		r.Patch("/state", h.UpdateVoiceState)
		r.Post("/mute/{userId}", h.ServerMuteUser)
	})

	// Current user voice state
	r.Get("/me", h.GetMyVoiceState)

	return r
}

func (h *Handler) JoinChannel(w http.ResponseWriter, r *http.Request) {
	userID, err := middleware.RequireAuth(r.Context())
	if err != nil {
		utils.RespondError(w, http.StatusUnauthorized, "Unauthorized")
		return
	}

	channelID, err := uuid.Parse(chi.URLParam(r, "channelId"))
	if err != nil {
		utils.RespondError(w, http.StatusBadRequest, "Invalid channel ID")
		return
	}

	state, err := h.service.JoinChannel(r.Context(), channelID, userID)
	if err != nil {
		switch err {
		case ErrNotVoiceChannel:
			utils.RespondError(w, http.StatusBadRequest, "Not a voice channel")
		case ErrInsufficientPerms:
			utils.RespondError(w, http.StatusForbidden, "Insufficient permissions")
		case ErrAlreadyInChannel:
			utils.RespondError(w, http.StatusConflict, "Already in this voice channel")
		default:
			utils.RespondError(w, http.StatusInternalServerError, "Failed to join voice channel")
		}
		return
	}

	utils.RespondSuccess(w, state)
}

func (h *Handler) LeaveChannel(w http.ResponseWriter, r *http.Request) {
	userID, err := middleware.RequireAuth(r.Context())
	if err != nil {
		utils.RespondError(w, http.StatusUnauthorized, "Unauthorized")
		return
	}

	channelID, err := uuid.Parse(chi.URLParam(r, "channelId"))
	if err != nil {
		utils.RespondError(w, http.StatusBadRequest, "Invalid channel ID")
		return
	}

	if err := h.service.LeaveChannel(r.Context(), channelID, userID); err != nil {
		if err == ErrNotInVoiceChannel {
			utils.RespondError(w, http.StatusNotFound, "Not in this voice channel")
			return
		}
		utils.RespondError(w, http.StatusInternalServerError, "Failed to leave voice channel")
		return
	}

	utils.RespondNoContent(w)
}

func (h *Handler) GetChannelVoiceStates(w http.ResponseWriter, r *http.Request) {
	userID, err := middleware.RequireAuth(r.Context())
	if err != nil {
		utils.RespondError(w, http.StatusUnauthorized, "Unauthorized")
		return
	}

	channelID, err := uuid.Parse(chi.URLParam(r, "channelId"))
	if err != nil {
		utils.RespondError(w, http.StatusBadRequest, "Invalid channel ID")
		return
	}

	// Check access
	if !h.service.channelService.CanAccessChannel(r.Context(), channelID, userID) {
		utils.RespondError(w, http.StatusForbidden, "Cannot access this channel")
		return
	}

	states, err := h.service.GetChannelVoiceStates(r.Context(), channelID)
	if err != nil {
		utils.RespondError(w, http.StatusInternalServerError, "Failed to get voice states")
		return
	}

	if states == nil {
		states = []*models.VoiceStateWithUser{}
	}

	utils.RespondSuccess(w, states)
}

func (h *Handler) UpdateVoiceState(w http.ResponseWriter, r *http.Request) {
	userID, err := middleware.RequireAuth(r.Context())
	if err != nil {
		utils.RespondError(w, http.StatusUnauthorized, "Unauthorized")
		return
	}

	channelID, err := uuid.Parse(chi.URLParam(r, "channelId"))
	if err != nil {
		utils.RespondError(w, http.StatusBadRequest, "Invalid channel ID")
		return
	}

	var req struct {
		IsSelfMuted     *bool `json:"isSelfMuted"`
		IsSelfDeafened  *bool `json:"isSelfDeafened"`
		IsScreenSharing *bool `json:"isScreenSharing"`
		IsWebcamOn      *bool `json:"isWebcamOn"`
	}
	if err := utils.DecodeJSON(r, &req); err != nil {
		utils.RespondError(w, http.StatusBadRequest, "Invalid request body")
		return
	}

	state, err := h.service.UpdateVoiceState(r.Context(), channelID, userID, req.IsSelfMuted, req.IsSelfDeafened, req.IsScreenSharing, req.IsWebcamOn)
	if err != nil {
		if err == ErrNotInVoiceChannel {
			utils.RespondError(w, http.StatusNotFound, "Not in this voice channel")
			return
		}
		utils.RespondError(w, http.StatusInternalServerError, "Failed to update voice state")
		return
	}

	utils.RespondSuccess(w, state)
}

func (h *Handler) ServerMuteUser(w http.ResponseWriter, r *http.Request) {
	actorID, err := middleware.RequireAuth(r.Context())
	if err != nil {
		utils.RespondError(w, http.StatusUnauthorized, "Unauthorized")
		return
	}

	channelID, err := uuid.Parse(chi.URLParam(r, "channelId"))
	if err != nil {
		utils.RespondError(w, http.StatusBadRequest, "Invalid channel ID")
		return
	}

	targetUserID, err := uuid.Parse(chi.URLParam(r, "userId"))
	if err != nil {
		utils.RespondError(w, http.StatusBadRequest, "Invalid user ID")
		return
	}

	var req struct {
		Muted bool `json:"muted"`
	}
	if err := utils.DecodeJSON(r, &req); err != nil {
		utils.RespondError(w, http.StatusBadRequest, "Invalid request body")
		return
	}

	state, err := h.service.ServerMuteUser(r.Context(), channelID, targetUserID, actorID, req.Muted)
	if err != nil {
		switch err {
		case ErrInsufficientPerms:
			utils.RespondError(w, http.StatusForbidden, "Insufficient permissions")
		case ErrNotInVoiceChannel:
			utils.RespondError(w, http.StatusNotFound, "User not in this voice channel")
		default:
			utils.RespondError(w, http.StatusInternalServerError, "Failed to mute user")
		}
		return
	}

	utils.RespondSuccess(w, state)
}

func (h *Handler) GetMyVoiceState(w http.ResponseWriter, r *http.Request) {
	userID, err := middleware.RequireAuth(r.Context())
	if err != nil {
		utils.RespondError(w, http.StatusUnauthorized, "Unauthorized")
		return
	}

	state, err := h.service.GetUserCurrentVoiceChannel(r.Context(), userID)
	if err != nil {
		if err == ErrNotInVoiceChannel {
			utils.RespondSuccess(w, nil)
			return
		}
		utils.RespondError(w, http.StatusInternalServerError, "Failed to get voice state")
		return
	}

	utils.RespondSuccess(w, state)
}
