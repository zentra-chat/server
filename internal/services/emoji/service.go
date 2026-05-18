package emoji

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"mime/multipart"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/minio/minio-go/v7"
	"github.com/nfnt/resize"
	"github.com/rs/zerolog/log"
	"github.com/zentra/server/internal/models"
)

var (
	ErrEmojiNotFound     = errors.New("emoji not found")
	ErrNameTaken         = errors.New("an emoji with that name already exists in this community")
	ErrInvalidName       = errors.New("emoji name must be 2-32 alphanumeric or underscore characters")
	ErrInvalidImage      = errors.New("emoji must be a PNG, JPEG, GIF, or WebP image")
	ErrImageTooLarge     = errors.New("emoji image must be under 256KB")
	ErrTooManyEmojis     = errors.New("community has reached the emoji limit")
	ErrInsufficientPerms = errors.New("insufficient permissions")
	ErrNotMember         = errors.New("user is not a member of this community")
)

const (
	MaxEmojiSize          = 256 * 1024 // 256KB
	MaxEmojisPerCommunity = 200
	MaxEmojiDimension     = 128
)

var emojiNameRegex = regexp.MustCompile(`^[a-zA-Z0-9_]{2,32}$`)

var allowedEmojiTypes = map[string]bool{
	"image/png":  true,
	"image/jpeg": true,
	"image/gif":  true,
	"image/webp": true,
}

// CommunityServiceInterface is the subset of community.Service we depend on
type CommunityServiceInterface interface {
	GetMemberPermissions(ctx context.Context, communityID, userID uuid.UUID) (int64, error)
	IsMember(ctx context.Context, communityID, userID uuid.UUID) bool
}

type Service struct {
	db               *pgxpool.Pool
	minio            *minio.Client
	bucketCommunity  string
	cdnBaseURL       string
	communityService CommunityServiceInterface
}

func NewService(db *pgxpool.Pool, minioClient *minio.Client, bucketCommunity, cdnBaseURL string, communityService CommunityServiceInterface) *Service {
	return &Service{
		db:               db,
		minio:            minioClient,
		bucketCommunity:  bucketCommunity,
		cdnBaseURL:       cdnBaseURL,
		communityService: communityService,
	}
}

// CreateEmoji uploads a custom emoji image and stores the record
func (s *Service) CreateEmoji(ctx context.Context, communityID, uploaderID uuid.UUID, name string, file multipart.File, header *multipart.FileHeader) (*models.CustomEmoji, error) {
	// Check permissions
	if err := s.requireManageEmojis(ctx, communityID, uploaderID); err != nil {
		return nil, err
	}

	// Validate the name
	name = strings.TrimSpace(name)
	if !emojiNameRegex.MatchString(name) {
		return nil, ErrInvalidName
	}

	// Check community emoji count
	var count int
	err := s.db.QueryRow(ctx, `SELECT COUNT(*) FROM custom_emojis WHERE community_id = $1`, communityID).Scan(&count)
	if err != nil {
		return nil, fmt.Errorf("failed to count emojis: %w", err)
	}
	if count >= MaxEmojisPerCommunity {
		return nil, ErrTooManyEmojis
	}

	// Check for duplicate name within community
	var exists bool
	err = s.db.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM custom_emojis WHERE community_id = $1 AND LOWER(name) = LOWER($2))`,
		communityID, name,
	).Scan(&exists)
	if err != nil {
		return nil, fmt.Errorf("failed to check emoji name: %w", err)
	}
	if exists {
		return nil, ErrNameTaken
	}

	// Validate image type and size
	contentType := header.Header.Get("Content-Type")
	if !allowedEmojiTypes[contentType] {
		return nil, ErrInvalidImage
	}
	if header.Size > MaxEmojiSize {
		return nil, ErrImageTooLarge
	}

	fileData, err := io.ReadAll(file)
	if err != nil {
		return nil, fmt.Errorf("failed to read file: %w", err)
	}

	animated := contentType == "image/gif"
	emojiID := uuid.New()

	// Compress and resize the emoji to save space
	processedData, processedType, ext := s.processEmojiImage(fileData, contentType, header.Filename)

	objectName := fmt.Sprintf("emojis/%s/%s%s", communityID.String(), emojiID.String(), ext)

	_, err = s.minio.PutObject(ctx, s.bucketCommunity, objectName, bytes.NewReader(processedData), int64(len(processedData)),
		minio.PutObjectOptions{ContentType: processedType})
	if err != nil {
		return nil, fmt.Errorf("failed to upload emoji: %w", err)
	}

	imageURL := fmt.Sprintf("%s/%s/%s", s.cdnBaseURL, s.bucketCommunity, objectName)

	emoji := &models.CustomEmoji{
		ID:          emojiID,
		CommunityID: communityID,
		Name:        name,
		ImageURL:    imageURL,
		UploaderID:  uploaderID,
		Animated:    animated,
		CreatedAt:   time.Now(),
		UpdatedAt:   time.Now(),
	}

	_, err = s.db.Exec(ctx,
		`INSERT INTO custom_emojis (id, community_id, name, image_url, uploader_id, animated, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		emoji.ID, emoji.CommunityID, emoji.Name, emoji.ImageURL, emoji.UploaderID, emoji.Animated, emoji.CreatedAt, emoji.UpdatedAt,
	)
	if err != nil {
		// Clean up the uploaded file if the DB insert fails
		_ = s.minio.RemoveObject(ctx, s.bucketCommunity, objectName, minio.RemoveObjectOptions{})
		return nil, fmt.Errorf("failed to save emoji: %w", err)
	}

	return emoji, nil
}

// UpdateEmoji renames an existing emoji
func (s *Service) UpdateEmoji(ctx context.Context, emojiID, userID uuid.UUID, newName string) (*models.CustomEmoji, error) {
	emoji, err := s.getEmoji(ctx, emojiID)
	if err != nil {
		return nil, err
	}

	if err := s.requireManageEmojis(ctx, emoji.CommunityID, userID); err != nil {
		return nil, err
	}

	newName = strings.TrimSpace(newName)
	if !emojiNameRegex.MatchString(newName) {
		return nil, ErrInvalidName
	}

	// Check for duplicate name (excluding current emoji)
	var exists bool
	err = s.db.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM custom_emojis WHERE community_id = $1 AND LOWER(name) = LOWER($2) AND id != $3)`,
		emoji.CommunityID, newName, emojiID,
	).Scan(&exists)
	if err != nil {
		return nil, fmt.Errorf("failed to check emoji name: %w", err)
	}
	if exists {
		return nil, ErrNameTaken
	}

	_, err = s.db.Exec(ctx,
		`UPDATE custom_emojis SET name = $1, updated_at = NOW() WHERE id = $2`,
		newName, emojiID,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to update emoji: %w", err)
	}

	emoji.Name = newName
	return emoji, nil
}

// DeleteEmoji removes an emoji and its image
func (s *Service) DeleteEmoji(ctx context.Context, emojiID, userID uuid.UUID) error {
	emoji, err := s.getEmoji(ctx, emojiID)
	if err != nil {
		return err
	}

	if err := s.requireManageEmojis(ctx, emoji.CommunityID, userID); err != nil {
		return err
	}

	// Remove the file from storage
	objectName := s.extractObjectName(emoji.ImageURL)
	if objectName != "" {
		_ = s.minio.RemoveObject(ctx, s.bucketCommunity, objectName, minio.RemoveObjectOptions{})
	}

	_, err = s.db.Exec(ctx, `DELETE FROM custom_emojis WHERE id = $1`, emojiID)
	if err != nil {
		return fmt.Errorf("failed to delete emoji: %w", err)
	}

	return nil
}

// GetCommunityEmojis returns all emojis for a given community
func (s *Service) GetCommunityEmojis(ctx context.Context, communityID, userID uuid.UUID) ([]models.CustomEmoji, error) {
	if !s.communityService.IsMember(ctx, communityID, userID) {
		return nil, ErrNotMember
	}

	rows, err := s.db.Query(ctx,
		`SELECT id, community_id, name, image_url, uploader_id, animated, created_at, updated_at
		FROM custom_emojis
		WHERE community_id = $1
		ORDER BY name ASC`,
		communityID,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch emojis: %w", err)
	}
	defer rows.Close()

	var emojis []models.CustomEmoji
	for rows.Next() {
		var e models.CustomEmoji
		if err := rows.Scan(&e.ID, &e.CommunityID, &e.Name, &e.ImageURL, &e.UploaderID, &e.Animated, &e.CreatedAt, &e.UpdatedAt); err != nil {
			return nil, fmt.Errorf("failed to scan emoji: %w", err)
		}
		emojis = append(emojis, e)
	}

	if err := rows.Err(); err != nil {
		log.Warn().Err(err).Msg("Failed to iterate emoji rows")
		return nil, err
	}

	if emojis == nil {
		emojis = []models.CustomEmoji{}
	}
	return emojis, nil
}

// GetAllAccessibleEmojis returns emojis from every community the user belongs to.
// This powers the "use anywhere" feature similar to Discord Nitro, but free for everyone.
func (s *Service) GetAllAccessibleEmojis(ctx context.Context, userID uuid.UUID) ([]models.CustomEmojiWithCommunity, error) {
	rows, err := s.db.Query(ctx,
		`SELECT e.id, e.community_id, e.name, e.image_url, e.uploader_id, e.animated, e.created_at, e.updated_at,
		        c.name AS community_name
		FROM custom_emojis e
		JOIN communities c ON c.id = e.community_id
		JOIN community_members cm ON cm.community_id = e.community_id AND cm.user_id = $1
		ORDER BY c.name ASC, e.name ASC`,
		userID,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch accessible emojis: %w", err)
	}
	defer rows.Close()

	var emojis []models.CustomEmojiWithCommunity
	for rows.Next() {
		var e models.CustomEmojiWithCommunity
		if err := rows.Scan(
			&e.ID, &e.CommunityID, &e.Name, &e.ImageURL, &e.UploaderID, &e.Animated, &e.CreatedAt, &e.UpdatedAt,
			&e.CommunityName,
		); err != nil {
			return nil, fmt.Errorf("failed to scan emoji: %w", err)
		}
		emojis = append(emojis, e)
	}

	if err := rows.Err(); err != nil {
		log.Warn().Err(err).Msg("Failed to iterate accessible emoji rows")
		return nil, err
	}

	if emojis == nil {
		emojis = []models.CustomEmojiWithCommunity{}
	}
	return emojis, nil
}

// ResolveEmoji looks up a single custom emoji by ID for rendering in messages
func (s *Service) ResolveEmoji(ctx context.Context, emojiID uuid.UUID) (*models.CustomEmoji, error) {
	return s.getEmoji(ctx, emojiID)
}

// --- internal helpers ---

func (s *Service) getEmoji(ctx context.Context, emojiID uuid.UUID) (*models.CustomEmoji, error) {
	var e models.CustomEmoji
	err := s.db.QueryRow(ctx,
		`SELECT id, community_id, name, image_url, uploader_id, animated, created_at, updated_at
		FROM custom_emojis WHERE id = $1`,
		emojiID,
	).Scan(&e.ID, &e.CommunityID, &e.Name, &e.ImageURL, &e.UploaderID, &e.Animated, &e.CreatedAt, &e.UpdatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrEmojiNotFound
		}
		return nil, fmt.Errorf("failed to fetch emoji: %w", err)
	}
	return &e, nil
}

func (s *Service) requireManageEmojis(ctx context.Context, communityID, userID uuid.UUID) error {
	perms, err := s.communityService.GetMemberPermissions(ctx, communityID, userID)
	if err != nil {
		return ErrNotMember
	}
	if !models.HasPermission(perms, models.PermissionManageEmojis) {
		return ErrInsufficientPerms
	}
	return nil
}

// extractObjectName strips the CDN prefix to get the MinIO object path
func (s *Service) extractObjectName(imageURL string) string {
	prefix := fmt.Sprintf("%s/%s/", s.cdnBaseURL, s.bucketCommunity)
	if strings.HasPrefix(imageURL, prefix) {
		return strings.TrimPrefix(imageURL, prefix)
	}
	return ""
}

// processEmojiImage resizes and compresses the emoji for storage.
// GIFs are kept as-is to preserve animation. Everything else gets
// downscaled to 128x128 max and re-encoded to save space.
func (s *Service) processEmojiImage(data []byte, contentType string, filename string) ([]byte, string, string) {
	// Don't touch GIFs -- we'd lose the animation
	if contentType == "image/gif" {
		return data, contentType, ".gif"
	}

	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		// Can't decode it, just store the original with proper extension
		ext := filepath.Ext(filename)
		if ext == "" {
			switch contentType {
			case "image/png":
				ext = ".png"
			case "image/jpeg":
				ext = ".jpg"
			case "image/webp":
				ext = ".webp"
			default:
				ext = ".png"
			}
		}
		return data, contentType, ext
	}

	bounds := img.Bounds()
	w, h := bounds.Dx(), bounds.Dy()

	// Only resize if it's bigger than our limit
	if w > MaxEmojiDimension || h > MaxEmojiDimension {
		img = resize.Thumbnail(uint(MaxEmojiDimension), uint(MaxEmojiDimension), img, resize.Lanczos3)
	}

	// PNG and WebP sources stay as PNG to keep transparency
	if contentType == "image/png" || contentType == "image/webp" {
		var buf bytes.Buffer
		if err := png.Encode(&buf, img); err != nil {
			return data, contentType, ".png"
		}
		// Only use the compressed version if it actually saved space
		if buf.Len() < len(data) {
			return buf.Bytes(), "image/png", ".png"
		}
		return data, "image/png", ".png"
	}

	// JPEG gets re-encoded at quality 85
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 85}); err != nil {
		return data, contentType, ".jpg"
	}
	if buf.Len() < len(data) {
		return buf.Bytes(), "image/jpeg", ".jpg"
	}
	return data, "image/jpeg", ".jpg"
}
