package utils

import (
	"regexp"
	"strings"
	"unicode"

	"github.com/go-playground/validator/v10"
)

var (
	validate                 *validator.Validate
	usernameRegex            = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)
	channelNameRegex         = regexp.MustCompile(`^[a-z0-9-]+$`)
	invalidChannelCharsRegex = regexp.MustCompile(`[^a-z0-9-]`)
)

func init() {
	validate = validator.New()

	// Custom validation for username
	validate.RegisterValidation("username", func(fl validator.FieldLevel) bool {
		username := fl.Field().String()
		if len(username) < 3 || len(username) > 32 {
			return false
		}
		return usernameRegex.MatchString(username)
	})

	// Custom validation for channel names (lowercase, hyphens, numbers)
	validate.RegisterValidation("channelname", func(fl validator.FieldLevel) bool {
		name := fl.Field().String()
		if len(name) < 1 || len(name) > 64 {
			return false
		}
		return channelNameRegex.MatchString(name)
	})

	// Custom validation for password strength
	validate.RegisterValidation("strongpassword", func(fl validator.FieldLevel) bool {
		password := fl.Field().String()
		if len(password) < 8 {
			return false
		}
		var hasUpper, hasLower, hasNumber bool
		for _, c := range password {
			switch {
			case unicode.IsUpper(c):
				hasUpper = true
			case unicode.IsLower(c):
				hasLower = true
			case unicode.IsNumber(c):
				hasNumber = true
			}
		}
		return hasUpper && hasLower && hasNumber
	})
}

// Validate validates a struct using the validator
func Validate(s any) error {
	return validate.Struct(s)
}

// FormatValidationErrors formats validation errors for API response
func FormatValidationErrors(err error) map[string]string {
	errors := make(map[string]string)

	if validationErrors, ok := err.(validator.ValidationErrors); ok {
		for _, e := range validationErrors {
			field := strings.ToLower(e.Field())
			switch e.Tag() {
			case "required":
				errors[field] = "This field is required"
			case "email":
				errors[field] = "Invalid email format"
			case "min":
				errors[field] = "Value is too short"
			case "max":
				errors[field] = "Value is too long"
			case "username":
				errors[field] = "Username must be 3-32 characters and contain only letters, numbers, underscores, or hyphens"
			case "channelname":
				errors[field] = "Channel name must contain only lowercase letters, numbers, and hyphens"
			case "strongpassword":
				errors[field] = "Password must be at least 8 characters with uppercase, lowercase, and numbers"
			default:
				errors[field] = "Invalid value"
			}
		}
	}

	return errors
}

// SanitizeString removes potentially dangerous characters from a string
func SanitizeString(s string) string {
	// Remove null bytes
	s = strings.ReplaceAll(s, "\x00", "")
	// Trim whitespace
	s = strings.TrimSpace(s)
	return s
}

// SanitizeHTML removes HTML tags from a string
func SanitizeHTML(s string) string {
	// Simple HTML tag removal - for more complex cases, use a proper library
	re := regexp.MustCompile(`<[^>]*>`)
	return re.ReplaceAllString(s, "")
}

// SanitizeSearchQuery sanitizes a user-provided search query for ILIKE patterns.
// It escapes wildcard characters (% and _), trims whitespace, and limits length
// to prevent DoS via expensive pattern matching, character probing, and full table scans.
func SanitizeSearchQuery(query string) string {
	query = strings.TrimSpace(query)
	if len(query) > 100 {
		query = query[:100]
	}
	query = strings.ReplaceAll(query, "%", "\\%")
	query = strings.ReplaceAll(query, "_", "\\_")
	return query
}

// TruncateString truncates a string to a maximum length
func TruncateString(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen]
}

// NormalizeChannelName converts a string to a valid channel name
func NormalizeChannelName(name string) string {
	name = strings.ToLower(name)
	name = strings.TrimSpace(name)
	// Replace spaces with hyphens
	name = strings.ReplaceAll(name, " ", "-")
	// Remove invalid characters
	name = invalidChannelCharsRegex.ReplaceAllString(name, "")
	// Remove consecutive hyphens
	for strings.Contains(name, "--") {
		name = strings.ReplaceAll(name, "--", "-")
	}
	// Trim hyphens from ends
	name = strings.Trim(name, "-")
	return name
}
