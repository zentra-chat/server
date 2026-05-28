package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"github.com/rs/zerolog/log"
)

var sentinelUUID = uuid.MustParse("00000000-0000-0000-0000-000000000000")

type StateStore struct {
	db    *pgxpool.Pool
	redis *redis.Client
}

func NewStateStore(db *pgxpool.Pool, redis *redis.Client) *StateStore {
	return &StateStore{db: db, redis: redis}
}

func (s *StateStore) redisKey(communityID, pluginID uuid.UUID, key string, channelID uuid.UUID) string {
	if channelID == sentinelUUID {
		return fmt.Sprintf("plugin_state:%s:%s:%s", communityID, pluginID, key)
	}
	return fmt.Sprintf("plugin_state:%s:%s:%s:%s", communityID, pluginID, key, channelID)
}

func (s *StateStore) Get(ctx context.Context, communityID, pluginID uuid.UUID, key string) (json.RawMessage, error) {
	return s.getScoped(ctx, communityID, pluginID, key, sentinelUUID)
}

func (s *StateStore) GetChannel(ctx context.Context, communityID, pluginID uuid.UUID, channelID uuid.UUID, key string) (json.RawMessage, error) {
	return s.getScoped(ctx, communityID, pluginID, key, channelID)
}

func (s *StateStore) getScoped(ctx context.Context, communityID, pluginID uuid.UUID, key string, channelID uuid.UUID) (json.RawMessage, error) {
	rk := s.redisKey(communityID, pluginID, key, channelID)

	val, err := s.redis.Get(ctx, rk).Bytes()
	if err == nil {
		return val, nil
	}
	if err != redis.Nil {
		log.Warn().Err(err).Msg("plugin state redis read failed, falling back to db")
	}

	var value json.RawMessage
	err = s.db.QueryRow(ctx,
		`SELECT value FROM plugin_state
		 WHERE community_id = $1 AND plugin_id = $2 AND key = $3 AND channel_id = $4`,
		communityID, pluginID, key, channelID,
	).Scan(&value)
	if err != nil {
		return nil, err
	}

	s.redis.Set(ctx, rk, value, 5*time.Minute)
	return value, nil
}

func (s *StateStore) Set(ctx context.Context, communityID, pluginID uuid.UUID, key string, value json.RawMessage) error {
	return s.setScoped(ctx, communityID, pluginID, key, value, sentinelUUID)
}

func (s *StateStore) SetChannel(ctx context.Context, communityID, pluginID uuid.UUID, channelID uuid.UUID, key string, value json.RawMessage) error {
	return s.setScoped(ctx, communityID, pluginID, key, value, channelID)
}

func (s *StateStore) setScoped(ctx context.Context, communityID, pluginID uuid.UUID, key string, value json.RawMessage, channelID uuid.UUID) error {
	rk := s.redisKey(communityID, pluginID, key, channelID)

	_, err := s.db.Exec(ctx,
		`INSERT INTO plugin_state (community_id, plugin_id, key, value, channel_id, updated_at)
		 VALUES ($1, $2, $3, $4, $5, NOW())
		 ON CONFLICT (community_id, plugin_id, key, channel_id)
		 DO UPDATE SET value = $4, updated_at = NOW()`,
		communityID, pluginID, key, value, channelID,
	)
	if err != nil {
		return fmt.Errorf("plugin state set: %w", err)
	}

	s.redis.Set(ctx, rk, value, 5*time.Minute)
	return nil
}

func (s *StateStore) Delete(ctx context.Context, communityID, pluginID uuid.UUID, key string) error {
	return s.deleteScoped(ctx, communityID, pluginID, key, sentinelUUID)
}

func (s *StateStore) DeleteChannel(ctx context.Context, communityID, pluginID uuid.UUID, channelID uuid.UUID, key string) error {
	return s.deleteScoped(ctx, communityID, pluginID, key, channelID)
}

func (s *StateStore) deleteScoped(ctx context.Context, communityID, pluginID uuid.UUID, key string, channelID uuid.UUID) error {
	rk := s.redisKey(communityID, pluginID, key, channelID)

	_, err := s.db.Exec(ctx,
		`DELETE FROM plugin_state
		 WHERE community_id = $1 AND plugin_id = $2 AND key = $3 AND channel_id = $4`,
		communityID, pluginID, key, channelID,
	)
	if err != nil {
		return fmt.Errorf("plugin state delete: %w", err)
	}

	s.redis.Del(ctx, rk)
	return nil
}
