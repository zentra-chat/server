package user

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

	// Current user routes
	r.Get("/me", h.GetCurrentUser)
	r.Get("/me/id", h.GetCurrentUserID)
	r.Patch("/me", h.UpdateProfile)
	r.Delete("/me/avatar", h.RemoveAvatar)
	r.Delete("/me/banner", h.RemoveBanner)
	r.Get("/me/settings", h.GetSettings)
	r.Patch("/me/settings", h.UpdateSettings)
	r.Put("/me/status", h.UpdateStatus)
	r.Get("/me/relationships/{id}", h.GetRelationship)

	// Friend management
	r.Get("/me/friends", h.GetFriends)
	r.Get("/me/friends/requests", h.GetFriendRequests)
	r.Post("/me/friends/requests/{id}", h.SendFriendRequest)
	r.Post("/me/friends/requests/{id}/accept", h.AcceptFriendRequest)
	r.Delete("/me/friends/requests/{id}", h.RemoveFriendRequest)
	r.Delete("/me/friends/{id}", h.RemoveFriend)

	// Block management
	r.Get("/me/blocks", h.GetBlockedUsers)
	r.Post("/me/blocks/{id}", h.BlockUser)
	r.Delete("/me/blocks/{id}", h.UnblockUser)

	// User lookup routes
	r.Get("/search", h.SearchUsers)
	r.Get("/{id}", h.GetUser)
	r.Get("/username/{username}", h.GetUserByUsername)

	return r
}

func (h *Handler) GetCurrentUser(w http.ResponseWriter, r *http.Request) {
	userID, err := middleware.RequireAuth(r.Context())
	if err != nil {
		utils.RespondError(w, http.StatusUnauthorized, "Unauthorized")
		return
	}

	user, err := h.service.GetUserByID(r.Context(), userID)
	if err != nil {
		utils.RespondError(w, http.StatusInternalServerError, "Failed to get user")
		return
	}

	utils.RespondSuccess(w, user)
}

func (h *Handler) GetCurrentUserID(w http.ResponseWriter, r *http.Request) {
	userID, err := middleware.RequireAuth(r.Context())
	if err != nil {
		utils.RespondError(w, http.StatusUnauthorized, "Unauthorized")
		return
	}

	utils.RespondSuccess(w, map[string]string{
		"id": userID.String(),
	})
}

func (h *Handler) RemoveAvatar(w http.ResponseWriter, r *http.Request) {
	userID, err := middleware.RequireAuth(r.Context())
	if err != nil {
		utils.RespondError(w, http.StatusUnauthorized, "Unauthorized")
		return
	}

	if err := h.service.RemoveAvatar(r.Context(), userID); err != nil {
		utils.RespondError(w, http.StatusInternalServerError, "Failed to remove avatar")
		return
	}

	utils.RespondNoContent(w)
}

func (h *Handler) RemoveBanner(w http.ResponseWriter, r *http.Request) {
	userID, err := middleware.RequireAuth(r.Context())
	if err != nil {
		utils.RespondError(w, http.StatusUnauthorized, "Unauthorized")
		return
	}

	if err := h.service.RemoveBanner(r.Context(), userID); err != nil {
		utils.RespondError(w, http.StatusInternalServerError, "Failed to remove banner")
		return
	}

	utils.RespondNoContent(w)
}

func (h *Handler) GetUser(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		utils.RespondError(w, http.StatusBadRequest, "Invalid user ID")
		return
	}

	user, err := h.service.GetPublicUser(r.Context(), id)
	if err != nil {
		if err == ErrUserNotFound {
			utils.RespondError(w, http.StatusNotFound, "User not found")
			return
		}
		utils.RespondError(w, http.StatusInternalServerError, "Failed to get user")
		return
	}

	utils.RespondSuccess(w, user)
}

func (h *Handler) GetUserByUsername(w http.ResponseWriter, r *http.Request) {
	username := chi.URLParam(r, "username")
	if username == "" {
		utils.RespondError(w, http.StatusBadRequest, "Username is required")
		return
	}

	user, err := h.service.GetUserByUsername(r.Context(), username)
	if err != nil {
		if err == ErrUserNotFound {
			utils.RespondError(w, http.StatusNotFound, "User not found")
			return
		}
		utils.RespondError(w, http.StatusInternalServerError, "Failed to get user")
		return
	}

	utils.RespondSuccess(w, user)
}

func (h *Handler) UpdateProfile(w http.ResponseWriter, r *http.Request) {
	userID, err := middleware.RequireAuth(r.Context())
	if err != nil {
		utils.RespondError(w, http.StatusUnauthorized, "Unauthorized")
		return
	}

	var req UpdateProfileRequest
	if err := utils.DecodeJSON(r, &req); err != nil {
		utils.RespondError(w, http.StatusBadRequest, "Invalid request body")
		return
	}

	if err := utils.Validate(&req); err != nil {
		utils.RespondValidationError(w, utils.FormatValidationErrors(err))
		return
	}

	user, err := h.service.UpdateProfile(r.Context(), userID, &req)
	if err != nil {
		utils.RespondError(w, http.StatusInternalServerError, "Failed to update profile")
		return
	}

	utils.RespondSuccess(w, user)
}

func (h *Handler) UpdateStatus(w http.ResponseWriter, r *http.Request) {
	userID, err := middleware.RequireAuth(r.Context())
	if err != nil {
		utils.RespondError(w, http.StatusUnauthorized, "Unauthorized")
		return
	}

	var req struct {
		Status string `json:"status" validate:"required,oneof=online away busy invisible offline"`
	}
	if err := utils.DecodeJSON(r, &req); err != nil {
		utils.RespondError(w, http.StatusBadRequest, "Invalid request body")
		return
	}

	if err := utils.Validate(&req); err != nil {
		utils.RespondValidationError(w, utils.FormatValidationErrors(err))
		return
	}

	if err := h.service.UpdateStatus(r.Context(), userID, models.UserStatus(req.Status)); err != nil {
		utils.RespondError(w, http.StatusInternalServerError, "Failed to update status")
		return
	}

	utils.RespondNoContent(w)
}

func (h *Handler) SearchUsers(w http.ResponseWriter, r *http.Request) {
	query := utils.GetQueryString(r, "q", "")
	if query == "" {
		utils.RespondError(w, http.StatusBadRequest, "Search query is required")
		return
	}

	page := utils.GetQueryInt(r, "page", 1)
	pageSize := utils.GetQueryInt(r, "pageSize", 20)
	offset := (page - 1) * pageSize

	users, total, err := h.service.SearchUsers(r.Context(), query, pageSize, offset)
	if err != nil {
		utils.RespondError(w, http.StatusInternalServerError, "Failed to search users")
		return
	}

	utils.RespondPaginated(w, users, total, page, pageSize)
}

func (h *Handler) GetSettings(w http.ResponseWriter, r *http.Request) {
	userID, err := middleware.RequireAuth(r.Context())
	if err != nil {
		utils.RespondError(w, http.StatusUnauthorized, "Unauthorized")
		return
	}

	settings, err := h.service.GetSettings(r.Context(), userID)
	if err != nil {
		utils.RespondError(w, http.StatusInternalServerError, "Failed to get settings")
		return
	}

	utils.RespondSuccess(w, settings)
}

func (h *Handler) UpdateSettings(w http.ResponseWriter, r *http.Request) {
	userID, err := middleware.RequireAuth(r.Context())
	if err != nil {
		utils.RespondError(w, http.StatusUnauthorized, "Unauthorized")
		return
	}

	var req UpdateSettingsRequest
	if err := utils.DecodeJSON(r, &req); err != nil {
		utils.RespondError(w, http.StatusBadRequest, "Invalid request body")
		return
	}

	settings, err := h.service.UpdateSettings(r.Context(), userID, &req)
	if err != nil {
		utils.RespondError(w, http.StatusInternalServerError, "Failed to update settings")
		return
	}

	utils.RespondSuccess(w, settings)
}

func (h *Handler) GetRelationship(w http.ResponseWriter, r *http.Request) {
	userID, err := middleware.RequireAuth(r.Context())
	if err != nil {
		utils.RespondError(w, http.StatusUnauthorized, "Unauthorized")
		return
	}

	otherID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		utils.RespondError(w, http.StatusBadRequest, "Invalid user ID")
		return
	}

	relationship, err := h.service.GetRelationship(r.Context(), userID, otherID)
	if err != nil {
		if err == ErrUserNotFound {
			utils.RespondError(w, http.StatusNotFound, "User not found")
			return
		}
		utils.RespondError(w, http.StatusInternalServerError, "Failed to get relationship")
		return
	}

	utils.RespondSuccess(w, relationship)
}

func (h *Handler) GetFriends(w http.ResponseWriter, r *http.Request) {
	userID, err := middleware.RequireAuth(r.Context())
	if err != nil {
		utils.RespondError(w, http.StatusUnauthorized, "Unauthorized")
		return
	}

	friends, err := h.service.GetFriends(r.Context(), userID)
	if err != nil {
		utils.RespondError(w, http.StatusInternalServerError, "Failed to get friends")
		return
	}

	utils.RespondSuccess(w, friends)
}

func (h *Handler) GetFriendRequests(w http.ResponseWriter, r *http.Request) {
	userID, err := middleware.RequireAuth(r.Context())
	if err != nil {
		utils.RespondError(w, http.StatusUnauthorized, "Unauthorized")
		return
	}

	requests, err := h.service.GetFriendRequests(r.Context(), userID)
	if err != nil {
		utils.RespondError(w, http.StatusInternalServerError, "Failed to get friend requests")
		return
	}

	utils.RespondSuccess(w, requests)
}

func (h *Handler) SendFriendRequest(w http.ResponseWriter, r *http.Request) {
	userID, err := middleware.RequireAuth(r.Context())
	if err != nil {
		utils.RespondError(w, http.StatusUnauthorized, "Unauthorized")
		return
	}

	otherID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		utils.RespondError(w, http.StatusBadRequest, "Invalid user ID")
		return
	}

	if err := h.service.SendFriendRequest(r.Context(), userID, otherID); err != nil {
		switch err {
		case ErrCannotFriendYourself:
			utils.RespondError(w, http.StatusBadRequest, "Cannot add yourself as a friend")
		case ErrUserNotFound:
			utils.RespondError(w, http.StatusNotFound, "User not found")
		case ErrAlreadyFriends:
			utils.RespondError(w, http.StatusConflict, "You are already friends")
		case ErrFriendRequestExists:
			utils.RespondError(w, http.StatusConflict, "Friend request already sent")
		case ErrIncomingFriendRequest:
			utils.RespondError(w, http.StatusConflict, "This user already sent you a friend request")
		case ErrUsersBlocked:
			utils.RespondError(w, http.StatusForbidden, "Cannot send friend request")
		default:
			utils.RespondError(w, http.StatusInternalServerError, "Failed to send friend request")
		}
		return
	}

	utils.RespondNoContent(w)
}

func (h *Handler) AcceptFriendRequest(w http.ResponseWriter, r *http.Request) {
	userID, err := middleware.RequireAuth(r.Context())
	if err != nil {
		utils.RespondError(w, http.StatusUnauthorized, "Unauthorized")
		return
	}

	senderID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		utils.RespondError(w, http.StatusBadRequest, "Invalid user ID")
		return
	}

	if err := h.service.AcceptFriendRequest(r.Context(), userID, senderID); err != nil {
		switch err {
		case ErrCannotAcceptOwnRequest:
			utils.RespondError(w, http.StatusBadRequest, "Cannot accept your own friend request")
		case ErrUserNotFound:
			utils.RespondError(w, http.StatusNotFound, "User not found")
		case ErrUsersBlocked:
			utils.RespondError(w, http.StatusForbidden, "Cannot accept friend request")
		case ErrFriendRequestNotFound:
			utils.RespondError(w, http.StatusNotFound, "Friend request not found")
		default:
			utils.RespondError(w, http.StatusInternalServerError, "Failed to accept friend request")
		}
		return
	}

	utils.RespondNoContent(w)
}

func (h *Handler) RemoveFriendRequest(w http.ResponseWriter, r *http.Request) {
	userID, err := middleware.RequireAuth(r.Context())
	if err != nil {
		utils.RespondError(w, http.StatusUnauthorized, "Unauthorized")
		return
	}

	otherID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		utils.RespondError(w, http.StatusBadRequest, "Invalid user ID")
		return
	}

	if err := h.service.RemoveFriendRequest(r.Context(), userID, otherID); err != nil {
		switch err {
		case ErrCannotRemoveSelfRequest:
			utils.RespondError(w, http.StatusBadRequest, "Cannot remove your own friend request")
		case ErrUserNotFound:
			utils.RespondError(w, http.StatusNotFound, "User not found")
		case ErrFriendRequestNotFound:
			utils.RespondError(w, http.StatusNotFound, "Friend request not found")
		default:
			utils.RespondError(w, http.StatusInternalServerError, "Failed to remove friend request")
		}
		return
	}

	utils.RespondNoContent(w)
}

func (h *Handler) RemoveFriend(w http.ResponseWriter, r *http.Request) {
	userID, err := middleware.RequireAuth(r.Context())
	if err != nil {
		utils.RespondError(w, http.StatusUnauthorized, "Unauthorized")
		return
	}

	friendID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		utils.RespondError(w, http.StatusBadRequest, "Invalid user ID")
		return
	}

	if err := h.service.RemoveFriend(r.Context(), userID, friendID); err != nil {
		switch err {
		case ErrCannotRemoveSelfFriend:
			utils.RespondError(w, http.StatusBadRequest, "Cannot remove yourself as a friend")
		case ErrUserNotFound:
			utils.RespondError(w, http.StatusNotFound, "User not found")
		case ErrNotFriends:
			utils.RespondError(w, http.StatusNotFound, "User is not your friend")
		default:
			utils.RespondError(w, http.StatusInternalServerError, "Failed to remove friend")
		}
		return
	}

	utils.RespondNoContent(w)
}

func (h *Handler) GetBlockedUsers(w http.ResponseWriter, r *http.Request) {
	userID, err := middleware.RequireAuth(r.Context())
	if err != nil {
		utils.RespondError(w, http.StatusUnauthorized, "Unauthorized")
		return
	}

	users, err := h.service.GetBlockedUsers(r.Context(), userID)
	if err != nil {
		utils.RespondError(w, http.StatusInternalServerError, "Failed to get blocked users")
		return
	}

	utils.RespondSuccess(w, users)
}

func (h *Handler) BlockUser(w http.ResponseWriter, r *http.Request) {
	userID, err := middleware.RequireAuth(r.Context())
	if err != nil {
		utils.RespondError(w, http.StatusUnauthorized, "Unauthorized")
		return
	}

	blockedID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		utils.RespondError(w, http.StatusBadRequest, "Invalid user ID")
		return
	}

	if err := h.service.BlockUser(r.Context(), userID, blockedID); err != nil {
		if err == ErrUserNotFound {
			utils.RespondError(w, http.StatusNotFound, "User not found")
			return
		}
		utils.RespondError(w, http.StatusInternalServerError, "Failed to block user")
		return
	}

	utils.RespondNoContent(w)
}

func (h *Handler) UnblockUser(w http.ResponseWriter, r *http.Request) {
	userID, err := middleware.RequireAuth(r.Context())
	if err != nil {
		utils.RespondError(w, http.StatusUnauthorized, "Unauthorized")
		return
	}

	blockedID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		utils.RespondError(w, http.StatusBadRequest, "Invalid user ID")
		return
	}

	if err := h.service.UnblockUser(r.Context(), userID, blockedID); err != nil {
		if err == ErrNotBlocked {
			utils.RespondError(w, http.StatusBadRequest, "User is not blocked")
			return
		}
		utils.RespondError(w, http.StatusInternalServerError, "Failed to unblock user")
		return
	}

	utils.RespondNoContent(w)
}
