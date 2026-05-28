package plugin

import (
	"encoding/json"
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

	// Bridge auth endpoint for sandboxed iframe plugins
	r.Post("/actions/{pluginId}/{communityId}", h.BridgeAction)

	// Global plugin catalog (marketplace browsing from within the app)
	r.Get("/", h.ListPlugins)
	r.Get("/search", h.SearchPlugins)
	r.Get("/{pluginId}", h.GetPlugin)

	// Per-community plugin management
	r.Route("/communities/{communityId}", func(r chi.Router) {
		r.Get("/", h.GetCommunityPlugins)
		r.Post("/install", h.InstallPlugin)
		r.Delete("/{pluginId}", h.UninstallPlugin)
		r.Patch("/{pluginId}/toggle", h.TogglePlugin)
		r.Patch("/{pluginId}/config", h.UpdateConfig)
		r.Patch("/{pluginId}/permissions", h.UpdatePermissions)
		r.Get("/{pluginId}", h.GetCommunityPlugin)
		r.Get("/audit-log", h.GetAuditLog)

		// Plugin sources
		r.Get("/sources", h.GetSources)
		r.Post("/sources", h.AddSource)
		r.Delete("/sources/{sourceId}", h.RemoveSource)
		r.Post("/sources/{sourceId}/sync", h.SyncSource)
	})

	return r
}

type bridgeActionRequest struct {
	Action string          `json:"action"`
	Data   json.RawMessage `json:"data"`
}

// BridgeAction proxies an action from a sandboxed iframe plugin to the WASM runtime.
// The bridge token in the Authorization header is validated to ensure the caller
// is the legitimate plugin sandbox for the given plugin/community pair.
func (h *Handler) BridgeAction(w http.ResponseWriter, r *http.Request) {
	pluginID, err := uuid.Parse(chi.URLParam(r, "pluginId"))
	if err != nil {
		utils.RespondError(w, http.StatusBadRequest, "Invalid plugin ID")
		return
	}

	communityID, err := uuid.Parse(chi.URLParam(r, "communityId"))
	if err != nil {
		utils.RespondError(w, http.StatusBadRequest, "Invalid community ID")
		return
	}

	var req bridgeActionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		utils.RespondError(w, http.StatusBadRequest, "Invalid request body")
		return
	}

	actions, err := h.service.WasmRuntime().Dispatch(r.Context(), pluginID, communityID, req.Action, req.Data)
	if err != nil {
		utils.RespondError(w, http.StatusInternalServerError, "Plugin action failed")
		return
	}

	utils.RespondSuccess(w, actions)
}

// ListPlugins returns all available plugins from the local catalog
func (h *Handler) ListPlugins(w http.ResponseWriter, r *http.Request) {
	_, err := middleware.RequireAuth(r.Context())
	if err != nil {
		utils.RespondError(w, http.StatusUnauthorized, "Unauthorized")
		return
	}

	source := r.URL.Query().Get("source")
	plugins, err := h.service.ListAvailablePlugins(r.Context(), source)
	if err != nil {
		utils.RespondError(w, http.StatusInternalServerError, "Failed to list plugins")
		return
	}

	utils.RespondSuccess(w, plugins)
}

// SearchPlugins searches the local plugin catalog
func (h *Handler) SearchPlugins(w http.ResponseWriter, r *http.Request) {
	_, err := middleware.RequireAuth(r.Context())
	if err != nil {
		utils.RespondError(w, http.StatusUnauthorized, "Unauthorized")
		return
	}

	q := r.URL.Query().Get("q")
	if q == "" {
		utils.RespondError(w, http.StatusBadRequest, "Search query required")
		return
	}

	plugins, err := h.service.SearchPlugins(r.Context(), q)
	if err != nil {
		utils.RespondError(w, http.StatusInternalServerError, "Search failed")
		return
	}

	utils.RespondSuccess(w, plugins)
}

// GetPlugin returns a single plugin's info
func (h *Handler) GetPlugin(w http.ResponseWriter, r *http.Request) {
	_, err := middleware.RequireAuth(r.Context())
	if err != nil {
		utils.RespondError(w, http.StatusUnauthorized, "Unauthorized")
		return
	}

	pluginID, err := uuid.Parse(chi.URLParam(r, "pluginId"))
	if err != nil {
		utils.RespondError(w, http.StatusBadRequest, "Invalid plugin ID")
		return
	}

	plugin, err := h.service.GetPlugin(r.Context(), pluginID)
	if err != nil {
		if err == ErrPluginNotFound {
			utils.RespondError(w, http.StatusNotFound, "Plugin not found")
			return
		}
		utils.RespondError(w, http.StatusInternalServerError, "Failed to get plugin")
		return
	}

	utils.RespondSuccess(w, plugin)
}

// GetCommunityPlugins lists all plugins on a community
func (h *Handler) GetCommunityPlugins(w http.ResponseWriter, r *http.Request) {
	_, err := middleware.RequireAuth(r.Context())
	if err != nil {
		utils.RespondError(w, http.StatusUnauthorized, "Unauthorized")
		return
	}

	communityID, err := uuid.Parse(chi.URLParam(r, "communityId"))
	if err != nil {
		utils.RespondError(w, http.StatusBadRequest, "Invalid community ID")
		return
	}

	plugins, err := h.service.GetCommunityPlugins(r.Context(), communityID)
	if err != nil {
		utils.RespondError(w, http.StatusInternalServerError, "Failed to load plugins")
		return
	}

	utils.RespondSuccess(w, plugins)
}

// GetCommunityPlugin gets a single installed plugin
func (h *Handler) GetCommunityPlugin(w http.ResponseWriter, r *http.Request) {
	_, err := middleware.RequireAuth(r.Context())
	if err != nil {
		utils.RespondError(w, http.StatusUnauthorized, "Unauthorized")
		return
	}

	communityID, err := uuid.Parse(chi.URLParam(r, "communityId"))
	if err != nil {
		utils.RespondError(w, http.StatusBadRequest, "Invalid community ID")
		return
	}

	pluginID, err := uuid.Parse(chi.URLParam(r, "pluginId"))
	if err != nil {
		utils.RespondError(w, http.StatusBadRequest, "Invalid plugin ID")
		return
	}

	cp, err := h.service.GetCommunityPlugin(r.Context(), communityID, pluginID)
	if err != nil {
		if err == ErrNotInstalled {
			utils.RespondError(w, http.StatusNotFound, "Plugin not installed")
			return
		}
		utils.RespondError(w, http.StatusInternalServerError, "Failed to get plugin")
		return
	}

	utils.RespondSuccess(w, cp)
}

type installRequest struct {
	PluginID           string `json:"pluginId"`
	GrantedPermissions int64  `json:"grantedPermissions"`
}

// InstallPlugin installs a plugin on a community
func (h *Handler) InstallPlugin(w http.ResponseWriter, r *http.Request) {
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

	var req installRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		utils.RespondError(w, http.StatusBadRequest, "Invalid request body")
		return
	}

	pluginID, err := uuid.Parse(req.PluginID)
	if err != nil {
		utils.RespondError(w, http.StatusBadRequest, "Invalid plugin ID")
		return
	}

	cp, err := h.service.InstallPlugin(r.Context(), communityID, pluginID, userID, req.GrantedPermissions)
	if err != nil {
		switch err {
		case ErrPluginNotFound:
			utils.RespondError(w, http.StatusNotFound, "Plugin not found")
		case ErrAlreadyInstalled:
			utils.RespondError(w, http.StatusConflict, "Plugin already installed")
		case ErrInvalidPermissions:
			utils.RespondError(w, http.StatusBadRequest, "Granted permissions exceed what the plugin requests")
		default:
			utils.RespondError(w, http.StatusInternalServerError, "Failed to install plugin")
		}
		return
	}

	utils.RespondJSON(w, http.StatusCreated, utils.SuccessResponse{Data: cp})
}

// UninstallPlugin removes a plugin from a community
func (h *Handler) UninstallPlugin(w http.ResponseWriter, r *http.Request) {
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

	pluginID, err := uuid.Parse(chi.URLParam(r, "pluginId"))
	if err != nil {
		utils.RespondError(w, http.StatusBadRequest, "Invalid plugin ID")
		return
	}

	if err := h.service.UninstallPlugin(r.Context(), communityID, pluginID, userID); err != nil {
		switch err {
		case ErrPluginNotFound:
			utils.RespondError(w, http.StatusNotFound, "Plugin not found")
		case ErrNotInstalled:
			utils.RespondError(w, http.StatusNotFound, "Plugin not installed")
		default:
			utils.RespondError(w, http.StatusInternalServerError, "Failed to uninstall plugin")
		}
		return
	}

	utils.RespondJSON(w, http.StatusNoContent, nil)
}

type toggleRequest struct {
	Enabled bool `json:"enabled"`
}

// TogglePlugin enables/disables a plugin
func (h *Handler) TogglePlugin(w http.ResponseWriter, r *http.Request) {
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

	pluginID, err := uuid.Parse(chi.URLParam(r, "pluginId"))
	if err != nil {
		utils.RespondError(w, http.StatusBadRequest, "Invalid plugin ID")
		return
	}

	var req toggleRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		utils.RespondError(w, http.StatusBadRequest, "Invalid request body")
		return
	}

	if err := h.service.TogglePlugin(r.Context(), communityID, pluginID, userID, req.Enabled); err != nil {
		switch err {
		case ErrNotInstalled:
			utils.RespondError(w, http.StatusNotFound, "Plugin not installed")
		default:
			utils.RespondError(w, http.StatusInternalServerError, "Failed to toggle plugin")
		}
		return
	}

	utils.RespondJSON(w, http.StatusNoContent, nil)
}

type configRequest struct {
	Config json.RawMessage `json:"config"`
}

// UpdateConfig updates plugin-specific settings for a community
func (h *Handler) UpdateConfig(w http.ResponseWriter, r *http.Request) {
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

	pluginID, err := uuid.Parse(chi.URLParam(r, "pluginId"))
	if err != nil {
		utils.RespondError(w, http.StatusBadRequest, "Invalid plugin ID")
		return
	}

	var req configRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		utils.RespondError(w, http.StatusBadRequest, "Invalid request body")
		return
	}

	if err := h.service.UpdatePluginConfig(r.Context(), communityID, pluginID, userID, req.Config); err != nil {
		if err == ErrNotInstalled {
			utils.RespondError(w, http.StatusNotFound, "Plugin not installed")
			return
		}
		utils.RespondError(w, http.StatusInternalServerError, "Failed to update config")
		return
	}

	utils.RespondJSON(w, http.StatusNoContent, nil)
}

type permissionsRequest struct {
	GrantedPermissions int64 `json:"grantedPermissions"`
}

// UpdatePermissions changes what a plugin is allowed to do
func (h *Handler) UpdatePermissions(w http.ResponseWriter, r *http.Request) {
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

	pluginID, err := uuid.Parse(chi.URLParam(r, "pluginId"))
	if err != nil {
		utils.RespondError(w, http.StatusBadRequest, "Invalid plugin ID")
		return
	}

	var req permissionsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		utils.RespondError(w, http.StatusBadRequest, "Invalid request body")
		return
	}

	if err := h.service.UpdatePluginPermissions(r.Context(), communityID, pluginID, userID, req.GrantedPermissions); err != nil {
		switch err {
		case ErrInvalidPermissions:
			utils.RespondError(w, http.StatusBadRequest, "Invalid permissions")
		case ErrNotInstalled:
			utils.RespondError(w, http.StatusNotFound, "Plugin not installed")
		default:
			utils.RespondError(w, http.StatusInternalServerError, "Failed to update permissions")
		}
		return
	}

	utils.RespondJSON(w, http.StatusNoContent, nil)
}

// GetSources lists plugin sources for a community
func (h *Handler) GetSources(w http.ResponseWriter, r *http.Request) {
	_, err := middleware.RequireAuth(r.Context())
	if err != nil {
		utils.RespondError(w, http.StatusUnauthorized, "Unauthorized")
		return
	}

	communityID, err := uuid.Parse(chi.URLParam(r, "communityId"))
	if err != nil {
		utils.RespondError(w, http.StatusBadRequest, "Invalid community ID")
		return
	}

	sources, err := h.service.GetSources(r.Context(), communityID)
	if err != nil {
		utils.RespondError(w, http.StatusInternalServerError, "Failed to load sources")
		return
	}

	utils.RespondSuccess(w, sources)
}

type addSourceRequest struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

// AddSource adds a plugin source repo
func (h *Handler) AddSource(w http.ResponseWriter, r *http.Request) {
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

	var req addSourceRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		utils.RespondError(w, http.StatusBadRequest, "Invalid request body")
		return
	}

	if req.Name == "" || req.URL == "" {
		utils.RespondError(w, http.StatusBadRequest, "Name and URL are required")
		return
	}

	src, err := h.service.AddSource(r.Context(), communityID, userID, req.Name, req.URL)
	if err != nil {
		utils.RespondError(w, http.StatusInternalServerError, "Failed to add source")
		return
	}

	utils.RespondJSON(w, http.StatusCreated, utils.SuccessResponse{Data: src})
}

// RemoveSource deletes a plugin source
func (h *Handler) RemoveSource(w http.ResponseWriter, r *http.Request) {
	_, err := middleware.RequireAuth(r.Context())
	if err != nil {
		utils.RespondError(w, http.StatusUnauthorized, "Unauthorized")
		return
	}

	communityID, err := uuid.Parse(chi.URLParam(r, "communityId"))
	if err != nil {
		utils.RespondError(w, http.StatusBadRequest, "Invalid community ID")
		return
	}

	sourceID, err := uuid.Parse(chi.URLParam(r, "sourceId"))
	if err != nil {
		utils.RespondError(w, http.StatusBadRequest, "Invalid source ID")
		return
	}

	if err := h.service.RemoveSource(r.Context(), communityID, sourceID); err != nil {
		if err == ErrSourceNotFound {
			utils.RespondError(w, http.StatusNotFound, "Source not found")
			return
		}
		utils.RespondError(w, http.StatusInternalServerError, "Failed to remove source")
		return
	}

	utils.RespondJSON(w, http.StatusNoContent, nil)
}

// SyncSource fetches plugins from a source and updates the local catalog
func (h *Handler) SyncSource(w http.ResponseWriter, r *http.Request) {
	_, err := middleware.RequireAuth(r.Context())
	if err != nil {
		utils.RespondError(w, http.StatusUnauthorized, "Unauthorized")
		return
	}

	communityID, err := uuid.Parse(chi.URLParam(r, "communityId"))
	if err != nil {
		utils.RespondError(w, http.StatusBadRequest, "Invalid community ID")
		return
	}

	sourceID, err := uuid.Parse(chi.URLParam(r, "sourceId"))
	if err != nil {
		utils.RespondError(w, http.StatusBadRequest, "Invalid source ID")
		return
	}

	// Look up the source URL
	sources, err := h.service.GetSources(r.Context(), communityID)
	if err != nil {
		utils.RespondError(w, http.StatusInternalServerError, "Failed to load sources")
		return
	}

	var sourceURL string
	for _, src := range sources {
		if src.ID == sourceID {
			sourceURL = src.URL
			break
		}
	}
	if sourceURL == "" {
		utils.RespondError(w, http.StatusNotFound, "Source not found")
		return
	}

	count, err := h.service.SyncFromSource(r.Context(), sourceURL)
	if err != nil {
		utils.RespondError(w, http.StatusBadGateway, "Failed to sync from source")
		return
	}

	utils.RespondSuccess(w, map[string]int{"synced": count})
}

// GetAuditLog returns plugin activity for a community
func (h *Handler) GetAuditLog(w http.ResponseWriter, r *http.Request) {
	_, err := middleware.RequireAuth(r.Context())
	if err != nil {
		utils.RespondError(w, http.StatusUnauthorized, "Unauthorized")
		return
	}

	communityID, err := uuid.Parse(chi.URLParam(r, "communityId"))
	if err != nil {
		utils.RespondError(w, http.StatusBadRequest, "Invalid community ID")
		return
	}

	entries, err := h.service.GetPluginAuditLog(r.Context(), communityID, 50)
	if err != nil {
		utils.RespondError(w, http.StatusInternalServerError, "Failed to load audit log")
		return
	}

	utils.RespondSuccess(w, entries)
}
