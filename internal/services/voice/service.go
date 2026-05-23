package voice

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog/log"
	"github.com/zentra/server/internal/models"
	"github.com/zentra/server/internal/services/channel"
	"github.com/zentra/server/internal/services/user"
)

var (
	ErrNotInVoiceChannel = errors.New("not in a voice channel")
	ErrAlreadyInChannel  = errors.New("already in a voice channel")
	ErrNotVoiceChannel   = errors.New("channel is not a voice channel")
	ErrInsufficientPerms = errors.New("insufficient permissions")
)

// Hub defines the interface for WebSocket broadcasting (avoids circular imports)
type Hub interface {
	Broadcast(channelID string, event interface{}, excludeClientID *uuid.UUID)
	SendToUser(userID uuid.UUID, event interface{})
}

type Service struct {
	db             *pgxpool.Pool
	channelService *channel.Service
	userService    *user.Service
}

func NewService(db *pgxpool.Pool, channelService *channel.Service, userService *user.Service) *Service {
	return &Service{
		db:             db,
		channelService: channelService,
		userService:    userService,
	}
}

// JoinChannel adds a user to a voice channel
func (s *Service) JoinChannel(ctx context.Context, channelID, userID uuid.UUID) (*models.VoiceState, error) {
	// Verify it's a voice channel
	ch, err := s.channelService.GetChannel(ctx, channelID)
	if err != nil {
		return nil, err
	}
	if ch.Type != models.ChannelTypeVoice {
		return nil, ErrNotVoiceChannel
	}

	// Check if user can access the channel
	if !s.channelService.CanAccessChannel(ctx, channelID, userID) {
		return nil, ErrInsufficientPerms
	}

	state := &models.VoiceState{
		ID:              uuid.New(),
		ChannelID:       channelID,
		UserID:          userID,
		IsMuted:         false,
		IsDeafened:      false,
		IsSelfMuted:     false,
		IsSelfDeaf:      false,
		IsScreenSharing: false,
		IsWebcamOn:      false,
		JoinedAt:        time.Now(),
	}

	tx, err := s.db.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	// Serialize join attempts per user to avoid race conditions that could place
	// a user in multiple channels when rapidly switching.
	_, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, userID.String())
	if err != nil {
		return nil, err
	}

	_, err = tx.Exec(ctx, `DELETE FROM voice_states WHERE user_id = $1`, userID)
	if err != nil {
		return nil, err
	}

	_, err = tx.Exec(ctx,
		`INSERT INTO voice_states (id, channel_id, user_id, is_muted, is_deafened, is_self_muted, is_self_deafened, is_screen_sharing, is_webcam_on, joined_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		ON CONFLICT (channel_id, user_id) DO UPDATE SET joined_at = $10`,
		state.ID, state.ChannelID, state.UserID, state.IsMuted, state.IsDeafened,
		state.IsSelfMuted, state.IsSelfDeaf, state.IsScreenSharing, state.IsWebcamOn, state.JoinedAt,
	)
	if err != nil {
		return nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}

	return state, nil
}

// LeaveChannel removes a user from a voice channel
func (s *Service) LeaveChannel(ctx context.Context, channelID, userID uuid.UUID) error {
	result, err := s.db.Exec(ctx,
		`DELETE FROM voice_states WHERE channel_id = $1 AND user_id = $2`,
		channelID, userID,
	)
	if err != nil {
		return err
	}
	if result.RowsAffected() == 0 {
		return ErrNotInVoiceChannel
	}
	return nil
}

// leaveAllChannels removes a user from all voice channels
func (s *Service) leaveAllChannels(ctx context.Context, userID uuid.UUID) error {
	_, err := s.db.Exec(ctx,
		`DELETE FROM voice_states WHERE user_id = $1`,
		userID,
	)
	return err
}

// DisconnectUser removes a user from all voice channels (called on WebSocket disconnect)
func (s *Service) DisconnectUser(ctx context.Context, userID uuid.UUID) ([]uuid.UUID, error) {
	// Get channels user was in before disconnecting
	rows, err := s.db.Query(ctx,
		`SELECT channel_id FROM voice_states WHERE user_id = $1`,
		userID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var channelIDs []uuid.UUID
	for rows.Next() {
		var channelID uuid.UUID
		if err := rows.Scan(&channelID); err != nil {
			continue
		}
		channelIDs = append(channelIDs, channelID)
	}

	if err := rows.Err(); err != nil {
		log.Warn().Err(err).Msg("Error iterating over voice channels for disconnect")
	}

	// Remove from all voice channels
	err = s.leaveAllChannels(ctx, userID)
	return channelIDs, err
}

// UpdateVoiceState updates a user's voice state (mute/deafen/screen/webcam)
func (s *Service) UpdateVoiceState(ctx context.Context, channelID, userID uuid.UUID, isSelfMuted, isSelfDeafened, isScreenSharing, isWebcamOn *bool) (*models.VoiceState, error) {
	state, err := s.GetUserVoiceState(ctx, channelID, userID)
	if err != nil {
		return nil, ErrNotInVoiceChannel
	}

	if isSelfMuted != nil {
		state.IsSelfMuted = *isSelfMuted
	}
	if isSelfDeafened != nil {
		state.IsSelfDeaf = *isSelfDeafened
	}
	if isScreenSharing != nil {
		state.IsScreenSharing = *isScreenSharing
	}
	if isWebcamOn != nil {
		state.IsWebcamOn = *isWebcamOn
	}

	_, err = s.db.Exec(ctx,
		`UPDATE voice_states SET is_self_muted = $3, is_self_deafened = $4, is_screen_sharing = $5, is_webcam_on = $6 WHERE channel_id = $1 AND user_id = $2`,
		channelID, userID, state.IsSelfMuted, state.IsSelfDeaf, state.IsScreenSharing, state.IsWebcamOn,
	)
	if err != nil {
		return nil, err
	}

	return state, nil
}

// ServerMuteUser allows a moderator to mute another user
func (s *Service) ServerMuteUser(ctx context.Context, channelID, targetUserID, actorUserID uuid.UUID, muted bool) (*models.VoiceState, error) {
	ch, err := s.channelService.GetChannel(ctx, channelID)
	if err != nil {
		return nil, err
	}

	// Check if actor has permission to mute others
	if !s.channelService.CanManageMessages(ctx, channelID, actorUserID) {
		_ = ch // suppress unused warning
		return nil, ErrInsufficientPerms
	}

	state, err := s.GetUserVoiceState(ctx, channelID, targetUserID)
	if err != nil {
		return nil, ErrNotInVoiceChannel
	}

	state.IsMuted = muted
	_, err = s.db.Exec(ctx,
		`UPDATE voice_states SET is_muted = $3 WHERE channel_id = $1 AND user_id = $2`,
		channelID, targetUserID, muted,
	)
	if err != nil {
		return nil, err
	}

	return state, nil
}

// GetChannelVoiceStates returns all voice states for a channel with user info
func (s *Service) GetChannelVoiceStates(ctx context.Context, channelID uuid.UUID) ([]*models.VoiceStateWithUser, error) {
	rows, err := s.db.Query(ctx,
		`SELECT vs.id, vs.channel_id, vs.user_id, vs.is_muted, vs.is_deafened, vs.is_self_muted, vs.is_self_deafened, vs.is_screen_sharing, vs.is_webcam_on, vs.joined_at,
			u.id, u.username, u.display_name, u.avatar_url, u.status
		FROM voice_states vs
		JOIN users u ON u.id = vs.user_id
		WHERE vs.channel_id = $1
		ORDER BY vs.joined_at ASC`,
		channelID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var states []*models.VoiceStateWithUser
	for rows.Next() {
		vs := &models.VoiceStateWithUser{
			User: &models.User{},
		}
		err := rows.Scan(
			&vs.ID, &vs.ChannelID, &vs.UserID, &vs.IsMuted, &vs.IsDeafened,
			&vs.IsSelfMuted, &vs.IsSelfDeaf, &vs.IsScreenSharing, &vs.IsWebcamOn, &vs.JoinedAt,
			&vs.User.ID, &vs.User.Username, &vs.User.DisplayName, &vs.User.AvatarURL, &vs.User.Status,
		)
		if err != nil {
			log.Error().Err(err).Msg("Failed to scan voice state")
			continue
		}
		states = append(states, vs)
	}

	if err := rows.Err(); err != nil {
		log.Warn().Err(err).Msg("Error iterating over voice states")
		return nil, err
	}

	return states, nil
}

// GetUserVoiceState gets a user's voice state in a specific channel
func (s *Service) GetUserVoiceState(ctx context.Context, channelID, userID uuid.UUID) (*models.VoiceState, error) {
	state := &models.VoiceState{}
	err := s.db.QueryRow(ctx,
		`SELECT id, channel_id, user_id, is_muted, is_deafened, is_self_muted, is_self_deafened, is_screen_sharing, is_webcam_on, joined_at
		FROM voice_states WHERE channel_id = $1 AND user_id = $2`,
		channelID, userID,
	).Scan(
		&state.ID, &state.ChannelID, &state.UserID, &state.IsMuted, &state.IsDeafened,
		&state.IsSelfMuted, &state.IsSelfDeaf, &state.IsScreenSharing, &state.IsWebcamOn, &state.JoinedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotInVoiceChannel
		}
		return nil, err
	}
	return state, nil
}

// GetUserCurrentVoiceChannel gets which voice channel a user is currently in
func (s *Service) GetUserCurrentVoiceChannel(ctx context.Context, userID uuid.UUID) (*models.VoiceState, error) {
	state := &models.VoiceState{}
	err := s.db.QueryRow(ctx,
		`SELECT id, channel_id, user_id, is_muted, is_deafened, is_self_muted, is_self_deafened, is_screen_sharing, is_webcam_on, joined_at
		FROM voice_states WHERE user_id = $1 LIMIT 1`,
		userID,
	).Scan(
		&state.ID, &state.ChannelID, &state.UserID, &state.IsMuted, &state.IsDeafened,
		&state.IsSelfMuted, &state.IsSelfDeaf, &state.IsScreenSharing, &state.IsWebcamOn, &state.JoinedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotInVoiceChannel
		}
		return nil, err
	}
	return state, nil
}
