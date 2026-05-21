package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog/log"
	"github.com/zentra/server/internal/models"
	"github.com/zentra/server/internal/services/channeltype"
	"github.com/zentra/server/internal/utils"
)

var (
	ErrPluginNotFound     = errors.New("plugin not found")
	ErrAlreadyInstalled   = errors.New("plugin is already installed on this community")
	ErrNotInstalled       = errors.New("plugin is not installed on this community")
	ErrBuiltInPlugin      = errors.New("cannot uninstall a built-in plugin")
	ErrSourceNotFound     = errors.New("plugin source not found")
	ErrSourceExists       = errors.New("source already exists for this community")
	ErrInvalidPermissions = errors.New("granted permissions exceed what the plugin requests")
	ErrFetchFailed        = errors.New("failed to fetch plugin from source")
)

type Service struct {
	db              *pgxpool.Pool
	channelRegistry *channeltype.Registry
	httpClient      *http.Client
}

func NewService(db *pgxpool.Pool, channelRegistry *channeltype.Registry) *Service {
	return &Service{
		db:              db,
		channelRegistry: channelRegistry,
		httpClient: &http.Client{
			Timeout: 15 * time.Second,
		},
	}
}

// GetPlugin fetches a single plugin by ID
func (s *Service) GetPlugin(ctx context.Context, pluginID uuid.UUID) (*models.Plugin, error) {
	plugin := &models.Plugin{}
	err := s.db.QueryRow(ctx,
		`SELECT id, slug, name, description, author, version, homepage_url, source_url, icon_url,
		        requested_permissions, manifest, built_in, source, is_verified, created_at, updated_at
		 FROM plugins WHERE id = $1`, pluginID,
	).Scan(
		&plugin.ID, &plugin.Slug, &plugin.Name, &plugin.Description, &plugin.Author, &plugin.Version,
		&plugin.HomepageURL, &plugin.SourceURL, &plugin.IconURL, &plugin.RequestedPermissions,
		&plugin.Manifest, &plugin.BuiltIn, &plugin.Source, &plugin.IsVerified, &plugin.CreatedAt, &plugin.UpdatedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrPluginNotFound
		}
		return nil, fmt.Errorf("get plugin: %w", err)
	}
	return plugin, nil
}

// GetPluginBySlug fetches a plugin by its unique slug
func (s *Service) GetPluginBySlug(ctx context.Context, slug string) (*models.Plugin, error) {
	plugin := &models.Plugin{}
	err := s.db.QueryRow(ctx,
		`SELECT id, slug, name, description, author, version, homepage_url, source_url, icon_url,
		        requested_permissions, manifest, built_in, source, is_verified, created_at, updated_at
		 FROM plugins WHERE slug = $1`, slug,
	).Scan(
		&plugin.ID, &plugin.Slug, &plugin.Name, &plugin.Description, &plugin.Author, &plugin.Version,
		&plugin.HomepageURL, &plugin.SourceURL, &plugin.IconURL, &plugin.RequestedPermissions,
		&plugin.Manifest, &plugin.BuiltIn, &plugin.Source, &plugin.IsVerified, &plugin.CreatedAt, &plugin.UpdatedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrPluginNotFound
		}
		return nil, fmt.Errorf("get plugin by slug: %w", err)
	}
	return plugin, nil
}

// ListAvailablePlugins returns all plugins in the system (for marketplace browsing)
func (s *Service) ListAvailablePlugins(ctx context.Context, source string) ([]*models.Plugin, error) {
	query := `SELECT id, slug, name, description, author, version, homepage_url, source_url, icon_url,
	                  requested_permissions, manifest, built_in, source, is_verified, created_at, updated_at
	           FROM plugins`
	args := []any{}

	if source != "" {
		query += " WHERE source = $1"
		args = append(args, source)
	}
	query += " ORDER BY built_in DESC, name ASC"

	rows, err := s.db.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list plugins: %w", err)
	}
	defer rows.Close()

	var plugins []*models.Plugin
	for rows.Next() {
		p := &models.Plugin{}
		if err := rows.Scan(
			&p.ID, &p.Slug, &p.Name, &p.Description, &p.Author, &p.Version,
			&p.HomepageURL, &p.SourceURL, &p.IconURL, &p.RequestedPermissions,
			&p.Manifest, &p.BuiltIn, &p.Source, &p.IsVerified, &p.CreatedAt, &p.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan plugin: %w", err)
		}
		plugins = append(plugins, p)
	}
	return plugins, rows.Err()
}

// SearchPlugins searches available plugins by name or description
func (s *Service) SearchPlugins(ctx context.Context, query string) ([]*models.Plugin, error) {
	query = utils.SanitizeSearchQuery(query)

	rows, err := s.db.Query(ctx,
		`SELECT id, slug, name, description, author, version, homepage_url, source_url, icon_url,
		        requested_permissions, manifest, built_in, source, is_verified, created_at, updated_at
		 FROM plugins
		 WHERE name ILIKE '%' || $1 || '%' OR description ILIKE '%' || $1 || '%' OR slug ILIKE '%' || $1 || '%'
		 ORDER BY is_verified DESC, name ASC
		 LIMIT 50`, query,
	)
	if err != nil {
		return nil, fmt.Errorf("search plugins: %w", err)
	}
	defer rows.Close()

	var plugins []*models.Plugin
	for rows.Next() {
		p := &models.Plugin{}
		if err := rows.Scan(
			&p.ID, &p.Slug, &p.Name, &p.Description, &p.Author, &p.Version,
			&p.HomepageURL, &p.SourceURL, &p.IconURL, &p.RequestedPermissions,
			&p.Manifest, &p.BuiltIn, &p.Source, &p.IsVerified, &p.CreatedAt, &p.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan plugin: %w", err)
		}
		plugins = append(plugins, p)
	}
	return plugins, rows.Err()
}

// InstallPlugin puts a plugin on a community. Server owners decide which permissions to grant.
func (s *Service) InstallPlugin(ctx context.Context, communityID, pluginID, installedBy uuid.UUID, grantedPermissions int64) (*models.CommunityPlugin, error) {
	// Grab the plugin's definition first
	plugin, err := s.GetPlugin(ctx, pluginID)
	if err != nil {
		return nil, err
	}

	// Don't let people grant more than the plugin asks for
	if grantedPermissions & ^plugin.RequestedPermissions != 0 {
		return nil, ErrInvalidPermissions
	}

	cp := &models.CommunityPlugin{}
	err = s.db.QueryRow(ctx,
		`INSERT INTO community_plugins (community_id, plugin_id, enabled, granted_permissions, installed_by)
		 VALUES ($1, $2, TRUE, $3, $4)
		 ON CONFLICT (community_id, plugin_id) DO NOTHING
		 RETURNING id, community_id, plugin_id, enabled, granted_permissions, config, installed_by, installed_at, updated_at`,
		communityID, pluginID, grantedPermissions, installedBy,
	).Scan(
		&cp.ID, &cp.CommunityID, &cp.PluginID, &cp.Enabled, &cp.GrantedPermissions,
		&cp.Config, &cp.InstalledBy, &cp.InstalledAt, &cp.UpdatedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrAlreadyInstalled
		}
		return nil, fmt.Errorf("install plugin: %w", err)
	}
	cp.Plugin = plugin

	// Register any channel types this plugin provides
	s.registerPluginChannelTypes(ctx, plugin)

	// Log the install
	s.logAction(ctx, communityID, pluginID, installedBy, "install", map[string]any{
		"grantedPermissions": grantedPermissions,
	})

	return cp, nil
}

// UninstallPlugin removes a plugin from a community
func (s *Service) UninstallPlugin(ctx context.Context, communityID, pluginID, actorID uuid.UUID) error {
	plugin, err := s.GetPlugin(ctx, pluginID)
	if err != nil {
		return err
	}
	if plugin.BuiltIn {
		return ErrBuiltInPlugin
	}

	tag, err := s.db.Exec(ctx,
		`DELETE FROM community_plugins WHERE community_id = $1 AND plugin_id = $2`,
		communityID, pluginID,
	)
	if err != nil {
		return fmt.Errorf("uninstall plugin: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotInstalled
	}

	s.logAction(ctx, communityID, pluginID, actorID, "uninstall", nil)
	return nil
}

// TogglePlugin enables or disables a plugin on a community without removing it
func (s *Service) TogglePlugin(ctx context.Context, communityID, pluginID, actorID uuid.UUID, enabled bool) error {
	plugin, err := s.GetPlugin(ctx, pluginID)
	if err != nil {
		return err
	}
	if plugin.BuiltIn {
		return ErrBuiltInPlugin
	}

	tag, err := s.db.Exec(ctx,
		`UPDATE community_plugins SET enabled = $3, updated_at = NOW()
		 WHERE community_id = $1 AND plugin_id = $2`,
		communityID, pluginID, enabled,
	)
	if err != nil {
		return fmt.Errorf("toggle plugin: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotInstalled
	}

	action := "disable"
	if enabled {
		action = "enable"
	}
	s.logAction(ctx, communityID, pluginID, actorID, action, nil)
	return nil
}

// UpdatePluginConfig lets server owners change plugin-specific settings
func (s *Service) UpdatePluginConfig(ctx context.Context, communityID, pluginID, actorID uuid.UUID, config json.RawMessage) error {
	tag, err := s.db.Exec(ctx,
		`UPDATE community_plugins SET config = $3, updated_at = NOW()
		 WHERE community_id = $1 AND plugin_id = $2`,
		communityID, pluginID, config,
	)
	if err != nil {
		return fmt.Errorf("update plugin config: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotInstalled
	}

	s.logAction(ctx, communityID, pluginID, actorID, "config_update", nil)
	return nil
}

// UpdatePluginPermissions lets server owners change what a plugin is allowed to do
func (s *Service) UpdatePluginPermissions(ctx context.Context, communityID, pluginID, actorID uuid.UUID, grantedPermissions int64) error {
	plugin, err := s.GetPlugin(ctx, pluginID)
	if err != nil {
		return err
	}
	if grantedPermissions & ^plugin.RequestedPermissions != 0 {
		return ErrInvalidPermissions
	}

	tag, err := s.db.Exec(ctx,
		`UPDATE community_plugins SET granted_permissions = $3, updated_at = NOW()
		 WHERE community_id = $1 AND plugin_id = $2`,
		communityID, pluginID, grantedPermissions,
	)
	if err != nil {
		return fmt.Errorf("update plugin permissions: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotInstalled
	}

	s.logAction(ctx, communityID, pluginID, actorID, "permission_update", map[string]any{
		"grantedPermissions": grantedPermissions,
	})
	return nil
}

// GetCommunityPlugins lists all plugins installed on a community
func (s *Service) GetCommunityPlugins(ctx context.Context, communityID uuid.UUID) ([]*models.CommunityPlugin, error) {
	rows, err := s.db.Query(ctx,
		`SELECT cp.id, cp.community_id, cp.plugin_id, cp.enabled, cp.granted_permissions,
		        cp.config, cp.installed_by, cp.installed_at, cp.updated_at,
		        p.id, p.slug, p.name, p.description, p.author, p.version, p.homepage_url, p.source_url, p.icon_url,
		        p.requested_permissions, p.manifest, p.built_in, p.source, p.is_verified, p.created_at, p.updated_at
		 FROM community_plugins cp
		 JOIN plugins p ON p.id = cp.plugin_id
		 WHERE cp.community_id = $1
		 ORDER BY p.built_in DESC, p.name ASC`, communityID,
	)
	if err != nil {
		return nil, fmt.Errorf("get community plugins: %w", err)
	}
	defer rows.Close()

	list := make([]*models.CommunityPlugin, 0)
	for rows.Next() {
		cp := &models.CommunityPlugin{}
		p := &models.Plugin{}
		if err := rows.Scan(
			&cp.ID, &cp.CommunityID, &cp.PluginID, &cp.Enabled, &cp.GrantedPermissions,
			&cp.Config, &cp.InstalledBy, &cp.InstalledAt, &cp.UpdatedAt,
			&p.ID, &p.Slug, &p.Name, &p.Description, &p.Author, &p.Version, &p.HomepageURL, &p.SourceURL, &p.IconURL,
			&p.RequestedPermissions, &p.Manifest, &p.BuiltIn, &p.Source, &p.IsVerified, &p.CreatedAt, &p.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan community plugin: %w", err)
		}
		cp.Plugin = p
		list = append(list, cp)
	}
	return list, rows.Err()
}

// GetCommunityPlugin gets a single plugin installation
func (s *Service) GetCommunityPlugin(ctx context.Context, communityID, pluginID uuid.UUID) (*models.CommunityPlugin, error) {
	cp := &models.CommunityPlugin{}
	p := &models.Plugin{}
	err := s.db.QueryRow(ctx,
		`SELECT cp.id, cp.community_id, cp.plugin_id, cp.enabled, cp.granted_permissions,
		        cp.config, cp.installed_by, cp.installed_at, cp.updated_at,
		        p.id, p.slug, p.name, p.description, p.author, p.version, p.homepage_url, p.source_url, p.icon_url,
		        p.requested_permissions, p.manifest, p.built_in, p.source, p.is_verified, p.created_at, p.updated_at
		 FROM community_plugins cp
		 JOIN plugins p ON p.id = cp.plugin_id
		 WHERE cp.community_id = $1 AND cp.plugin_id = $2`, communityID, pluginID,
	).Scan(
		&cp.ID, &cp.CommunityID, &cp.PluginID, &cp.Enabled, &cp.GrantedPermissions,
		&cp.Config, &cp.InstalledBy, &cp.InstalledAt, &cp.UpdatedAt,
		&p.ID, &p.Slug, &p.Name, &p.Description, &p.Author, &p.Version, &p.HomepageURL, &p.SourceURL, &p.IconURL,
		&p.RequestedPermissions, &p.Manifest, &p.BuiltIn, &p.Source, &p.IsVerified, &p.CreatedAt, &p.UpdatedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotInstalled
		}
		return nil, fmt.Errorf("get community plugin: %w", err)
	}
	cp.Plugin = p
	return cp, nil
}

// IsPluginInstalled checks if a plugin is active on a community
func (s *Service) IsPluginInstalled(ctx context.Context, communityID, pluginID uuid.UUID) (bool, error) {
	var exists bool
	err := s.db.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM community_plugins WHERE community_id = $1 AND plugin_id = $2 AND enabled = TRUE)`,
		communityID, pluginID,
	).Scan(&exists)
	return exists, err
}

// Plugin Sources (apt-style repos)

// AddSource registers a new plugin source for a community
func (s *Service) AddSource(ctx context.Context, communityID, addedBy uuid.UUID, name, url string) (*models.PluginSource, error) {
	src := &models.PluginSource{}
	err := s.db.QueryRow(ctx,
		`INSERT INTO plugin_sources (community_id, name, url, added_by)
		 VALUES ($1, $2, $3, $4)
		 RETURNING id, community_id, name, url, enabled, added_by, created_at`,
		communityID, name, url, addedBy,
	).Scan(&src.ID, &src.CommunityID, &src.Name, &src.URL, &src.Enabled, &src.AddedBy, &src.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("add source: %w", err)
	}
	return src, nil
}

// RemoveSource deletes a plugin source
func (s *Service) RemoveSource(ctx context.Context, communityID, sourceID uuid.UUID) error {
	tag, err := s.db.Exec(ctx,
		`DELETE FROM plugin_sources WHERE id = $1 AND community_id = $2`,
		sourceID, communityID,
	)
	if err != nil {
		return fmt.Errorf("remove source: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrSourceNotFound
	}
	return nil
}

// GetSources lists all plugin sources for a community
func (s *Service) GetSources(ctx context.Context, communityID uuid.UUID) ([]*models.PluginSource, error) {
	rows, err := s.db.Query(ctx,
		`SELECT id, community_id, name, url, enabled, added_by, created_at
		 FROM plugin_sources WHERE community_id = $1 ORDER BY created_at ASC`, communityID,
	)
	if err != nil {
		return nil, fmt.Errorf("get sources: %w", err)
	}
	defer rows.Close()

	sources := make([]*models.PluginSource, 0)
	for rows.Next() {
		src := &models.PluginSource{}
		if err := rows.Scan(&src.ID, &src.CommunityID, &src.Name, &src.URL, &src.Enabled, &src.AddedBy, &src.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan source: %w", err)
		}
		sources = append(sources, src)
	}
	return sources, rows.Err()
}

// FetchFromSource contacts a source URL and returns available plugins.
// Sources expose a simple JSON API that returns a list of plugin manifests.
func (s *Service) FetchFromSource(ctx context.Context, sourceURL string) ([]*models.Plugin, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, sourceURL+"/api/v1/plugins", nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, ErrFetchFailed
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, ErrFetchFailed
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 5*1024*1024)) // 5MB limit
	if err != nil {
		return nil, fmt.Errorf("read source response: %w", err)
	}

	var result struct {
		Data []*models.Plugin `json:"data"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("parse source response: %w", err)
	}

	return result.Data, nil
}

// SyncFromSource fetches plugins from a source and upserts them into the local DB
func (s *Service) SyncFromSource(ctx context.Context, sourceURL string) (int, error) {
	plugins, err := s.FetchFromSource(ctx, sourceURL)
	if err != nil {
		return 0, err
	}

	count := 0
	for _, p := range plugins {
		_, err := s.db.Exec(ctx,
			`INSERT INTO plugins (slug, name, description, author, version, homepage_url, source_url, icon_url,
			                      requested_permissions, manifest, source, is_verified)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
			 ON CONFLICT (slug) DO UPDATE SET
			   name = EXCLUDED.name,
			   description = EXCLUDED.description,
			   author = EXCLUDED.author,
			   version = EXCLUDED.version,
			   homepage_url = EXCLUDED.homepage_url,
			   source_url = EXCLUDED.source_url,
			   icon_url = EXCLUDED.icon_url,
			   requested_permissions = EXCLUDED.requested_permissions,
			   manifest = EXCLUDED.manifest,
			   is_verified = EXCLUDED.is_verified,
			   updated_at = NOW()
			 WHERE plugins.built_in = FALSE`,
			p.Slug, p.Name, p.Description, p.Author, p.Version, p.HomepageURL, p.SourceURL, p.IconURL,
			p.RequestedPermissions, p.Manifest, sourceURL, p.IsVerified,
		)
		if err != nil {
			log.Warn().Err(err).Str("slug", p.Slug).Msg("Failed to sync plugin from source")
			continue
		}
		count++
	}

	return count, nil
}

// EnsureBuiltInPluginsInstalled makes sure every community has the core plugin.
// Called at startup or when a new community is created.
func (s *Service) EnsureBuiltInPluginsInstalled(ctx context.Context, communityID, ownerID uuid.UUID) error {
	_, err := s.db.Exec(ctx,
		`INSERT INTO community_plugins (community_id, plugin_id, enabled, granted_permissions, installed_by)
		 SELECT $1, p.id, TRUE, p.requested_permissions, $2
		 FROM plugins p WHERE p.built_in = TRUE
		 ON CONFLICT (community_id, plugin_id) DO NOTHING`,
		communityID, ownerID,
	)
	if err != nil {
		return fmt.Errorf("auto-install built-in plugins: %w", err)
	}
	return nil
}

// GetPluginAuditLog returns recent plugin actions for a community
func (s *Service) GetPluginAuditLog(ctx context.Context, communityID uuid.UUID, limit int) ([]*models.PluginAuditEntry, error) {
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	rows, err := s.db.Query(ctx,
		`SELECT id, community_id, plugin_id, actor_id, action, details, created_at
		 FROM plugin_audit_log
		 WHERE community_id = $1
		 ORDER BY created_at DESC
		 LIMIT $2`, communityID, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("get plugin audit log: %w", err)
	}
	defer rows.Close()

	var entries []*models.PluginAuditEntry
	for rows.Next() {
		e := &models.PluginAuditEntry{}
		if err := rows.Scan(&e.ID, &e.CommunityID, &e.PluginID, &e.ActorID, &e.Action, &e.Details, &e.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan audit entry: %w", err)
		}
		entries = append(entries, e)
	}
	return entries, rows.Err()
}

// registerPluginChannelTypes takes a plugin's manifest and registers any channel types it declares
func (s *Service) registerPluginChannelTypes(ctx context.Context, plugin *models.Plugin) {
	manifest, err := plugin.ParsedManifest()
	if err != nil {
		log.Warn().Err(err).Str("plugin", plugin.Slug).Msg("Failed to parse plugin manifest for channel types")
		return
	}

	for _, typeID := range manifest.ChannelTypes {
		// Skip if this type is already registered (e.g. built-in types)
		if s.channelRegistry.Exists(typeID) {
			continue
		}

		pluginIDStr := plugin.ID.String()
		def := &models.ChannelTypeDefinition{
			ID:           typeID,
			Name:         typeID, // plugins will provide better names via their frontend bundle
			Description:  fmt.Sprintf("Provided by %s", plugin.Name),
			Icon:         "puzzle",
			Capabilities: models.CapMessages,
			BuiltIn:      false,
			PluginID:     &pluginIDStr,
		}

		if err := s.channelRegistry.Register(ctx, def); err != nil {
			log.Warn().Err(err).Str("type", typeID).Str("plugin", plugin.Slug).Msg("Failed to register plugin channel type")
		}
	}
}

// logAction writes an entry to the plugin audit log
func (s *Service) logAction(ctx context.Context, communityID, pluginID, actorID uuid.UUID, action string, details map[string]any) {
	detailsJSON, _ := json.Marshal(details)
	if detailsJSON == nil {
		detailsJSON = []byte("{}")
	}

	_, err := s.db.Exec(ctx,
		`INSERT INTO plugin_audit_log (community_id, plugin_id, actor_id, action, details)
		 VALUES ($1, $2, $3, $4, $5)`,
		communityID, pluginID, actorID, action, detailsJSON,
	)
	if err != nil {
		log.Warn().Err(err).Str("action", action).Msg("Failed to log plugin action")
	}
}
