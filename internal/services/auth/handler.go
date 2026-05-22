package auth

import (
	"net"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
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

	// Public routes (with strict rate limiting)
	r.Group(func(r chi.Router) {
		r.Use(middleware.StrictRateLimitMiddleware(10)) // 10 requests per minute, I need to tune this later
		r.Post("/register", h.Register)
		r.Post("/verify-email", h.VerifyEmail)
		r.Post("/resend-verification", h.ResendVerification)
		r.Post("/login", h.Login)
		r.Post("/refresh", h.RefreshToken)
	})

	// Authenticated routes
	r.Group(func(r chi.Router) {
		r.Post("/logout", h.Logout)
		r.Post("/logout-all", h.LogoutAll)
		r.Post("/change-password", h.ChangePassword)

		// 2FA routes
		r.Post("/2fa/enable", h.Enable2FA)
		r.Post("/2fa/verify", h.Verify2FA)
		r.Post("/2fa/disable", h.Disable2FA)
	})

	return r
}

func (h *Handler) Register(w http.ResponseWriter, r *http.Request) {
	var req RegisterRequest
	if err := utils.DecodeJSON(r, &req); err != nil {
		utils.RespondError(w, http.StatusBadRequest, "Invalid request body")
		return
	}

	if err := utils.Validate(&req); err != nil {
		utils.RespondValidationError(w, utils.FormatValidationErrors(err))
		return
	}

	resp, err := h.service.Register(r.Context(), &req, clientIPFromRequest(r))
	if err != nil {
		switch err {
		case ErrUserExists:
			utils.RespondErrorWithCode(w, http.StatusConflict, "USER_EXISTS", "Username or email already in use")
		case ErrCaptchaRequired:
			utils.RespondErrorWithCode(w, http.StatusBadRequest, "CAPTCHA_REQUIRED", "Captcha token is required")
		case ErrCaptchaInvalid:
			utils.RespondErrorWithCode(w, http.StatusBadRequest, "CAPTCHA_INVALID", "Captcha verification failed")
		case ErrCaptchaUnavailable:
			utils.RespondErrorWithCode(w, http.StatusServiceUnavailable, "CAPTCHA_UNAVAILABLE", "Captcha verification is currently unavailable")
		default:
			utils.RespondError(w, http.StatusInternalServerError, "Failed to register user")
		}
		return
	}

	utils.RespondCreated(w, resp)
}

func (h *Handler) Login(w http.ResponseWriter, r *http.Request) {
	var req LoginRequest
	if err := utils.DecodeJSON(r, &req); err != nil {
		utils.RespondError(w, http.StatusBadRequest, "Invalid request body")
		return
	}

	if err := utils.Validate(&req); err != nil {
		utils.RespondValidationError(w, utils.FormatValidationErrors(err))
		return
	}

	resp, err := h.service.Login(r.Context(), &req)
	if err != nil {
		switch err {
		case ErrInvalidCredentials:
			utils.RespondErrorWithCode(w, http.StatusUnauthorized, "INVALID_CREDENTIALS", "Invalid username/email or password")
		case ErrEmailNotVerified:
			utils.RespondErrorWithCode(w, http.StatusForbidden, "EMAIL_NOT_VERIFIED", "Please verify your email before logging in")
		case ErrInvalid2FA:
			utils.RespondErrorWithCode(w, http.StatusUnauthorized, "INVALID_2FA", "Invalid 2FA code")
		default:
			utils.RespondError(w, http.StatusInternalServerError, "Failed to login")
		}
		return
	}

	utils.RespondSuccess(w, resp)
}

func (h *Handler) VerifyEmail(w http.ResponseWriter, r *http.Request) {
	var req VerifyEmailRequest
	if err := utils.DecodeJSON(r, &req); err != nil {
		utils.RespondError(w, http.StatusBadRequest, "Invalid request body")
		return
	}

	if err := utils.Validate(&req); err != nil {
		utils.RespondValidationError(w, utils.FormatValidationErrors(err))
		return
	}

	if err := h.service.VerifyEmail(r.Context(), req.Token); err != nil {
		switch err {
		case ErrInvalidVerifyToken:
			utils.RespondErrorWithCode(w, http.StatusBadRequest, "INVALID_VERIFICATION_TOKEN", "Verification link is invalid or expired")
		case ErrUserNotFound:
			utils.RespondErrorWithCode(w, http.StatusBadRequest, "INVALID_VERIFICATION_TOKEN", "Verification link is invalid or expired")
		default:
			utils.RespondError(w, http.StatusInternalServerError, "Failed to verify email")
		}
		return
	}

	utils.RespondSuccess(w, map[string]string{"message": "Email verified successfully"})
}

func (h *Handler) ResendVerification(w http.ResponseWriter, r *http.Request) {
	var req ResendVerificationRequest
	if err := utils.DecodeJSON(r, &req); err != nil {
		utils.RespondError(w, http.StatusBadRequest, "Invalid request body")
		return
	}

	if err := utils.Validate(&req); err != nil {
		utils.RespondValidationError(w, utils.FormatValidationErrors(err))
		return
	}

	if err := h.service.ResendVerificationEmail(r.Context(), req.Email); err != nil {
		switch err {
		case ErrEmailNotConfigured:
			utils.RespondErrorWithCode(w, http.StatusServiceUnavailable, "EMAIL_UNAVAILABLE", "Verification email delivery is not configured")
		case ErrEmailSendFailed:
			utils.RespondErrorWithCode(w, http.StatusServiceUnavailable, "EMAIL_SEND_FAILED", "Failed to send verification email")
		default:
			utils.RespondError(w, http.StatusInternalServerError, "Failed to resend verification email")
		}
		return
	}

	utils.RespondSuccess(w, map[string]string{"message": "If an unverified account exists for that email, a verification link has been sent."})
}

func (h *Handler) RefreshToken(w http.ResponseWriter, r *http.Request) {
	var req RefreshRequest
	if err := utils.DecodeJSON(r, &req); err != nil {
		utils.RespondError(w, http.StatusBadRequest, "Invalid request body")
		return
	}

	resp, err := h.service.RefreshToken(r.Context(), req.RefreshToken)
	if err != nil {
		switch err {
		case ErrSessionNotFound, ErrSessionExpired:
			utils.RespondErrorWithCode(w, http.StatusUnauthorized, "INVALID_SESSION", "Session not found or expired")
		default:
			utils.RespondError(w, http.StatusInternalServerError, "Failed to refresh token")
		}
		return
	}

	utils.RespondSuccess(w, resp)
}

func (h *Handler) Logout(w http.ResponseWriter, r *http.Request) {
	userID, err := middleware.RequireAuth(r.Context())
	if err != nil {
		utils.RespondError(w, http.StatusUnauthorized, "Unauthorized")
		return
	}

	var req RefreshRequest
	if err := utils.DecodeJSON(r, &req); err != nil {
		utils.RespondError(w, http.StatusBadRequest, "Invalid request body")
		return
	}

	if err := h.service.Logout(r.Context(), userID, req.RefreshToken); err != nil {
		utils.RespondError(w, http.StatusInternalServerError, "Failed to logout")
		return
	}

	utils.RespondNoContent(w)
}

func (h *Handler) LogoutAll(w http.ResponseWriter, r *http.Request) {
	userID, err := middleware.RequireAuth(r.Context())
	if err != nil {
		utils.RespondError(w, http.StatusUnauthorized, "Unauthorized")
		return
	}

	if err := h.service.LogoutAll(r.Context(), userID); err != nil {
		utils.RespondError(w, http.StatusInternalServerError, "Failed to logout from all devices")
		return
	}

	utils.RespondNoContent(w)
}

func (h *Handler) ChangePassword(w http.ResponseWriter, r *http.Request) {
	userID, err := middleware.RequireAuth(r.Context())
	if err != nil {
		utils.RespondError(w, http.StatusUnauthorized, "Unauthorized")
		return
	}

	var req struct {
		CurrentPassword string `json:"currentPassword" validate:"required"`
		NewPassword     string `json:"newPassword" validate:"required,strongpassword"`
	}
	if err := utils.DecodeJSON(r, &req); err != nil {
		utils.RespondError(w, http.StatusBadRequest, "Invalid request body")
		return
	}

	if err := utils.Validate(&req); err != nil {
		utils.RespondValidationError(w, utils.FormatValidationErrors(err))
		return
	}

	if err := h.service.ChangePassword(r.Context(), userID, req.CurrentPassword, req.NewPassword); err != nil {
		switch err {
		case ErrInvalidCredentials:
			utils.RespondErrorWithCode(w, http.StatusUnauthorized, "INVALID_PASSWORD", "Current password is incorrect")
		default:
			utils.RespondError(w, http.StatusInternalServerError, "Failed to change password")
		}
		return
	}

	utils.RespondJSON(w, http.StatusOK, map[string]string{"message": "Password changed successfully. Please login again."})
}

func (h *Handler) Enable2FA(w http.ResponseWriter, r *http.Request) {
	userID, err := middleware.RequireAuth(r.Context())
	if err != nil {
		utils.RespondError(w, http.StatusUnauthorized, "Unauthorized")
		return
	}

	resp, err := h.service.Enable2FA(r.Context(), userID)
	if err != nil {
		utils.RespondError(w, http.StatusInternalServerError, "Failed to enable 2FA")
		return
	}

	utils.RespondSuccess(w, resp)
}

func (h *Handler) Verify2FA(w http.ResponseWriter, r *http.Request) {
	userID, err := middleware.RequireAuth(r.Context())
	if err != nil {
		utils.RespondError(w, http.StatusUnauthorized, "Unauthorized")
		return
	}

	var req struct {
		Code string `json:"code" validate:"required,len=6"`
	}
	if err := utils.DecodeJSON(r, &req); err != nil {
		utils.RespondError(w, http.StatusBadRequest, "Invalid request body")
		return
	}

	if err := h.service.Verify2FA(r.Context(), userID, req.Code); err != nil {
		switch err {
		case ErrInvalid2FA:
			utils.RespondErrorWithCode(w, http.StatusBadRequest, "INVALID_CODE", "Invalid verification code")
		default:
			utils.RespondError(w, http.StatusInternalServerError, "Failed to verify 2FA")
		}
		return
	}

	utils.RespondJSON(w, http.StatusOK, map[string]string{"message": "2FA enabled successfully"})
}

func (h *Handler) Disable2FA(w http.ResponseWriter, r *http.Request) {
	userID, err := middleware.RequireAuth(r.Context())
	if err != nil {
		utils.RespondError(w, http.StatusUnauthorized, "Unauthorized")
		return
	}

	var req struct {
		Password string `json:"password" validate:"required"`
		Code     string `json:"code" validate:"required,len=6"`
	}
	if err := utils.DecodeJSON(r, &req); err != nil {
		utils.RespondError(w, http.StatusBadRequest, "Invalid request body")
		return
	}

	if err := h.service.Disable2FA(r.Context(), userID, req.Password, req.Code); err != nil {
		switch err {
		case ErrInvalidCredentials:
			utils.RespondErrorWithCode(w, http.StatusUnauthorized, "INVALID_PASSWORD", "Password is incorrect")
		case ErrInvalid2FA:
			utils.RespondErrorWithCode(w, http.StatusBadRequest, "INVALID_CODE", "Invalid 2FA code")
		default:
			utils.RespondError(w, http.StatusInternalServerError, "Failed to disable 2FA")
		}
		return
	}

	utils.RespondJSON(w, http.StatusOK, map[string]string{"message": "2FA disabled successfully"})
}

func clientIPFromRequest(r *http.Request) string {
	forwardedFor := strings.TrimSpace(r.Header.Get("X-Forwarded-For"))
	if forwardedFor != "" {
		parts := strings.Split(forwardedFor, ",")
		if len(parts) > 0 {
			ip := strings.TrimSpace(parts[0])
			if ip != "" {
				return ip
			}
		}
	}

	realIP := strings.TrimSpace(r.Header.Get("X-Real-IP"))
	if realIP != "" {
		return realIP
	}

	host, _, err := net.SplitHostPort(strings.TrimSpace(r.RemoteAddr))
	if err == nil {
		return host
	}

	return strings.TrimSpace(r.RemoteAddr)
}
