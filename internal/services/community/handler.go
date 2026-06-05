package community

import (
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/rs/zerolog/log"
	"github.com/zentra/server/internal/middleware"
	"github.com/zentra/server/internal/models"
	"github.com/zentra/server/internal/utils"
	"github.com/zentra/server/pkg/database"
)

type Handler struct {
	service            *Service
	discordImportToken string
}

func NewHandler(service *Service, discordImportToken string) *Handler {
	return &Handler{service: service, discordImportToken: discordImportToken}
}

func (h *Handler) Routes(secret string) chi.Router {
	r := chi.NewRouter()

	// Public routes (for discovery)
	r.Get("/discover", h.DiscoverCommunities)
	r.Get("/invite/{code}", h.GetInviteInfo)
	r.Get("/import/discord/status", h.GetDiscordImportStatus)
	r.Post("/import/discord", h.ImportDiscordServer)

	// Authenticated routes
	r.Group(func(r chi.Router) {
		r.Use(middleware.AuthMiddleware(secret))

		r.Post("/", h.CreateCommunity)
		r.Get("/", h.GetUserCommunities)
		r.Put("/reorder", h.ReorderCommunities)
		r.Post("/join/{code}", h.JoinWithInvite)

		r.Route("/{id}", func(r chi.Router) {
			r.Get("/", h.GetCommunity)
			r.Patch("/", h.UpdateCommunity)
			r.Delete("/", h.DeleteCommunity)

			r.Delete("/icon", h.RemoveCommunityIcon)
			r.Delete("/banner", h.RemoveCommunityBanner)

			r.Post("/join", h.JoinCommunity)
			r.Post("/leave", h.LeaveCommunity)

			// Members
			r.Get("/members", h.GetMembers)
			r.Delete("/members/{userId}", h.KickMember)
			r.Get("/members/{userId}/roles", h.GetMemberRoles)
			r.Put("/members/{userId}/roles", h.SetMemberRoles)

			// Bans
			r.Get("/bans", h.GetBans)
			r.Post("/bans/{userId}", h.BanMember)
			r.Delete("/bans/{userId}", h.UnbanMember)

			// Audit Log
			r.Get("/audit-log", h.GetAuditLog)

			// Invites
			r.Get("/invites", h.GetInvites)
			r.Post("/invites", h.CreateInvite)
			r.Delete("/invites/{inviteId}", h.DeleteInvite)

			// Roles
			r.Get("/roles", h.GetRoles)
			r.Post("/roles", h.CreateRole)
			r.Patch("/roles/{roleId}", h.UpdateRole)
			r.Delete("/roles/{roleId}", h.DeleteRole)
		})
	})

	return r
}

func (h *Handler) CreateCommunity(w http.ResponseWriter, r *http.Request) {
	userID, err := middleware.RequireAuth(r.Context())
	if err != nil {
		utils.RespondError(w, http.StatusUnauthorized, "Unauthorized")
		return
	}

	var req CreateCommunityRequest
	if err := utils.DecodeJSON(r, &req); err != nil {
		utils.RespondError(w, http.StatusBadRequest, "Invalid request body")
		return
	}

	if err := utils.Validate(&req); err != nil {
		utils.RespondValidationError(w, utils.FormatValidationErrors(err))
		return
	}

	community, err := h.service.CreateCommunity(r.Context(), userID, &req)
	if err != nil {
		utils.RespondError(w, http.StatusInternalServerError, "Failed to create community")
		return
	}

	utils.RespondCreated(w, community)
}

func (h *Handler) ImportDiscordServer(w http.ResponseWriter, r *http.Request) {
	configuredToken := strings.TrimSpace(h.discordImportToken)
	if configuredToken == "" {
		log.Warn().Msg("Discord import attempted while DISCORD_IMPORT_TOKEN is not configured")
		utils.RespondError(w, http.StatusServiceUnavailable, "Discord import is not configured")
		return
	}

	providedToken := strings.TrimSpace(r.Header.Get("X-Discord-Import-Token"))
	if providedToken != configuredToken {
		log.Warn().Int("providedTokenLen", len(providedToken)).Int("configuredTokenLen", len(configuredToken)).Msg("Discord import token mismatch")
		utils.RespondError(w, http.StatusUnauthorized, "Invalid import token")
		return
	}

	var req DiscordImportRequest
	if err := utils.DecodeJSON(r, &req); err != nil {
		utils.RespondError(w, http.StatusBadRequest, "Invalid request body")
		return
	}

	if err := utils.Validate(&req); err != nil {
		utils.RespondValidationError(w, utils.FormatValidationErrors(err))
		return
	}

	result, err := h.service.ImportDiscordServer(r.Context(), &req)
	if err != nil {
		log.Error().Err(err).Msg("Failed to import Discord server")
		utils.RespondError(w, http.StatusInternalServerError, "Failed to import Discord server: "+err.Error())
		return
	}

	utils.RespondCreated(w, result)
}

func (h *Handler) GetDiscordImportStatus(w http.ResponseWriter, r *http.Request) {
	utils.RespondSuccess(w, map[string]bool{
		"configured": strings.TrimSpace(h.discordImportToken) != "",
	})
}

func (h *Handler) GetCommunity(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		utils.RespondError(w, http.StatusBadRequest, "Invalid community ID")
		return
	}

	community, err := h.service.GetCommunity(r.Context(), id)
	if err != nil {
		if err == ErrCommunityNotFound {
			utils.RespondError(w, http.StatusNotFound, "Community not found")
			return
		}
		utils.RespondError(w, http.StatusInternalServerError, "Failed to get community")
		return
	}

	utils.RespondSuccess(w, community)
}

func (h *Handler) GetUserCommunities(w http.ResponseWriter, r *http.Request) {
	userID, err := middleware.RequireAuth(r.Context())
	if err != nil {
		utils.RespondError(w, http.StatusUnauthorized, "Unauthorized")
		return
	}

	communities, err := h.service.GetUserCommunities(r.Context(), userID)
	if err != nil {
		utils.RespondError(w, http.StatusInternalServerError, "Failed to get communities")
		return
	}

	utils.RespondSuccess(w, communities)
}

func (h *Handler) DiscoverCommunities(w http.ResponseWriter, r *http.Request) {
	query := utils.GetQueryString(r, "q", "")
	page := utils.GetQueryInt(r, "page", 1)
	pageSize := utils.GetQueryInt(r, "pageSize", 20)
	offset := (page - 1) * pageSize

	communities, total, err := h.service.DiscoverCommunities(r.Context(), query, pageSize, offset)
	if err != nil {
		utils.RespondError(w, http.StatusInternalServerError, "Failed to discover communities")
		return
	}

	utils.RespondPaginated(w, communities, total, page, pageSize)
}

func (h *Handler) UpdateCommunity(w http.ResponseWriter, r *http.Request) {
	userID, err := middleware.RequireAuth(r.Context())
	if err != nil {
		utils.RespondError(w, http.StatusUnauthorized, "Unauthorized")
		return
	}

	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		utils.RespondError(w, http.StatusBadRequest, "Invalid community ID")
		return
	}

	var req UpdateCommunityRequest
	if err := utils.DecodeJSON(r, &req); err != nil {
		utils.RespondError(w, http.StatusBadRequest, "Invalid request body")
		return
	}

	community, err := h.service.UpdateCommunity(r.Context(), id, userID, &req)
	if err != nil {
		switch err {
		case ErrCommunityNotFound:
			utils.RespondError(w, http.StatusNotFound, "Community not found")
		case ErrInsufficientPerms:
			utils.RespondError(w, http.StatusForbidden, "Insufficient permissions")
		default:
			utils.RespondError(w, http.StatusInternalServerError, "Failed to update community")
		}
		return
	}

	utils.RespondSuccess(w, community)
}

func (h *Handler) RemoveCommunityIcon(w http.ResponseWriter, r *http.Request) {
	userID, err := middleware.RequireAuth(r.Context())
	if err != nil {
		utils.RespondError(w, http.StatusUnauthorized, "Unauthorized")
		return
	}

	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		utils.RespondError(w, http.StatusBadRequest, "Invalid community ID")
		return
	}

	if err := h.service.RemoveCommunityIcon(r.Context(), id, userID); err != nil {
		switch err {
		case ErrCommunityNotFound:
			utils.RespondError(w, http.StatusNotFound, "Community not found")
		case ErrInsufficientPerms:
			utils.RespondError(w, http.StatusForbidden, "Insufficient permissions")
		default:
			utils.RespondError(w, http.StatusInternalServerError, "Failed to remove community icon")
		}
		return
	}

	utils.RespondNoContent(w)
}

func (h *Handler) RemoveCommunityBanner(w http.ResponseWriter, r *http.Request) {
	userID, err := middleware.RequireAuth(r.Context())
	if err != nil {
		utils.RespondError(w, http.StatusUnauthorized, "Unauthorized")
		return
	}

	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		utils.RespondError(w, http.StatusBadRequest, "Invalid community ID")
		return
	}

	if err := h.service.RemoveCommunityBanner(r.Context(), id, userID); err != nil {
		switch err {
		case ErrCommunityNotFound:
			utils.RespondError(w, http.StatusNotFound, "Community not found")
		case ErrInsufficientPerms:
			utils.RespondError(w, http.StatusForbidden, "Insufficient permissions")
		default:
			utils.RespondError(w, http.StatusInternalServerError, "Failed to remove community banner")
		}
		return
	}

	utils.RespondNoContent(w)
}

func (h *Handler) DeleteCommunity(w http.ResponseWriter, r *http.Request) {
	userID, err := middleware.RequireAuth(r.Context())
	if err != nil {
		utils.RespondError(w, http.StatusUnauthorized, "Unauthorized")
		return
	}

	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		utils.RespondError(w, http.StatusBadRequest, "Invalid community ID")
		return
	}

	if err := h.service.DeleteCommunity(r.Context(), id, userID); err != nil {
		switch err {
		case ErrCommunityNotFound:
			utils.RespondError(w, http.StatusNotFound, "Community not found")
		case ErrNotOwner:
			utils.RespondError(w, http.StatusForbidden, "Only the owner can delete the community")
		default:
			utils.RespondError(w, http.StatusInternalServerError, "Failed to delete community")
		}
		return
	}

	utils.RespondNoContent(w)
}

func (h *Handler) JoinCommunity(w http.ResponseWriter, r *http.Request) {
	userID, err := middleware.RequireAuth(r.Context())
	if err != nil {
		utils.RespondError(w, http.StatusUnauthorized, "Unauthorized")
		return
	}

	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		utils.RespondError(w, http.StatusBadRequest, "Invalid community ID")
		return
	}

	if err := h.service.JoinCommunity(r.Context(), id, userID); err != nil {
		switch err {
		case ErrCommunityNotFound:
			utils.RespondError(w, http.StatusNotFound, "Community not found")
		case ErrAlreadyMember:
			utils.RespondError(w, http.StatusConflict, "Already a member of this community")
		case ErrUserBanned:
			utils.RespondError(w, http.StatusForbidden, "You are banned from this community")
		default:
			utils.RespondError(w, http.StatusInternalServerError, "Failed to join community")
		}
		return
	}

	utils.RespondNoContent(w)
}

func (h *Handler) JoinWithInvite(w http.ResponseWriter, r *http.Request) {
	userID, err := middleware.RequireAuth(r.Context())
	if err != nil {
		utils.RespondError(w, http.StatusUnauthorized, "Unauthorized")
		return
	}

	code := chi.URLParam(r, "code")
	if code == "" {
		utils.RespondError(w, http.StatusBadRequest, "Invite code is required")
		return
	}

	community, err := h.service.JoinWithInvite(r.Context(), code, userID)
	if err != nil {
		switch err {
		case ErrInvalidInvite:
			utils.RespondError(w, http.StatusBadRequest, "Invalid or expired invite")
		case ErrAlreadyMember:
			utils.RespondError(w, http.StatusConflict, "Already a member of this community")
		case ErrUserBanned:
			utils.RespondError(w, http.StatusForbidden, "You are banned from this community")
		default:
			utils.RespondError(w, http.StatusInternalServerError, "Failed to join community")
		}
		return
	}

	utils.RespondSuccess(w, community)
}

func (h *Handler) ReorderCommunities(w http.ResponseWriter, r *http.Request) {
	userID, err := middleware.RequireAuth(r.Context())
	if err != nil {
		utils.RespondError(w, http.StatusUnauthorized, "Unauthorized")
		return
	}

	var req struct {
		CommunityIDs []uuid.UUID `json:"communityIds" validate:"required"`
	}
	if err := utils.DecodeJSON(r, &req); err != nil {
		utils.RespondError(w, http.StatusBadRequest, "Invalid request body")
		return
	}

	if err := h.service.ReorderCommunities(r.Context(), userID, req.CommunityIDs); err != nil {
		utils.RespondError(w, http.StatusInternalServerError, "Failed to reorder communities")
		return
	}

	utils.RespondNoContent(w)
}

func (h *Handler) GetInviteInfo(w http.ResponseWriter, r *http.Request) {
	code := chi.URLParam(r, "code")
	if code == "" {
		utils.RespondError(w, http.StatusBadRequest, "Invite code is required")
		return
	}

	// This is a public endpoint to check invite validity
	var communityID uuid.UUID
	var expiresAt *time.Time
	var maxUses, useCount *int

	err := database.Pool.QueryRow(r.Context(),
		`SELECT community_id, expires_at, max_uses, use_count FROM community_invites WHERE code = $1`,
		code,
	).Scan(&communityID, &expiresAt, &maxUses, &useCount)
	if err != nil {
		utils.RespondError(w, http.StatusNotFound, "Invite not found")
		return
	}

	// Check validity
	if expiresAt != nil && expiresAt.Before(time.Now()) {
		utils.RespondError(w, http.StatusGone, "Invite has expired")
		return
	}
	if maxUses != nil && useCount != nil && *useCount >= *maxUses {
		utils.RespondError(w, http.StatusGone, "Invite has reached maximum uses")
		return
	}

	community, err := h.service.GetCommunity(r.Context(), communityID)
	if err != nil {
		utils.RespondError(w, http.StatusNotFound, "Community not found")
		return
	}

	utils.RespondSuccess(w, map[string]interface{}{
		"community": community,
		"valid":     true,
	})
}

func (h *Handler) LeaveCommunity(w http.ResponseWriter, r *http.Request) {
	userID, err := middleware.RequireAuth(r.Context())
	if err != nil {
		utils.RespondError(w, http.StatusUnauthorized, "Unauthorized")
		return
	}

	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		utils.RespondError(w, http.StatusBadRequest, "Invalid community ID")
		return
	}

	if err := h.service.LeaveCommunity(r.Context(), id, userID); err != nil {
		utils.RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	utils.RespondNoContent(w)
}

func (h *Handler) GetMembers(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		utils.RespondError(w, http.StatusBadRequest, "Invalid community ID")
		return
	}

	page := utils.GetQueryInt(r, "page", 1)
	pageSize := utils.GetQueryInt(r, "pageSize", 50)
	offset := (page - 1) * pageSize

	members, total, err := h.service.GetMembers(r.Context(), id, pageSize, offset)
	if err != nil {
		utils.RespondError(w, http.StatusInternalServerError, "Failed to get members")
		return
	}

	utils.RespondPaginated(w, members, total, page, pageSize)
}

func (h *Handler) KickMember(w http.ResponseWriter, r *http.Request) {
	userID, err := middleware.RequireAuth(r.Context())
	if err != nil {
		utils.RespondError(w, http.StatusUnauthorized, "Unauthorized")
		return
	}

	communityID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		utils.RespondError(w, http.StatusBadRequest, "Invalid community ID")
		return
	}

	targetID, err := uuid.Parse(chi.URLParam(r, "userId"))
	if err != nil {
		utils.RespondError(w, http.StatusBadRequest, "Invalid user ID")
		return
	}

	if err := h.service.KickMember(r.Context(), communityID, userID, targetID); err != nil {
		switch err {
		case ErrInsufficientPerms:
			utils.RespondError(w, http.StatusForbidden, "Insufficient permissions")
		case ErrCannotRemoveOwner:
			utils.RespondError(w, http.StatusForbidden, "Cannot kick the owner")
		default:
			utils.RespondError(w, http.StatusInternalServerError, "Failed to kick member")
		}
		return
	}

	utils.RespondNoContent(w)
}

func (h *Handler) BanMember(w http.ResponseWriter, r *http.Request) {
	userID, err := middleware.RequireAuth(r.Context())
	if err != nil {
		utils.RespondError(w, http.StatusUnauthorized, "Unauthorized")
		return
	}

	communityID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		utils.RespondError(w, http.StatusBadRequest, "Invalid community ID")
		return
	}

	targetID, err := uuid.Parse(chi.URLParam(r, "userId"))
	if err != nil {
		utils.RespondError(w, http.StatusBadRequest, "Invalid user ID")
		return
	}

	var req BanMemberRequest
	// Body is optional for bans (reason is optional)
	_ = utils.DecodeJSON(r, &req)

	if err := h.service.BanMember(r.Context(), communityID, userID, targetID, req.Reason); err != nil {
		switch err {
		case ErrInsufficientPerms:
			utils.RespondError(w, http.StatusForbidden, "Insufficient permissions")
		case ErrCannotBanOwner:
			utils.RespondError(w, http.StatusForbidden, "Cannot ban the owner")
		default:
			utils.RespondError(w, http.StatusInternalServerError, "Failed to ban member")
		}
		return
	}

	utils.RespondNoContent(w)
}

func (h *Handler) UnbanMember(w http.ResponseWriter, r *http.Request) {
	userID, err := middleware.RequireAuth(r.Context())
	if err != nil {
		utils.RespondError(w, http.StatusUnauthorized, "Unauthorized")
		return
	}

	communityID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		utils.RespondError(w, http.StatusBadRequest, "Invalid community ID")
		return
	}

	targetID, err := uuid.Parse(chi.URLParam(r, "userId"))
	if err != nil {
		utils.RespondError(w, http.StatusBadRequest, "Invalid user ID")
		return
	}

	if err := h.service.UnbanMember(r.Context(), communityID, userID, targetID); err != nil {
		switch err {
		case ErrInsufficientPerms:
			utils.RespondError(w, http.StatusForbidden, "Insufficient permissions")
		case ErrNotBanned:
			utils.RespondError(w, http.StatusNotFound, "User is not banned")
		default:
			utils.RespondError(w, http.StatusInternalServerError, "Failed to unban member")
		}
		return
	}

	utils.RespondNoContent(w)
}

func (h *Handler) GetBans(w http.ResponseWriter, r *http.Request) {
	userID, err := middleware.RequireAuth(r.Context())
	if err != nil {
		utils.RespondError(w, http.StatusUnauthorized, "Unauthorized")
		return
	}

	communityID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		utils.RespondError(w, http.StatusBadRequest, "Invalid community ID")
		return
	}

	bans, err := h.service.GetBans(r.Context(), communityID, userID)
	if err != nil {
		switch err {
		case ErrInsufficientPerms:
			utils.RespondError(w, http.StatusForbidden, "Insufficient permissions")
		default:
			utils.RespondError(w, http.StatusInternalServerError, "Failed to get bans")
		}
		return
	}

	utils.RespondSuccess(w, bans)
}

func (h *Handler) GetAuditLog(w http.ResponseWriter, r *http.Request) {
	userID, err := middleware.RequireAuth(r.Context())
	if err != nil {
		utils.RespondError(w, http.StatusUnauthorized, "Unauthorized")
		return
	}

	communityID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		utils.RespondError(w, http.StatusBadRequest, "Invalid community ID")
		return
	}

	page := utils.GetQueryInt(r, "page", 1)
	pageSize := utils.GetQueryInt(r, "pageSize", 50)
	offset := (page - 1) * pageSize

	logs, total, err := h.service.GetAuditLogs(r.Context(), communityID, userID, pageSize, offset)
	if err != nil {
		switch err {
		case ErrInsufficientPerms:
			utils.RespondError(w, http.StatusForbidden, "Insufficient permissions")
		default:
			utils.RespondError(w, http.StatusInternalServerError, "Failed to get audit log")
		}
		return
	}

	utils.RespondPaginated(w, logs, total, page, pageSize)
}

func (h *Handler) GetInvites(w http.ResponseWriter, r *http.Request) {
	userID, err := middleware.RequireAuth(r.Context())
	if err != nil {
		utils.RespondError(w, http.StatusUnauthorized, "Unauthorized")
		return
	}

	communityID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		utils.RespondError(w, http.StatusBadRequest, "Invalid community ID")
		return
	}

	invites, err := h.service.GetInvites(r.Context(), communityID, userID)
	if err != nil {
		switch err {
		case ErrInsufficientPerms:
			utils.RespondError(w, http.StatusForbidden, "Insufficient permissions")
		default:
			utils.RespondError(w, http.StatusInternalServerError, "Failed to get invites")
		}
		return
	}

	utils.RespondSuccess(w, invites)
}

func (h *Handler) CreateInvite(w http.ResponseWriter, r *http.Request) {
	userID, err := middleware.RequireAuth(r.Context())
	if err != nil {
		utils.RespondError(w, http.StatusUnauthorized, "Unauthorized")
		return
	}

	communityID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		utils.RespondError(w, http.StatusBadRequest, "Invalid community ID")
		return
	}

	var req struct {
		MaxUses   *int   `json:"maxUses" validate:"omitempty,min=1,max=100"`
		ExpiresIn *int64 `json:"expiresIn"` // Duration in seconds
	}
	if err := utils.DecodeJSON(r, &req); err != nil {
		utils.RespondError(w, http.StatusBadRequest, "Invalid request body")
		return
	}

	var expiresIn *time.Duration
	if req.ExpiresIn != nil {
		d := time.Duration(*req.ExpiresIn) * time.Second
		expiresIn = &d
	}

	invite, err := h.service.CreateInvite(r.Context(), communityID, userID, req.MaxUses, expiresIn)
	if err != nil {
		switch err {
		case ErrInsufficientPerms:
			utils.RespondError(w, http.StatusForbidden, "Insufficient permissions")
		default:
			utils.RespondError(w, http.StatusInternalServerError, "Failed to create invite")
		}
		return
	}

	utils.RespondCreated(w, invite)
}

func (h *Handler) DeleteInvite(w http.ResponseWriter, r *http.Request) {
	userID, err := middleware.RequireAuth(r.Context())
	if err != nil {
		utils.RespondError(w, http.StatusUnauthorized, "Unauthorized")
		return
	}

	communityID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		utils.RespondError(w, http.StatusBadRequest, "Invalid community ID")
		return
	}

	inviteID, err := uuid.Parse(chi.URLParam(r, "inviteId"))
	if err != nil {
		utils.RespondError(w, http.StatusBadRequest, "Invalid invite ID")
		return
	}

	if err := h.service.DeleteInvite(r.Context(), communityID, inviteID, userID); err != nil {
		utils.RespondError(w, http.StatusInternalServerError, "Failed to delete invite")
		return
	}

	utils.RespondNoContent(w)
}

func (h *Handler) GetRoles(w http.ResponseWriter, r *http.Request) {
	communityID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		utils.RespondError(w, http.StatusBadRequest, "Invalid community ID")
		return
	}

	roles, err := h.service.GetRoles(r.Context(), communityID)
	if err != nil {
		utils.RespondError(w, http.StatusInternalServerError, "Failed to get roles")
		return
	}

	utils.RespondSuccess(w, roles)
}

func (h *Handler) CreateRole(w http.ResponseWriter, r *http.Request) {
	userID, err := middleware.RequireAuth(r.Context())
	if err != nil {
		utils.RespondError(w, http.StatusUnauthorized, "Unauthorized")
		return
	}

	communityID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		utils.RespondError(w, http.StatusBadRequest, "Invalid community ID")
		return
	}

	var req CreateRoleRequest
	if err := utils.DecodeJSON(r, &req); err != nil {
		utils.RespondError(w, http.StatusBadRequest, "Invalid request body")
		return
	}

	if err := utils.Validate(&req); err != nil {
		utils.RespondValidationError(w, utils.FormatValidationErrors(err))
		return
	}

	role, err := h.service.CreateRole(r.Context(), communityID, userID, &req)
	if err != nil {
		switch err {
		case ErrInsufficientPerms:
			utils.RespondError(w, http.StatusForbidden, "Insufficient permissions")
		default:
			utils.RespondError(w, http.StatusInternalServerError, "Failed to create role")
		}
		return
	}

	utils.RespondCreated(w, role)
}

func (h *Handler) DeleteRole(w http.ResponseWriter, r *http.Request) {
	userID, err := middleware.RequireAuth(r.Context())
	if err != nil {
		utils.RespondError(w, http.StatusUnauthorized, "Unauthorized")
		return
	}

	communityID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		utils.RespondError(w, http.StatusBadRequest, "Invalid community ID")
		return
	}

	roleID, err := uuid.Parse(chi.URLParam(r, "roleId"))
	if err != nil {
		utils.RespondError(w, http.StatusBadRequest, "Invalid role ID")
		return
	}

	if err := h.service.DeleteRole(r.Context(), communityID, roleID, userID); err != nil {
		switch err {
		case ErrInsufficientPerms:
			utils.RespondError(w, http.StatusForbidden, "Insufficient permissions")
		case ErrRoleNotFound:
			utils.RespondError(w, http.StatusNotFound, "Role not found")
		default:
			utils.RespondError(w, http.StatusInternalServerError, "Failed to delete role")
		}
		return
	}

	utils.RespondNoContent(w)
}

func (h *Handler) UpdateRole(w http.ResponseWriter, r *http.Request) {
	userID, err := middleware.RequireAuth(r.Context())
	if err != nil {
		utils.RespondError(w, http.StatusUnauthorized, "Unauthorized")
		return
	}

	communityID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		utils.RespondError(w, http.StatusBadRequest, "Invalid community ID")
		return
	}

	roleID, err := uuid.Parse(chi.URLParam(r, "roleId"))
	if err != nil {
		utils.RespondError(w, http.StatusBadRequest, "Invalid role ID")
		return
	}

	var req UpdateRoleRequest
	if err := utils.DecodeJSON(r, &req); err != nil {
		utils.RespondError(w, http.StatusBadRequest, "Invalid request body")
		return
	}

	if err := utils.Validate(&req); err != nil {
		utils.RespondValidationError(w, utils.FormatValidationErrors(err))
		return
	}

	role, err := h.service.UpdateRole(r.Context(), communityID, roleID, userID, &req)
	if err != nil {
		switch err {
		case ErrInsufficientPerms:
			utils.RespondError(w, http.StatusForbidden, "Insufficient permissions")
		case ErrRoleNotFound:
			utils.RespondError(w, http.StatusNotFound, "Role not found")
		default:
			utils.RespondError(w, http.StatusInternalServerError, "Failed to update role")
		}
		return
	}

	utils.RespondSuccess(w, role)
}

func (h *Handler) GetMemberRoles(w http.ResponseWriter, r *http.Request) {
	actorID, err := middleware.RequireAuth(r.Context())
	if err != nil {
		utils.RespondError(w, http.StatusUnauthorized, "Unauthorized")
		return
	}

	communityID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		utils.RespondError(w, http.StatusBadRequest, "Invalid community ID")
		return
	}

	targetID, err := uuid.Parse(chi.URLParam(r, "userId"))
	if err != nil {
		utils.RespondError(w, http.StatusBadRequest, "Invalid user ID")
		return
	}

	if actorID != targetID {
		if err := h.service.RequirePermission(r.Context(), communityID, actorID, models.PermissionManageRoles); err != nil {
			utils.RespondError(w, http.StatusForbidden, "Insufficient permissions")
			return
		}
	}

	roles, err := h.service.GetMemberRoles(r.Context(), communityID, targetID)
	if err != nil {
		switch err {
		case ErrNotMember:
			utils.RespondError(w, http.StatusNotFound, "Member not found")
		default:
			utils.RespondError(w, http.StatusInternalServerError, "Failed to get member roles")
		}
		return
	}

	utils.RespondSuccess(w, roles)
}

func (h *Handler) SetMemberRoles(w http.ResponseWriter, r *http.Request) {
	actorID, err := middleware.RequireAuth(r.Context())
	if err != nil {
		utils.RespondError(w, http.StatusUnauthorized, "Unauthorized")
		return
	}

	communityID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		utils.RespondError(w, http.StatusBadRequest, "Invalid community ID")
		return
	}

	targetID, err := uuid.Parse(chi.URLParam(r, "userId"))
	if err != nil {
		utils.RespondError(w, http.StatusBadRequest, "Invalid user ID")
		return
	}

	var req struct {
		RoleIDs []uuid.UUID `json:"roleIds"`
	}
	if err := utils.DecodeJSON(r, &req); err != nil {
		utils.RespondError(w, http.StatusBadRequest, "Invalid request body")
		return
	}

	if err := h.service.SetMemberRoles(r.Context(), communityID, actorID, targetID, req.RoleIDs); err != nil {
		switch err {
		case ErrInsufficientPerms, ErrNotOwner:
			utils.RespondError(w, http.StatusForbidden, "Insufficient permissions")
		case ErrRoleNotFound:
			utils.RespondError(w, http.StatusNotFound, "Role not found")
		case ErrNotMember:
			utils.RespondError(w, http.StatusNotFound, "Member not found")
		default:
			utils.RespondError(w, http.StatusInternalServerError, "Failed to update member roles")
		}
		return
	}

	utils.RespondNoContent(w)
}
