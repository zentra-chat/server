package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"os"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
	"golang.org/x/crypto/hkdf"

	"github.com/zentra/server/internal/services/messaging"
	"github.com/zentra/server/pkg/encryption"
)

func main() {
	_ = godotenv.Load()

	kekHex := os.Getenv("ENCRYPTION_KEY")
	if kekHex == "" {
		log.Fatal("ENCRYPTION_KEY is required")
	}
	kek, err := hex.DecodeString(kekHex)
	if err != nil {
		log.Fatalf("invalid ENCRYPTION_KEY hex: %v", err)
	}
	if len(kek) != 32 {
		log.Fatal("ENCRYPTION_KEY must decode to 32 bytes")
	}

	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		log.Fatal("DATABASE_URL is required")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		log.Fatalf("connect to DB: %v", err)
	}
	defer pool.Close()

	channelCipher := messaging.NewChannelCipher()
	dmCipher := messaging.NewDMCipher()

	// Migrate communities
	var communityCount, communityMsgCount int
	rows, err := pool.Query(ctx, `SELECT id FROM communities WHERE encrypted_dek IS NULL`)
	if err != nil {
		log.Fatalf("query communities: %v", err)
	}
	var communityIDs []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			log.Fatalf("scan community: %v", err)
		}
		communityIDs = append(communityIDs, id)
	}
	rows.Close()

	for _, communityID := range communityIDs {
		dek, err := encryption.GenerateDEK()
		if err != nil {
			log.Fatalf("generate DEK: %v", err)
		}
		wrapped, err := encryption.WrapKey(dek, kek)
		if err != nil {
			log.Fatalf("wrap DEK: %v", err)
		}

		_, err = pool.Exec(ctx, `UPDATE communities SET encrypted_dek = $1 WHERE id = $2`, wrapped, communityID)
		if err != nil {
			log.Fatalf("update community DEK: %v", err)
		}

		// Re-encrypt channel messages for this community
		channelRows, err := pool.Query(ctx, `SELECT id FROM channels WHERE community_id = $1`, communityID)
		if err != nil {
			log.Fatalf("query channels: %v", err)
		}
		var channelIDs []uuid.UUID
		for channelRows.Next() {
			var chID uuid.UUID
			if err := channelRows.Scan(&chID); err != nil {
				log.Fatalf("scan channel: %v", err)
			}
			channelIDs = append(channelIDs, chID)
		}
		channelRows.Close()

		for _, channelID := range channelIDs {
			count, err := reencryptChannelMessages(ctx, pool, channelCipher, kek, channelID, dek)
			if err != nil {
				log.Printf("ERROR re-encrypting channel %s: %v", channelID, err)
				continue
			}
			communityMsgCount += count
		}

		communityCount++
		fmt.Printf("Migrated community %s (%d channels, %d messages re-encrypted)\n", communityID, len(channelIDs), communityMsgCount)
	}

	// Migrate DM conversations
	var convCount, convMsgCount int
	convRows, err := pool.Query(ctx, `SELECT id FROM dm_conversations WHERE encrypted_dek IS NULL`)
	if err != nil {
		log.Fatalf("query conversations: %v", err)
	}
	var convIDs []uuid.UUID
	for convRows.Next() {
		var id uuid.UUID
		if err := convRows.Scan(&id); err != nil {
			log.Fatalf("scan conversation: %v", err)
		}
		convIDs = append(convIDs, id)
	}
	convRows.Close()

	for _, convID := range convIDs {
		dek, err := encryption.GenerateDEK()
		if err != nil {
			log.Fatalf("generate DEK: %v", err)
		}
		wrapped, err := encryption.WrapKey(dek, kek)
		if err != nil {
			log.Fatalf("wrap DEK: %v", err)
		}

		_, err = pool.Exec(ctx, `UPDATE dm_conversations SET encrypted_dek = $1 WHERE id = $2`, wrapped, convID)
		if err != nil {
			log.Fatalf("update conversation DEK: %v", err)
		}

		count, err := reencryptDMMessages(ctx, pool, dmCipher, kek, convID, dek)
		if err != nil {
			log.Printf("ERROR re-encrypting conversation %s: %v", convID, err)
			continue
		}
		convMsgCount += count
		convCount++
	}

	fmt.Printf("\nDone. Migrated %d communities (%d messages) and %d conversations (%d messages)\n",
		communityCount, communityMsgCount, convCount, convMsgCount)
}

// oldChannelKey replicates the previous HKDF-based per-channel key derivation.
func oldChannelKey(masterKey []byte, channelID uuid.UUID) []byte {
	r := hkdf.New(sha256.New, masterKey, channelID[:], []byte("zentra-channel-key"))
	key := make([]byte, 32)
	if _, err := io.ReadFull(r, key); err != nil {
		panic(err)
	}
	return key
}

// oldConversationKey replicates the previous HKDF-based per-conversation key derivation.
func oldConversationKey(masterKey []byte, conversationID uuid.UUID) []byte {
	r := hkdf.New(sha256.New, masterKey, conversationID[:], []byte("zentra-dm-key"))
	key := make([]byte, 32)
	if _, err := io.ReadFull(r, key); err != nil {
		panic(err)
	}
	return key
}

func reencryptChannelMessages(ctx context.Context, pool *pgxpool.Pool, cipher *messaging.ChannelCipher, kek []byte, channelID uuid.UUID, newDEK []byte) (int, error) {
	// Try HKDF-derived key first, fall back to raw master key for
	// messages encrypted before per-channel derivation was introduced.
	hkdfKey := oldChannelKey(kek, channelID)

	rows, err := pool.Query(ctx,
		`SELECT id, encrypted_content FROM messages WHERE channel_id = $1 AND deleted_at IS NULL`,
		channelID,
	)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	var count int
	for rows.Next() {
		var msgID uuid.UUID
		var encContent []byte
		if err := rows.Scan(&msgID, &encContent); err != nil {
			return count, err
		}
		if encContent == nil {
			continue
		}

		plaintext, err := cipher.Decrypt(encContent, nil, hkdfKey)
		if err != nil {
			plaintext, err = cipher.Decrypt(encContent, nil, kek)
			if err != nil {
				log.Printf("WARN: decrypt failed for message %s: %v (skipping)", msgID, err)
				continue
			}
		}

		newEnc, _, err := cipher.Encrypt(plaintext, newDEK)
		if err != nil {
			return count, fmt.Errorf("re-encrypt message %s: %w", msgID, err)
		}

		_, err = pool.Exec(ctx, `UPDATE messages SET encrypted_content = $1 WHERE id = $2`, newEnc, msgID)
		if err != nil {
			return count, fmt.Errorf("update message %s: %w", msgID, err)
		}
		count++
	}
	return count, rows.Err()
}

func reencryptDMMessages(ctx context.Context, pool *pgxpool.Pool, cipher *messaging.DMCipher, kek []byte, conversationID uuid.UUID, newDEK []byte) (int, error) {
	hkdfKey := oldConversationKey(kek, conversationID)

	rows, err := pool.Query(ctx,
		`SELECT id, encrypted_content, nonce FROM direct_messages WHERE conversation_id = $1 AND deleted_at IS NULL`,
		conversationID,
	)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	var count int
	for rows.Next() {
		var msgID uuid.UUID
		var encContent, nonce []byte
		if err := rows.Scan(&msgID, &encContent, &nonce); err != nil {
			return count, err
		}
		if encContent == nil {
			continue
		}

		plaintext, err := cipher.Decrypt(encContent, nonce, hkdfKey)
		if err != nil {
			plaintext, err = cipher.Decrypt(encContent, nonce, kek)
			if err != nil {
				log.Printf("WARN: decrypt failed for DM %s: %v (skipping)", msgID, err)
				continue
			}
		}

		newEnc, newNonce, err := cipher.Encrypt(plaintext, newDEK)
		if err != nil {
			return count, fmt.Errorf("re-encrypt DM %s: %w", msgID, err)
		}

		_, err = pool.Exec(ctx,
			`UPDATE direct_messages SET encrypted_content = $1, nonce = $2 WHERE id = $3`,
			newEnc, newNonce, msgID,
		)
		if err != nil {
			return count, fmt.Errorf("update DM %s: %w", msgID, err)
		}
		count++
	}
	return count, rows.Err()
}
