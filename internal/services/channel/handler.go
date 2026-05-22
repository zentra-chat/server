package channel

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

	// Unread counts for all channels in a community
	r.Get("/communities/{communityId}/unread", h.GetCommunityUnreadCounts)

	// Community-scoped channel routes
	// This needs to be changed so we do not have two references to "channels" in the URL
	// I am keeping it like this for now to avoid breaking changes
	// Also because I am lazy and don't want to change the E2E tests right now
	// For me reading this later, this also applies to the messages routes
	r.Route("/communities/{communityId}/channels", func(r chi.Router) {
		r.Get("/", h.GetCommunityChannels)
		r.Post("/", h.CreateChannel)
		r.Put("/reorder", h.ReorderChannels)
	})

	// Community-scoped category routes
	r.Route("/communities/{communityId}/categories", func(r chi.Router) {
		r.Get("/", h.GetCategories)
		r.Post("/", h.CreateCategory)
		r.Put("/reorder", h.ReorderCategories)
	})

	// Channel-specific routes
	r.Route("/{id}", func(r chi.Router) {
		r.Get("/", h.GetChannel)
		r.Patch("/", h.UpdateChannel)
		r.Delete("/", h.DeleteChannel)
		r.Post("/read", h.MarkRead)

		// Permissions
		r.Get("/permissions", h.GetChannelPermissions)
		r.Put("/permissions", h.SetChannelPermission)
		r.Delete("/permissions/{targetType}/{targetId}", h.DeleteChannelPermission)
	})

	// Category-specific routes
	r.Route("/categories/{id}", func(r chi.Router) {
		r.Patch("/", h.UpdateCategory)
		r.Delete("/", h.DeleteCategory)
	})

	return r
}

func (h *Handler) CreateChannel(w http.ResponseWriter, r *http.Request) {
	userID, err := middleware.RequireAuth(r.Context())
	if err != nil {
		utils.RespondError(w, http.StatusUnauthorized, "Unauthorized")
		return
	}

	communityID, err := uuid.Parse(chi.URLParam(r, "communityId"))
	if err != nil {
		utils.RespondError(w, http.StatusBadRequest, "Invalid community ID")
		return
	}

	var req CreateChannelRequest
	if err := utils.DecodeJSON(r, &req); err != nil {
		utils.RespondError(w, http.StatusBadRequest, "Invalid request body")
		return
	}

	if err := utils.Validate(&req); err != nil {
		utils.RespondValidationError(w, utils.FormatValidationErrors(err))
		return
	}

	channel, err := h.service.CreateChannel(r.Context(), communityID, userID, &req)
	if err != nil {
		switch err {
		case ErrInsufficientPerms:
			utils.RespondError(w, http.StatusForbidden, "Insufficient permissions")
		case ErrInvalidChannelType:
			utils.RespondError(w, http.StatusBadRequest, "Invalid channel type")
		default:
			utils.RespondError(w, http.StatusInternalServerError, "Failed to create channel")
		}
		return
	}

	utils.RespondCreated(w, channel)
}

func (h *Handler) GetChannel(w http.ResponseWriter, r *http.Request) {
	userID, err := middleware.RequireAuth(r.Context())
	if err != nil {
		utils.RespondError(w, http.StatusUnauthorized, "Unauthorized")
		return
	}

	channelID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		utils.RespondError(w, http.StatusBadRequest, "Invalid channel ID")
		return
	}

	if !h.service.CanAccessChannel(r.Context(), channelID, userID) {
		utils.RespondError(w, http.StatusForbidden, "Cannot access this channel")
		return
	}

	channel, err := h.service.GetChannel(r.Context(), channelID)
	if err != nil {
		if err == ErrChannelNotFound {
			utils.RespondError(w, http.StatusNotFound, "Channel not found")
			return
		}
		utils.RespondError(w, http.StatusInternalServerError, "Failed to get channel")
		return
	}

	utils.RespondSuccess(w, channel)
}

func (h *Handler) GetCommunityChannels(w http.ResponseWriter, r *http.Request) {
	userID, err := middleware.RequireAuth(r.Context())
	if err != nil {
		utils.RespondError(w, http.StatusUnauthorized, "Unauthorized")
		return
	}

	communityID, err := uuid.Parse(chi.URLParam(r, "communityId"))
	if err != nil {
		utils.RespondError(w, http.StatusBadRequest, "Invalid community ID")
		return
	}

	// Check membership
	if !h.service.communityService.IsMember(r.Context(), communityID, userID) {
		utils.RespondError(w, http.StatusForbidden, "Not a member of this community")
		return
	}

	channels, err := h.service.GetCommunityChannels(r.Context(), communityID, userID)
	if err != nil {
		utils.RespondError(w, http.StatusInternalServerError, "Failed to get channels")
		return
	}

	utils.RespondSuccess(w, channels)
}

func (h *Handler) UpdateChannel(w http.ResponseWriter, r *http.Request) {
	userID, err := middleware.RequireAuth(r.Context())
	if err != nil {
		utils.RespondError(w, http.StatusUnauthorized, "Unauthorized")
		return
	}

	channelID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		utils.RespondError(w, http.StatusBadRequest, "Invalid channel ID")
		return
	}

	var req UpdateChannelRequest
	if err := utils.DecodeJSON(r, &req); err != nil {
		utils.RespondError(w, http.StatusBadRequest, "Invalid request body")
		return
	}

	channel, err := h.service.UpdateChannel(r.Context(), channelID, userID, &req)
	if err != nil {
		switch err {
		case ErrChannelNotFound:
			utils.RespondError(w, http.StatusNotFound, "Channel not found")
		case ErrInsufficientPerms:
			utils.RespondError(w, http.StatusForbidden, "Insufficient permissions")
		default:
			utils.RespondError(w, http.StatusInternalServerError, "Failed to update channel")
		}
		return
	}

	utils.RespondSuccess(w, channel)
}

func (h *Handler) DeleteChannel(w http.ResponseWriter, r *http.Request) {
	userID, err := middleware.RequireAuth(r.Context())
	if err != nil {
		utils.RespondError(w, http.StatusUnauthorized, "Unauthorized")
		return
	}

	channelID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		utils.RespondError(w, http.StatusBadRequest, "Invalid channel ID")
		return
	}

	if err := h.service.DeleteChannel(r.Context(), channelID, userID); err != nil {
		switch err {
		case ErrChannelNotFound:
			utils.RespondError(w, http.StatusNotFound, "Channel not found")
		case ErrInsufficientPerms:
			utils.RespondError(w, http.StatusForbidden, "Insufficient permissions")
		default:
			utils.RespondError(w, http.StatusInternalServerError, "Failed to delete channel")
		}
		return
	}

	utils.RespondNoContent(w)
}

func (h *Handler) ReorderChannels(w http.ResponseWriter, r *http.Request) {
	userID, err := middleware.RequireAuth(r.Context())
	if err != nil {
		utils.RespondError(w, http.StatusUnauthorized, "Unauthorized")
		return
	}

	communityID, err := uuid.Parse(chi.URLParam(r, "communityId"))
	if err != nil {
		utils.RespondError(w, http.StatusBadRequest, "Invalid community ID")
		return
	}

	var req struct {
		ChannelIDs []uuid.UUID `json:"channelIds" validate:"required,min=1"`
	}
	if err := utils.DecodeJSON(r, &req); err != nil {
		utils.RespondError(w, http.StatusBadRequest, "Invalid request body")
		return
	}

	if err := h.service.ReorderChannels(r.Context(), communityID, userID, req.ChannelIDs); err != nil {
		switch err {
		case ErrInsufficientPerms:
			utils.RespondError(w, http.StatusForbidden, "Insufficient permissions")
		default:
			utils.RespondError(w, http.StatusInternalServerError, "Failed to reorder channels")
		}
		return
	}

	utils.RespondNoContent(w)
}

func (h *Handler) CreateCategory(w http.ResponseWriter, r *http.Request) {
	userID, err := middleware.RequireAuth(r.Context())
	if err != nil {
		utils.RespondError(w, http.StatusUnauthorized, "Unauthorized")
		return
	}

	communityID, err := uuid.Parse(chi.URLParam(r, "communityId"))
	if err != nil {
		utils.RespondError(w, http.StatusBadRequest, "Invalid community ID")
		return
	}

	var req CreateCategoryRequest
	if err := utils.DecodeJSON(r, &req); err != nil {
		utils.RespondError(w, http.StatusBadRequest, "Invalid request body")
		return
	}

	if err := utils.Validate(&req); err != nil {
		utils.RespondValidationError(w, utils.FormatValidationErrors(err))
		return
	}

	category, err := h.service.CreateCategory(r.Context(), communityID, userID, &req)
	if err != nil {
		switch err {
		case ErrInsufficientPerms:
			utils.RespondError(w, http.StatusForbidden, "Insufficient permissions")
		default:
			utils.RespondError(w, http.StatusInternalServerError, "Failed to create category")
		}
		return
	}

	utils.RespondCreated(w, category)
}

func (h *Handler) GetCategories(w http.ResponseWriter, r *http.Request) {
	userID, err := middleware.RequireAuth(r.Context())
	if err != nil {
		utils.RespondError(w, http.StatusUnauthorized, "Unauthorized")
		return
	}

	communityID, err := uuid.Parse(chi.URLParam(r, "communityId"))
	if err != nil {
		utils.RespondError(w, http.StatusBadRequest, "Invalid community ID")
		return
	}

	if !h.service.communityService.IsMember(r.Context(), communityID, userID) {
		utils.RespondError(w, http.StatusForbidden, "Not a member of this community")
		return
	}

	categories, err := h.service.GetCategories(r.Context(), communityID)
	if err != nil {
		utils.RespondError(w, http.StatusInternalServerError, "Failed to get categories")
		return
	}

	utils.RespondSuccess(w, categories)
}

func (h *Handler) UpdateCategory(w http.ResponseWriter, r *http.Request) {
	userID, err := middleware.RequireAuth(r.Context())
	if err != nil {
		utils.RespondError(w, http.StatusUnauthorized, "Unauthorized")
		return
	}

	categoryID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		utils.RespondError(w, http.StatusBadRequest, "Invalid category ID")
		return
	}

	var req struct {
		Name string `json:"name" validate:"required,min=1,max=64"`
	}
	if err := utils.DecodeJSON(r, &req); err != nil {
		utils.RespondError(w, http.StatusBadRequest, "Invalid request body")
		return
	}

	category, err := h.service.UpdateCategory(r.Context(), categoryID, userID, req.Name)
	if err != nil {
		switch err {
		case ErrCategoryNotFound:
			utils.RespondError(w, http.StatusNotFound, "Category not found")
		case ErrInsufficientPerms:
			utils.RespondError(w, http.StatusForbidden, "Insufficient permissions")
		default:
			utils.RespondError(w, http.StatusInternalServerError, "Failed to update category")
		}
		return
	}

	utils.RespondSuccess(w, category)
}

func (h *Handler) DeleteCategory(w http.ResponseWriter, r *http.Request) {
	userID, err := middleware.RequireAuth(r.Context())
	if err != nil {
		utils.RespondError(w, http.StatusUnauthorized, "Unauthorized")
		return
	}

	categoryID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		utils.RespondError(w, http.StatusBadRequest, "Invalid category ID")
		return
	}

	if err := h.service.DeleteCategory(r.Context(), categoryID, userID); err != nil {
		switch err {
		case ErrCategoryNotFound:
			utils.RespondError(w, http.StatusNotFound, "Category not found")
		case ErrInsufficientPerms:
			utils.RespondError(w, http.StatusForbidden, "Insufficient permissions")
		default:
			utils.RespondError(w, http.StatusInternalServerError, "Failed to delete category")
		}
		return
	}

	utils.RespondNoContent(w)
}

func (h *Handler) ReorderCategories(w http.ResponseWriter, r *http.Request) {
	userID, err := middleware.RequireAuth(r.Context())
	if err != nil {
		utils.RespondError(w, http.StatusUnauthorized, "Unauthorized")
		return
	}

	communityID, err := uuid.Parse(chi.URLParam(r, "communityId"))
	if err != nil {
		utils.RespondError(w, http.StatusBadRequest, "Invalid community ID")
		return
	}

	var req struct {
		CategoryIDs []uuid.UUID `json:"categoryIds" validate:"required,min=1"`
	}
	if err := utils.DecodeJSON(r, &req); err != nil {
		utils.RespondError(w, http.StatusBadRequest, "Invalid request body")
		return
	}

	if err := h.service.ReorderCategories(r.Context(), communityID, userID, req.CategoryIDs); err != nil {
		switch err {
		case ErrInsufficientPerms:
			utils.RespondError(w, http.StatusForbidden, "Insufficient permissions")
		default:
			utils.RespondError(w, http.StatusInternalServerError, "Failed to reorder categories")
		}
		return
	}

	utils.RespondNoContent(w)
}

func (h *Handler) GetChannelPermissions(w http.ResponseWriter, r *http.Request) {
	userID, err := middleware.RequireAuth(r.Context())
	if err != nil {
		utils.RespondError(w, http.StatusUnauthorized, "Unauthorized")
		return
	}

	channelID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		utils.RespondError(w, http.StatusBadRequest, "Invalid channel ID")
		return
	}

	perms, err := h.service.GetChannelPermissions(r.Context(), channelID, userID)
	if err != nil {
		switch err {
		case ErrChannelNotFound:
			utils.RespondError(w, http.StatusNotFound, "Channel not found")
		case ErrInsufficientPerms:
			utils.RespondError(w, http.StatusForbidden, "Insufficient permissions")
		default:
			utils.RespondError(w, http.StatusInternalServerError, "Failed to get permissions")
		}
		return
	}

	utils.RespondSuccess(w, perms)
}

func (h *Handler) SetChannelPermission(w http.ResponseWriter, r *http.Request) {
	userID, err := middleware.RequireAuth(r.Context())
	if err != nil {
		utils.RespondError(w, http.StatusUnauthorized, "Unauthorized")
		return
	}

	channelID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		utils.RespondError(w, http.StatusBadRequest, "Invalid channel ID")
		return
	}

	var req SetChannelPermissionRequest
	if err := utils.DecodeJSON(r, &req); err != nil {
		utils.RespondError(w, http.StatusBadRequest, "Invalid request body")
		return
	}

	if err := utils.Validate(&req); err != nil {
		utils.RespondValidationError(w, utils.FormatValidationErrors(err))
		return
	}

	if err := h.service.SetChannelPermission(r.Context(), channelID, userID, &req); err != nil {
		switch err {
		case ErrChannelNotFound:
			utils.RespondError(w, http.StatusNotFound, "Channel not found")
		case ErrInsufficientPerms:
			utils.RespondError(w, http.StatusForbidden, "Insufficient permissions")
		default:
			utils.RespondError(w, http.StatusInternalServerError, "Failed to set permission")
		}
		return
	}

	utils.RespondNoContent(w)
}

func (h *Handler) DeleteChannelPermission(w http.ResponseWriter, r *http.Request) {
	userID, err := middleware.RequireAuth(r.Context())
	if err != nil {
		utils.RespondError(w, http.StatusUnauthorized, "Unauthorized")
		return
	}

	channelID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		utils.RespondError(w, http.StatusBadRequest, "Invalid channel ID")
		return
	}

	targetType := chi.URLParam(r, "targetType")
	targetID, err := uuid.Parse(chi.URLParam(r, "targetId"))
	if err != nil {
		utils.RespondError(w, http.StatusBadRequest, "Invalid target ID")
		return
	}

	if err := h.service.DeleteChannelPermission(r.Context(), channelID, userID, targetType, targetID); err != nil {
		switch err {
		case ErrChannelNotFound:
			utils.RespondError(w, http.StatusNotFound, "Channel not found")
		case ErrInsufficientPerms:
			utils.RespondError(w, http.StatusForbidden, "Insufficient permissions")
		default:
			utils.RespondError(w, http.StatusInternalServerError, "Failed to delete permission")
		}
		return
	}

	utils.RespondNoContent(w)
}

func (h *Handler) MarkRead(w http.ResponseWriter, r *http.Request) {
	userID, err := middleware.RequireAuth(r.Context())
	if err != nil {
		utils.RespondError(w, http.StatusUnauthorized, "Unauthorized")
		return
	}

	channelID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		utils.RespondError(w, http.StatusBadRequest, "Invalid channel ID")
		return
	}

	if !h.service.CanAccessChannel(r.Context(), channelID, userID) {
		utils.RespondError(w, http.StatusForbidden, "Cannot access this channel")
		return
	}

	if err := h.service.MarkRead(r.Context(), channelID, userID); err != nil {
		utils.RespondError(w, http.StatusInternalServerError, "Failed to mark channel as read")
		return
	}

	utils.RespondNoContent(w)
}

func (h *Handler) GetCommunityUnreadCounts(w http.ResponseWriter, r *http.Request) {
	userID, err := middleware.RequireAuth(r.Context())
	if err != nil {
		utils.RespondError(w, http.StatusUnauthorized, "Unauthorized")
		return
	}

	communityID, err := uuid.Parse(chi.URLParam(r, "communityId"))
	if err != nil {
		utils.RespondError(w, http.StatusBadRequest, "Invalid community ID")
		return
	}

	if !h.service.communityService.IsMember(r.Context(), communityID, userID) {
		utils.RespondError(w, http.StatusForbidden, "Not a member of this community")
		return
	}

	resp, err := h.service.GetCommunityUnreadCounts(r.Context(), communityID, userID)
	if err != nil {
		utils.RespondError(w, http.StatusInternalServerError, "Failed to get unread counts")
		return
	}

	utils.RespondSuccess(w, resp)
}
