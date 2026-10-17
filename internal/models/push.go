package models

import (
	"time"

	"github.com/google/uuid"
)

// PushPlatform identifies which OS push transport a device token belongs to.
type PushPlatform string

const (
	PushPlatformAndroid PushPlatform = "android"
	PushPlatformIOS     PushPlatform = "ios"
)

// PushDevice is one registered OS push token belonging to a user.
type PushDevice struct {
	ID         uuid.UUID    `json:"id" db:"id"`
	UserID     uuid.UUID    `json:"userId" db:"user_id"`
	Platform   PushPlatform `json:"platform" db:"platform"`
	Token      string       `json:"-" db:"token"`
	DeviceName *string      `json:"deviceName,omitempty" db:"device_name"`
	AppVersion *string      `json:"appVersion,omitempty" db:"app_version"`
	LastSeenAt time.Time    `json:"lastSeenAt" db:"last_seen_at"`
	CreatedAt  time.Time    `json:"createdAt" db:"created_at"`
}
