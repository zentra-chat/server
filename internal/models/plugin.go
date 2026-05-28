package models

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// Plugin permission flags - what a plugin is allowed to do on a server.
// Server owners grant a subset of these when installing a plugin.
const (
	PluginPermReadMessages    int64 = 1 << iota // can read messages in channels
	PluginPermSendMessages                      // can send messages on behalf of the plugin
	PluginPermManageMessages                    // can delete/edit messages
	PluginPermReadMembers                       // can see member list and profiles
	PluginPermManageMembers                     // can kick/ban (dangerous)
	PluginPermReadChannels                      // can see channel list and info
	PluginPermManageChannels                    // can create/edit/delete channels
	PluginPermAddChannelTypes                   // can register custom channel types
	PluginPermAddCommands                       // can register slash commands
	PluginPermServerInfo                        // can read community metadata
	PluginPermWebhooks                          // can create and use webhooks
	PluginPermReactToMessages                   // can add reactions
)

// ArgDef describes a single argument for a slash command.
type ArgDef struct {
	Name        string `json:"name"`
	Type        string `json:"type"`
	Required    bool   `json:"required"`
	Description string `json:"description,omitempty"`
}

// CommandDef describes a slash command provided by a plugin.
type CommandDef struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Args        []ArgDef `json:"args,omitempty"`
	Permissions int64    `json:"permissions,omitempty"`
}

// ChannelTypeDef describes a custom channel type that a plugin registers.
type ChannelTypeDef struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Icon        string `json:"icon"`
	// ViewFrame is the custom React component to render for this channel type
	ViewFrame string `json:"viewFrame,omitempty"`
}

// EventTrigger describes what events a plugin subscribes to.
type EventTrigger struct {
	Event string `json:"event"`
	// Filter is an optional JSONPath expression to match event payloads
	Filter string `json:"filter,omitempty"`
}

// PluginManifest is the structured content inside the manifest JSONB column.
// It declares everything the plugin provides: channel types, commands, hooks, etc.
type PluginManifest struct {
	ChannelTypes []ChannelTypeDef `json:"channelTypes,omitempty"`
	Commands     []CommandDef     `json:"commands,omitempty"`
	Triggers     []EventTrigger   `json:"triggers,omitempty"`
	Hooks        []string         `json:"hooks,omitempty"`
	// URL to the frontend bundle (JS) that registers custom components
	FrontendBundle string `json:"frontendBundle,omitempty"`
	// WASM runtime module reference
	WASMModule string `json:"wasmModule,omitempty"`
}

// Plugin represents a plugin available for installation
type Plugin struct {
	ID                   uuid.UUID       `json:"id" db:"id"`
	Slug                 string          `json:"slug" db:"slug"`
	Name                 string          `json:"name" db:"name"`
	Description          string          `json:"description" db:"description"`
	Author               string          `json:"author" db:"author"`
	Version              string          `json:"version" db:"version"`
	HomepageURL          *string         `json:"homepageUrl,omitempty" db:"homepage_url"`
	SourceURL            *string         `json:"sourceUrl,omitempty" db:"source_url"`
	IconURL              *string         `json:"iconUrl,omitempty" db:"icon_url"`
	RequestedPermissions int64           `json:"requestedPermissions" db:"requested_permissions"`
	Manifest             json.RawMessage `json:"manifest" db:"manifest"`
	BuiltIn              bool            `json:"builtIn" db:"built_in"`
	Source               string          `json:"source" db:"source"`
	IsVerified           bool            `json:"isVerified" db:"is_verified"`
	WASMObjectKey        *string         `json:"wasmObjectKey,omitempty" db:"wasm_object_key"`
	UIBundleObjectKey    *string         `json:"uiBundleObjectKey,omitempty" db:"ui_bundle_object_key"`
	CreatedAt            time.Time       `json:"createdAt" db:"created_at"`
	UpdatedAt            time.Time       `json:"updatedAt" db:"updated_at"`
}

// ParsedManifest returns the structured manifest from the raw JSON
func (p *Plugin) ParsedManifest() (*PluginManifest, error) {
	var m PluginManifest
	if err := json.Unmarshal(p.Manifest, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// CommunityPlugin represents a plugin installed on a specific community/server
type CommunityPlugin struct {
	ID                 uuid.UUID       `json:"id" db:"id"`
	CommunityID        uuid.UUID       `json:"communityId" db:"community_id"`
	PluginID           uuid.UUID       `json:"pluginId" db:"plugin_id"`
	Enabled            bool            `json:"enabled" db:"enabled"`
	GrantedPermissions int64           `json:"grantedPermissions" db:"granted_permissions"`
	Config             json.RawMessage `json:"config" db:"config"`
	InstalledBy        uuid.UUID       `json:"installedBy" db:"installed_by"`
	InstalledAt        time.Time       `json:"installedAt" db:"installed_at"`
	UpdatedAt          time.Time       `json:"updatedAt" db:"updated_at"`
	// Joined from plugins table
	Plugin *Plugin `json:"plugin,omitempty"`
}

// HasPermission checks if this installation has a specific permission granted
func (cp *CommunityPlugin) HasPermission(perm int64) bool {
	return cp.GrantedPermissions&perm != 0
}

// PluginSource is an apt-style source repository
type PluginSource struct {
	ID          uuid.UUID `json:"id" db:"id"`
	CommunityID uuid.UUID `json:"communityId" db:"community_id"`
	Name        string    `json:"name" db:"name"`
	URL         string    `json:"url" db:"url"`
	Enabled     bool      `json:"enabled" db:"enabled"`
	AddedBy     uuid.UUID `json:"addedBy" db:"added_by"`
	CreatedAt   time.Time `json:"createdAt" db:"created_at"`
}

// PluginAuditEntry tracks plugin-related actions for accountability
type PluginAuditEntry struct {
	ID          uuid.UUID       `json:"id" db:"id"`
	CommunityID uuid.UUID       `json:"communityId" db:"community_id"`
	PluginID    uuid.UUID       `json:"pluginId" db:"plugin_id"`
	ActorID     uuid.UUID       `json:"actorId" db:"actor_id"`
	Action      string          `json:"action" db:"action"`
	Details     json.RawMessage `json:"details" db:"details"`
	CreatedAt   time.Time       `json:"createdAt" db:"created_at"`
}
