package media

import (
	"net/http"
	"time"

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

	// Attachment routes
	r.Post("/attachments", h.UploadAttachment)
	r.Post("/attachments/dm", h.UploadDmAttachment)
	r.Get("/attachments/{id}", h.GetAttachment)
	r.Delete("/attachments/{id}", h.DeleteAttachment)
	r.Get("/attachments/{id}/download", h.GetPresignedURL)

	// Avatar routes
	r.Post("/avatars/user", h.UploadUserAvatar)
	r.Post("/avatars/community/{communityId}", h.UploadCommunityAvatar)

	// Banner routes
	r.Post("/banners/user", h.UploadUserBanner)

	// Community asset routes
	r.Post("/communities/{communityId}/banner", h.UploadCommunityBanner)
	r.Post("/communities/{communityId}/icon", h.UploadCommunityIcon)

	return r
}

func (h *Handler) UploadAttachment(w http.ResponseWriter, r *http.Request) {
	userID, err := middleware.RequireAuth(r.Context())
	if err != nil {
		utils.RespondError(w, http.StatusUnauthorized, "Unauthorized")
		return
	}

	// Limit request body size (100MB)
	r.Body = http.MaxBytesReader(w, r.Body, 100*1024*1024)

	// Parse multipart form
	if err := r.ParseMultipartForm(32 << 20); err != nil { // 32MB in memory
		utils.RespondError(w, http.StatusBadRequest, "Failed to parse form data")
		return
	}

	// Get channelId from form
	channelIDStr := r.FormValue("channelId")
	if channelIDStr == "" {
		utils.RespondError(w, http.StatusBadRequest, "channelId is required")
		return
	}

	channelID, err := uuid.Parse(channelIDStr)
	if err != nil {
		utils.RespondError(w, http.StatusBadRequest, "Invalid channelId")
		return
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		utils.RespondError(w, http.StatusBadRequest, "No file provided")
		return
	}
	defer file.Close()

	result, err := h.service.UploadAttachment(r.Context(), userID, channelID, file, header)
	if err != nil {
		switch err {
		case ErrFileTooLarge:
			utils.RespondError(w, http.StatusRequestEntityTooLarge, "File too large")
		case ErrInvalidFileType:
			utils.RespondError(w, http.StatusBadRequest, "Invalid file type")
		default:
			utils.RespondError(w, http.StatusInternalServerError, "Failed to upload file")
		}
		return
	}

	utils.RespondCreated(w, result)
}

func (h *Handler) UploadDmAttachment(w http.ResponseWriter, r *http.Request) {
	userID, err := middleware.RequireAuth(r.Context())
	if err != nil {
		utils.RespondError(w, http.StatusUnauthorized, "Unauthorized")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 100*1024*1024)

	if err := r.ParseMultipartForm(32 << 20); err != nil {
		utils.RespondError(w, http.StatusBadRequest, "Failed to parse form data")
		return
	}

	conversationIDStr := r.FormValue("conversationId")
	if conversationIDStr == "" {
		utils.RespondError(w, http.StatusBadRequest, "conversationId is required")
		return
	}

	conversationID, err := uuid.Parse(conversationIDStr)
	if err != nil {
		utils.RespondError(w, http.StatusBadRequest, "Invalid conversationId")
		return
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		utils.RespondError(w, http.StatusBadRequest, "No file provided")
		return
	}
	defer file.Close()

	result, err := h.service.UploadDmAttachment(r.Context(), userID, conversationID, file, header)
	if err != nil {
		switch err {
		case ErrFileTooLarge:
			utils.RespondError(w, http.StatusRequestEntityTooLarge, "File too large")
		case ErrInvalidFileType:
			utils.RespondError(w, http.StatusBadRequest, "Invalid file type")
		case ErrNotParticipant:
			utils.RespondError(w, http.StatusForbidden, "Not a participant")
		default:
			utils.RespondError(w, http.StatusInternalServerError, "Failed to upload file")
		}
		return
	}

	utils.RespondCreated(w, result)
}

func (h *Handler) GetAttachment(w http.ResponseWriter, r *http.Request) {
	userID, err := middleware.RequireAuth(r.Context())
	if err != nil {
		utils.RespondError(w, http.StatusUnauthorized, "Unauthorized")
		return
	}

	attachmentID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		utils.RespondError(w, http.StatusBadRequest, "Invalid attachment ID")
		return
	}

	if err := h.service.canAccessAttachment(r.Context(), attachmentID, userID); err != nil {
		if err == ErrAttachmentNotFound {
			utils.RespondError(w, http.StatusNotFound, "Attachment not found")
			return
		}
		utils.RespondError(w, http.StatusForbidden, "Access denied")
		return
	}

	attachment, err := h.service.GetAttachment(r.Context(), attachmentID)
	if err != nil {
		utils.RespondError(w, http.StatusInternalServerError, "Failed to get attachment")
		return
	}

	utils.RespondSuccess(w, attachment)
}

func (h *Handler) DeleteAttachment(w http.ResponseWriter, r *http.Request) {
	userID, err := middleware.RequireAuth(r.Context())
	if err != nil {
		utils.RespondError(w, http.StatusUnauthorized, "Unauthorized")
		return
	}

	attachmentID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		utils.RespondError(w, http.StatusBadRequest, "Invalid attachment ID")
		return
	}

	if err := h.service.DeleteAttachment(r.Context(), attachmentID, userID); err != nil {
		if err == ErrAttachmentNotFound {
			utils.RespondError(w, http.StatusNotFound, "Attachment not found")
			return
		}
		utils.RespondError(w, http.StatusInternalServerError, "Failed to delete attachment")
		return
	}

	utils.RespondNoContent(w)
}

func (h *Handler) GetPresignedURL(w http.ResponseWriter, r *http.Request) {
	userID, err := middleware.RequireAuth(r.Context())
	if err != nil {
		utils.RespondError(w, http.StatusUnauthorized, "Unauthorized")
		return
	}

	attachmentID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		utils.RespondError(w, http.StatusBadRequest, "Invalid attachment ID")
		return
	}

	if err := h.service.canAccessAttachment(r.Context(), attachmentID, userID); err != nil {
		if err == ErrAttachmentNotFound {
			utils.RespondError(w, http.StatusNotFound, "Attachment not found")
			return
		}
		utils.RespondError(w, http.StatusForbidden, "Access denied")
		return
	}

	// 1 hour expiry for download URLs
	url, err := h.service.GetPresignedURL(r.Context(), attachmentID, 1*time.Hour)
	if err != nil {
		if err == ErrAttachmentNotFound {
			utils.RespondError(w, http.StatusNotFound, "Attachment not found")
			return
		}
		utils.RespondError(w, http.StatusInternalServerError, "Failed to generate download URL")
		return
	}

	utils.RespondSuccess(w, map[string]string{"url": url})
}

func (h *Handler) UploadUserAvatar(w http.ResponseWriter, r *http.Request) {
	userID, err := middleware.RequireAuth(r.Context())
	if err != nil {
		utils.RespondError(w, http.StatusUnauthorized, "Unauthorized")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, MaxAvatarSize)

	if err := r.ParseMultipartForm(5 << 20); err != nil {
		utils.RespondError(w, http.StatusBadRequest, "Failed to parse form data")
		return
	}

	file, header, err := r.FormFile("avatar")
	if err != nil {
		utils.RespondError(w, http.StatusBadRequest, "No file provided")
		return
	}
	defer file.Close()

	url, err := h.service.UploadAvatar(r.Context(), userID, "users", file, header)
	if err != nil {
		switch err {
		case ErrFileTooLarge:
			utils.RespondError(w, http.StatusRequestEntityTooLarge, "File too large (max 5MB)")
		case ErrInvalidFileType:
			utils.RespondError(w, http.StatusBadRequest, "Invalid file type (images only)")
		default:
			utils.RespondError(w, http.StatusInternalServerError, "Failed to upload avatar")
		}
		return
	}

	utils.RespondSuccess(w, map[string]string{"url": url})
}

func (h *Handler) UploadCommunityAvatar(w http.ResponseWriter, r *http.Request) {
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

	if err := h.service.RequirePermission(r.Context(), communityID, userID, models.PermissionManageCommunity); err != nil {
		utils.RespondError(w, http.StatusForbidden, "Insufficient permissions")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, MaxAvatarSize)

	if err := r.ParseMultipartForm(5 << 20); err != nil {
		utils.RespondError(w, http.StatusBadRequest, "Failed to parse form data")
		return
	}

	file, header, err := r.FormFile("avatar")
	if err != nil {
		utils.RespondError(w, http.StatusBadRequest, "No file provided")
		return
	}
	defer file.Close()

	url, err := h.service.UploadAvatar(r.Context(), communityID, "communities", file, header)
	if err != nil {
		switch err {
		case ErrFileTooLarge:
			utils.RespondError(w, http.StatusRequestEntityTooLarge, "File too large (max 5MB)")
		case ErrInvalidFileType:
			utils.RespondError(w, http.StatusBadRequest, "Invalid file type (images only)")
		default:
			utils.RespondError(w, http.StatusInternalServerError, "Failed to upload avatar")
		}
		return
	}

	utils.RespondSuccess(w, map[string]string{"url": url})
}

func (h *Handler) UploadUserBanner(w http.ResponseWriter, r *http.Request) {
	userID, err := middleware.RequireAuth(r.Context())
	if err != nil {
		utils.RespondError(w, http.StatusUnauthorized, "Unauthorized")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, MaxImageSize)

	if err := r.ParseMultipartForm(10 << 20); err != nil {
		utils.RespondError(w, http.StatusBadRequest, "Failed to parse form data")
		return
	}

	file, header, err := r.FormFile("banner")
	if err != nil {
		utils.RespondError(w, http.StatusBadRequest, "No file provided")
		return
	}
	defer file.Close()

	bannerURL, err := h.service.UploadUserBanner(r.Context(), userID, file, header)
	if err != nil {
		switch err {
		case ErrFileTooLarge:
			utils.RespondError(w, http.StatusRequestEntityTooLarge, "File too large (max 10MB)")
		case ErrInvalidFileType:
			utils.RespondError(w, http.StatusBadRequest, "Invalid file type (images only)")
		default:
			utils.RespondError(w, http.StatusInternalServerError, "Failed to upload banner")
		}
		return
	}

	utils.RespondSuccess(w, map[string]string{"url": bannerURL})
}

func (h *Handler) UploadCommunityBanner(w http.ResponseWriter, r *http.Request) {
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

	if err := h.service.RequirePermission(r.Context(), communityID, userID, models.PermissionManageCommunity); err != nil {
		utils.RespondError(w, http.StatusForbidden, "Insufficient permissions")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, MaxImageSize)

	if err := r.ParseMultipartForm(10 << 20); err != nil {
		utils.RespondError(w, http.StatusBadRequest, "Failed to parse form data")
		return
	}

	file, header, err := r.FormFile("banner")
	if err != nil {
		utils.RespondError(w, http.StatusBadRequest, "No file provided")
		return
	}
	defer file.Close()

	url, err := h.service.UploadCommunityAsset(r.Context(), communityID, "banner", file, header)
	if err != nil {
		switch err {
		case ErrFileTooLarge:
			utils.RespondError(w, http.StatusRequestEntityTooLarge, "File too large (max 10MB)")
		case ErrInvalidFileType:
			utils.RespondError(w, http.StatusBadRequest, "Invalid file type (images only)")
		default:
			utils.RespondError(w, http.StatusInternalServerError, "Failed to upload banner")
		}
		return
	}

	if err := h.service.UpdateCommunityBanner(r.Context(), communityID, userID, url); err != nil {
		utils.RespondError(w, http.StatusInternalServerError, "Failed to update community banner")
		return
	}

	utils.RespondSuccess(w, map[string]string{"url": url})
}

func (h *Handler) UploadCommunityIcon(w http.ResponseWriter, r *http.Request) {
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

	if err := h.service.RequirePermission(r.Context(), communityID, userID, models.PermissionManageCommunity); err != nil {
		utils.RespondError(w, http.StatusForbidden, "Insufficient permissions")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, MaxAvatarSize)

	if err := r.ParseMultipartForm(5 << 20); err != nil {
		utils.RespondError(w, http.StatusBadRequest, "Failed to parse form data")
		return
	}

	file, header, err := r.FormFile("icon")
	if err != nil {
		utils.RespondError(w, http.StatusBadRequest, "No file provided")
		return
	}
	defer file.Close()

	url, err := h.service.UploadCommunityAsset(r.Context(), communityID, "icon", file, header)
	if err != nil {
		switch err {
		case ErrFileTooLarge:
			utils.RespondError(w, http.StatusRequestEntityTooLarge, "File too large (max 5MB)")
		case ErrInvalidFileType:
			utils.RespondError(w, http.StatusBadRequest, "Invalid file type (images only)")
		default:
			utils.RespondError(w, http.StatusInternalServerError, "Failed to upload icon")
		}
		return
	}

	if err := h.service.UpdateCommunityIcon(r.Context(), communityID, userID, url); err != nil {
		utils.RespondError(w, http.StatusInternalServerError, "Failed to update community icon")
		return
	}

	utils.RespondSuccess(w, map[string]string{"url": url})
}
