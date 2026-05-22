package media

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	"image/jpeg"
	_ "image/png"
	"io"
	"mime/multipart"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/minio/minio-go/v7"
	"github.com/nfnt/resize"
	"github.com/rs/zerolog/log"
	"github.com/zentra/server/internal/models"
	"github.com/zentra/server/internal/services/community"
)

var (
	ErrFileTooLarge       = errors.New("file too large")
	ErrInvalidFileType    = errors.New("invalid file type")
	ErrUploadFailed       = errors.New("upload failed")
	ErrAttachmentNotFound = errors.New("attachment not found")
	ErrNotParticipant     = errors.New("not a participant")
	ErrAccessDenied       = errors.New("access denied")
)

// File size limits
const (
	MaxImageSize       = 10 * 1024 * 1024 // 10MB
	MaxFileSize        = 50 * 1024 * 1024 // 50MB
	MaxAvatarSize      = 5 * 1024 * 1024  // 5MB
	ThumbnailMaxWidth  = 400
	ThumbnailMaxHeight = 300
)

// Used to decide whether to generate thumbnails for image uploads
var AllowedImageTypes = map[string]bool{
	"image/jpeg": true,
	"image/png":  true,
	"image/gif":  true,
	"image/webp": true,
}

type Service struct {
	db                *pgxpool.Pool
	minio             *minio.Client
	bucketAttachments string
	bucketAvatars     string
	bucketCommunity   string
	cdnBaseURL        string
	communityService  *community.Service
}

func NewService(db *pgxpool.Pool, minioClient *minio.Client, buckets [3]string, cdnBaseURL string, communityService *community.Service) *Service {
	return &Service{
		db:                db,
		minio:             minioClient,
		bucketAttachments: buckets[0],
		bucketAvatars:     buckets[1],
		bucketCommunity:   buckets[2],
		cdnBaseURL:        cdnBaseURL,
		communityService:  communityService,
	}
}

type UploadResult struct {
	ID           uuid.UUID `json:"id"`
	Filename     string    `json:"filename"`
	ContentType  string    `json:"contentType"`
	Size         int64     `json:"size"`
	URL          string    `json:"url"`
	ThumbnailURL *string   `json:"thumbnailUrl,omitempty"`
}

// UploadAttachment handles file uploads for message attachments
func (s *Service) UploadAttachment(ctx context.Context, userID, channelID uuid.UUID, file multipart.File, header *multipart.FileHeader) (*UploadResult, error) {
	contentType := header.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	// Validate file size
	if header.Size > MaxFileSize {
		return nil, ErrFileTooLarge
	}

	// Get community ID from channel
	var communityID uuid.UUID
	err := s.db.QueryRow(ctx, "SELECT community_id FROM channels WHERE id = $1", channelID).Scan(&communityID)
	if err != nil {
		return nil, fmt.Errorf("failed to get community for channel: %w", err)
	}

	// Read file content
	fileData, err := io.ReadAll(file)
	if err != nil {
		return nil, fmt.Errorf("failed to read file: %w", err)
	}

	// Generate unique filename with organized path: community/channel/filename
	ext := filepath.Ext(header.Filename)
	attachmentID := uuid.New()
	objectName := fmt.Sprintf("%s/%s/%s%s", communityID.String(), channelID.String(), attachmentID.String(), ext)

	// Upload to MinIO
	_, err = s.minio.PutObject(ctx, s.bucketAttachments, objectName, bytes.NewReader(fileData), int64(len(fileData)),
		minio.PutObjectOptions{
			ContentType: contentType,
		})
	if err != nil {
		return nil, fmt.Errorf("failed to upload file: %w", err)
	}

	fileURL := s.getPublicURL(s.bucketAttachments, objectName)

	// Generate thumbnail for images in separate thumbs folder
	var thumbnailURL *string
	if AllowedImageTypes[contentType] {
		thumbURL, err := s.generateThumbnail(ctx, fileData, attachmentID, communityID, channelID, ext)
		if err == nil {
			thumbnailURL = &thumbURL
		}
	}

	// Store in database
	contentTypePtr := &contentType
	attachment := &models.MessageAttachment{
		ID:           attachmentID,
		UploaderID:   userID,
		Filename:     header.Filename,
		ContentType:  contentTypePtr,
		FileSize:     header.Size,
		FileURL:      fileURL,
		ThumbnailURL: thumbnailURL,
		CreatedAt:    time.Now(),
	}

	query := `
		INSERT INTO message_attachments (id, uploader_id, filename, content_type, file_size, file_url, thumbnail_url, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`

	_, err = s.db.Exec(ctx, query,
		attachment.ID, attachment.UploaderID, attachment.Filename,
		attachment.ContentType, attachment.FileSize, attachment.FileURL,
		attachment.ThumbnailURL, attachment.CreatedAt,
	)
	if err != nil {
		// Cleanup uploaded file
		s.minio.RemoveObject(ctx, s.bucketAttachments, objectName, minio.RemoveObjectOptions{})
		return nil, fmt.Errorf("failed to save attachment record: %w", err)
	}

	return &UploadResult{
		ID:           attachment.ID,
		Filename:     attachment.Filename,
		ContentType:  *attachment.ContentType,
		Size:         attachment.FileSize,
		URL:          attachment.FileURL,
		ThumbnailURL: attachment.ThumbnailURL,
	}, nil
}

// UploadDmAttachment handles file uploads for DM attachments
func (s *Service) UploadDmAttachment(ctx context.Context, userID, conversationID uuid.UUID, file multipart.File, header *multipart.FileHeader) (*UploadResult, error) {
	contentType := header.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	// Validate file size
	if header.Size > MaxFileSize {
		return nil, ErrFileTooLarge
	}

	if !s.canAccessDmConversation(ctx, conversationID, userID) {
		return nil, ErrNotParticipant
	}

	fileData, err := io.ReadAll(file)
	if err != nil {
		return nil, fmt.Errorf("failed to read file: %w", err)
	}

	ext := filepath.Ext(header.Filename)
	attachmentID := uuid.New()
	objectName := fmt.Sprintf("dm/%s/%s%s", conversationID.String(), attachmentID.String(), ext)

	_, err = s.minio.PutObject(ctx, s.bucketAttachments, objectName, bytes.NewReader(fileData), int64(len(fileData)),
		minio.PutObjectOptions{
			ContentType: contentType,
		})
	if err != nil {
		return nil, fmt.Errorf("failed to upload file: %w", err)
	}

	fileURL := s.getPublicURL(s.bucketAttachments, objectName)

	var thumbnailURL *string
	if AllowedImageTypes[contentType] {
		thumbURL, err := s.generateDmThumbnail(ctx, fileData, attachmentID, conversationID)
		if err == nil {
			thumbnailURL = &thumbURL
		}
	}

	contentTypePtr := &contentType
	attachment := &models.MessageAttachment{
		ID:           attachmentID,
		UploaderID:   userID,
		Filename:     header.Filename,
		ContentType:  contentTypePtr,
		FileSize:     header.Size,
		FileURL:      fileURL,
		ThumbnailURL: thumbnailURL,
		CreatedAt:    time.Now(),
	}

	query := `
		INSERT INTO message_attachments (id, uploader_id, filename, content_type, file_size, file_url, thumbnail_url, created_at, dm_conversation_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`

	_, err = s.db.Exec(ctx, query,
		attachment.ID, attachment.UploaderID, attachment.Filename,
		attachment.ContentType, attachment.FileSize, attachment.FileURL,
		attachment.ThumbnailURL, attachment.CreatedAt, conversationID,
	)
	if err != nil {
		s.minio.RemoveObject(ctx, s.bucketAttachments, objectName, minio.RemoveObjectOptions{})
		return nil, fmt.Errorf("failed to save attachment record: %w", err)
	}

	return &UploadResult{
		ID:           attachment.ID,
		Filename:     attachment.Filename,
		ContentType:  *attachment.ContentType,
		Size:         attachment.FileSize,
		URL:          attachment.FileURL,
		ThumbnailURL: attachment.ThumbnailURL,
	}, nil
}

// UploadAvatar handles avatar uploads for users/communities
func (s *Service) UploadAvatar(ctx context.Context, ownerID uuid.UUID, ownerType string, file multipart.File, header *multipart.FileHeader) (string, error) {
	contentType := header.Header.Get("Content-Type")
	if !AllowedImageTypes[contentType] {
		return "", ErrInvalidFileType
	}

	if header.Size > MaxAvatarSize {
		return "", ErrFileTooLarge
	}

	fileData, err := io.ReadAll(file)
	if err != nil {
		return "", fmt.Errorf("failed to read file: %w", err)
	}

	// Process image - resize if needed
	processedData, err := s.processAvatar(fileData)
	if err != nil {
		return "", fmt.Errorf("failed to process avatar: %w", err)
	}

	ext := filepath.Ext(header.Filename)
	if ext == "" {
		ext = ".jpg"
	}

	// Include timestamp to ensure unique URL for cache busting
	objectName := fmt.Sprintf("%s-%d%s", ownerID.String(), time.Now().Unix(), ext)

	// Upload to MinIO
	_, err = s.minio.PutObject(ctx, s.bucketAvatars, objectName, bytes.NewReader(processedData), int64(len(processedData)),
		minio.PutObjectOptions{
			ContentType: "image/jpeg",
		})
	if err != nil {
		return "", fmt.Errorf("failed to upload avatar: %w", err)
	}

	url := s.getPublicURL(s.bucketAvatars, objectName)

	// Update the database record
	if ownerType == "users" {
		_, err = s.db.Exec(ctx, "UPDATE users SET avatar_url = $1, updated_at = NOW() WHERE id = $2", url, ownerID)
		if err != nil {
			return "", fmt.Errorf("failed to update user avatar: %w", err)
		}
	} else if ownerType == "communities" {
		_, err = s.db.Exec(ctx, "UPDATE communities SET avatar_url = $1, updated_at = NOW() WHERE id = $2", url, ownerID)
		if err != nil {
			return "", fmt.Errorf("failed to update community avatar: %w", err)
		}
	}

	return url, nil
}

// UploadCommunityAsset handles community banner/icon uploads
// assetType should be "banner" or "icon"
func (s *Service) UploadCommunityAsset(ctx context.Context, communityID uuid.UUID, assetType string, file multipart.File, header *multipart.FileHeader) (string, error) {
	contentType := header.Header.Get("Content-Type")
	if !AllowedImageTypes[contentType] {
		return "", ErrInvalidFileType
	}

	if header.Size > MaxImageSize {
		return "", ErrFileTooLarge
	}

	fileData, err := io.ReadAll(file)
	if err != nil {
		return "", fmt.Errorf("failed to read file: %w", err)
	}

	ext := filepath.Ext(header.Filename)
	// Include timestamp to ensure unique URL for cache busting
	objectName := fmt.Sprintf("%s-%s-%d%s", communityID.String(), assetType, time.Now().Unix(), ext)

	_, err = s.minio.PutObject(ctx, s.bucketCommunity, objectName, bytes.NewReader(fileData), int64(len(fileData)),
		minio.PutObjectOptions{
			ContentType: contentType,
		})
	if err != nil {
		return "", fmt.Errorf("failed to upload asset: %w", err)
	}

	url := s.getPublicURL(s.bucketCommunity, objectName)

	// Update the database record
	var column string
	if assetType == "banner" {
		column = "banner_url"
	} else if assetType == "icon" {
		column = "icon_url"
	}

	if column != "" {
		query := fmt.Sprintf("UPDATE communities SET %s = $1, updated_at = NOW() WHERE id = $2", column)
		_, err = s.db.Exec(ctx, query, url, communityID)
		if err != nil {
			return "", fmt.Errorf("failed to update community %s: %w", assetType, err)
		}
	}

	return url, nil
}

// getPublicURL constructs a public URL for an object
func (s *Service) getPublicURL(bucket, objectName string) string {
	baseURL := strings.TrimSuffix(s.cdnBaseURL, "/")

	// If the baseURL already contains a bucket (legacy or misconfig),
	// we try to extract only the base host part to avoid "bucket-in-bucket" URLs
	// but strictly speaking, it should just be host:port

	// A more robust way: if cdnBaseURL contains any of our bucket names,
	// strip them to get the true base URL
	for _, b := range []string{s.bucketAttachments, s.bucketAvatars, s.bucketCommunity} {
		suffix := "/" + b
		if before, ok := strings.CutSuffix(baseURL, suffix); ok {
			baseURL = before
			break
		}
	}

	return fmt.Sprintf("%s/%s/%s", baseURL, bucket, objectName)
}

// trimURLToObjectName removes the CDN and bucket prefix from a URL to get the MinIO object key
func (s *Service) trimURLToObjectName(fileURL, bucket string) string {
	baseURL := strings.TrimSuffix(s.cdnBaseURL, "/")

	// Clean baseURL similarly to getPublicURL for consistency
	for _, b := range []string{s.bucketAttachments, s.bucketAvatars, s.bucketCommunity} {
		suffix := "/" + b
		if before, ok := strings.CutSuffix(baseURL, suffix); ok {
			baseURL = before
			break
		}
	}

	prefix := fmt.Sprintf("%s/%s/", baseURL, bucket)
	return strings.TrimPrefix(fileURL, prefix)
}

// GetAttachment retrieves attachment metadata
func (s *Service) GetAttachment(ctx context.Context, attachmentID uuid.UUID) (*models.MessageAttachment, error) {
	var a models.MessageAttachment
	query := `
		SELECT id, message_id, uploader_id, filename, file_url, file_size, content_type, thumbnail_url, width, height, created_at
		FROM message_attachments
		WHERE id = $1`

	err := s.db.QueryRow(ctx, query, attachmentID).Scan(
		&a.ID, &a.MessageID, &a.UploaderID, &a.Filename, &a.FileURL,
		&a.FileSize, &a.ContentType, &a.ThumbnailURL, &a.Width, &a.Height, &a.CreatedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrAttachmentNotFound
		}
		return nil, err
	}

	return &a, nil
}

// DeleteAttachment removes an attachment
func (s *Service) DeleteAttachment(ctx context.Context, attachmentID, userID uuid.UUID) error {
	// Get attachment to verify ownership and get URL for deletion
	attachment, err := s.GetAttachment(ctx, attachmentID)
	if err != nil {
		return err
	}

	if attachment.UploaderID != userID {
		return errors.New("not attachment owner")
	}

	// Delete from database
	_, err = s.db.Exec(ctx, `DELETE FROM message_attachments WHERE id = $1`, attachmentID)
	if err != nil {
		return err
	}

	// Delete from MinIO
	objectName := s.trimURLToObjectName(attachment.FileURL, s.bucketAttachments)
	s.minio.RemoveObject(ctx, s.bucketAttachments, objectName, minio.RemoveObjectOptions{})

	// Delete thumbnail if exists
	if attachment.ThumbnailURL != nil {
		thumbObjectName := s.trimURLToObjectName(*attachment.ThumbnailURL, s.bucketAttachments)
		s.minio.RemoveObject(ctx, s.bucketAttachments, thumbObjectName, minio.RemoveObjectOptions{})
	}

	return nil
}

// GetPresignedURL generates a presigned URL for direct download
func (s *Service) GetPresignedURL(ctx context.Context, attachmentID uuid.UUID, expiry time.Duration) (string, error) {
	attachment, err := s.GetAttachment(ctx, attachmentID)
	if err != nil {
		return "", err
	}

	objectName := s.trimURLToObjectName(attachment.FileURL, s.bucketAttachments)

	presignedURL, err := s.minio.PresignedGetObject(ctx, s.bucketAttachments, objectName, expiry, nil)
	if err != nil {
		return "", fmt.Errorf("failed to generate presigned URL: %w", err)
	}

	return presignedURL.String(), nil
}

func (s *Service) canAccessDmConversation(ctx context.Context, conversationID, userID uuid.UUID) bool {
	var exists bool
	err := s.db.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM dm_participants WHERE conversation_id = $1 AND user_id = $2)`,
		conversationID, userID,
	).Scan(&exists)
	return err == nil && exists
}

func (s *Service) canAccessAttachment(ctx context.Context, attachmentID, userID uuid.UUID) error {
	var messageID, dmConversationID *uuid.UUID
	err := s.db.QueryRow(ctx,
		`SELECT message_id, dm_conversation_id FROM message_attachments WHERE id = $1`,
		attachmentID,
	).Scan(&messageID, &dmConversationID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrAttachmentNotFound
		}
		log.Warn().Err(err).Msg("failed to get attachment parent info")
		return fmt.Errorf("failed to get attachment parent: %w", err)
	}

	if messageID != nil {
		var channelID uuid.UUID
		err := s.db.QueryRow(ctx,
			`SELECT channel_id FROM messages WHERE id = $1`, messageID,
		).Scan(&channelID)
		if err != nil {
			log.Warn().Err(err).Msg("failed to get message channel")
			return fmt.Errorf("failed to get message channel: %w", err)
		}

		var communityID uuid.UUID
		err = s.db.QueryRow(ctx,
			`SELECT community_id FROM channels WHERE id = $1`, channelID,
		).Scan(&communityID)
		if err != nil {
			log.Warn().Err(err).Msg("failed to get channel community")
			return fmt.Errorf("failed to get channel community: %w", err)
		}

		if !s.communityService.IsMember(ctx, communityID, userID) {
			return ErrAccessDenied
		}
		return nil
	}

	if dmConversationID != nil {
		if !s.canAccessDmConversation(ctx, *dmConversationID, userID) {
			return ErrAccessDenied
		}
		return nil
	}

	return ErrAccessDenied
}

// generateThumbnail creates a thumbnail for image attachments
// Currently, only JPEG thumbnails are generated.
// I need to modify this later to support PNG's with transparency.
// For now, this will do.
func (s *Service) generateThumbnail(ctx context.Context, imageData []byte, attachmentID, communityID, channelID uuid.UUID, ext string) (string, error) {
	img, _, err := image.Decode(bytes.NewReader(imageData))
	if err != nil {
		return "", err
	}

	// Resize maintaining aspect ratio
	thumb := resize.Thumbnail(ThumbnailMaxWidth, ThumbnailMaxHeight, img, resize.Lanczos3)

	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, thumb, &jpeg.Options{Quality: 80}); err != nil {
		return "", err
	}

	// Store thumbnails in: community/channel/thumbs/filename
	thumbObjectName := fmt.Sprintf("%s/%s/thumbs/%s_thumb.jpg", communityID.String(), channelID.String(), attachmentID.String())

	_, err = s.minio.PutObject(ctx, s.bucketAttachments, thumbObjectName, &buf, int64(buf.Len()),
		minio.PutObjectOptions{
			ContentType: "image/jpeg",
		})
	if err != nil {
		return "", err
	}

	return s.getPublicURL(s.bucketAttachments, thumbObjectName), nil
}

func (s *Service) generateDmThumbnail(ctx context.Context, imageData []byte, attachmentID, conversationID uuid.UUID) (string, error) {
	img, _, err := image.Decode(bytes.NewReader(imageData))
	if err != nil {
		return "", err
	}

	thumb := resize.Thumbnail(ThumbnailMaxWidth, ThumbnailMaxHeight, img, resize.Lanczos3)

	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, thumb, &jpeg.Options{Quality: 80}); err != nil {
		return "", err
	}

	thumbObjectName := fmt.Sprintf("dm/%s/thumbs/%s_thumb.jpg", conversationID.String(), attachmentID.String())

	_, err = s.minio.PutObject(ctx, s.bucketAttachments, thumbObjectName, &buf, int64(buf.Len()),
		minio.PutObjectOptions{
			ContentType: "image/jpeg",
		})
	if err != nil {
		return "", err
	}

	return s.getPublicURL(s.bucketAttachments, thumbObjectName), nil
}

func (s *Service) processAvatar(imageData []byte) ([]byte, error) {
	img, _, err := image.Decode(bytes.NewReader(imageData))
	if err != nil {
		return nil, err
	}

	// Resize to 256x256 max while maintaining aspect ratio
	// I should allow larger sizes in the future, but for now this is fine.
	// Nobody is really using it right now, and this should be recreated by the time
	// it ever hits production anyways, so optimization isn't a huge concern.
	resized := resize.Thumbnail(256, 256, img, resize.Lanczos3)

	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, resized, &jpeg.Options{Quality: 85}); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

func (s *Service) RequirePermission(ctx context.Context, communityID, userID uuid.UUID, permission int64) error {
	return s.communityService.RequirePermission(ctx, communityID, userID, permission)
}

func (s *Service) UpdateCommunityIcon(ctx context.Context, communityID, userID uuid.UUID, iconURL string) error {
	return s.communityService.UpdateCommunityIcon(ctx, communityID, userID, iconURL)
}

func (s *Service) UpdateCommunityBanner(ctx context.Context, communityID, userID uuid.UUID, bannerURL string) error {
	return s.communityService.UpdateCommunityBanner(ctx, communityID, userID, bannerURL)
}
