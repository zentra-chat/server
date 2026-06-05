package admin

import (
	"context"
	"encoding/json"
	"runtime"
	"runtime/debug"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/rs/zerolog/log"
)

type ServerInfo struct {
	Version    string        `json:"version"`
	GoVersion  string        `json:"goVersion"`
	OS         string        `json:"os"`
	Arch       string        `json:"arch"`
	CPUCount   int           `json:"cpuCount"`
	GoRoutines int           `json:"goRoutines"`
	Uptime     int64         `json:"uptime"`
	StartTime  time.Time     `json:"startTime"`
	Database   ServiceStatus `json:"database"`
	Redis      ServiceStatus `json:"redis"`
}

type ServiceStatus struct {
	Status  string `json:"status"`
	Latency string `json:"latency,omitempty"`
	Error   string `json:"error,omitempty"`
}

type ServerConfigResponse struct {
	RegistrationOpen          bool   `json:"registrationOpen"`
	CaptchaEnabled            bool   `json:"captchaEnabled"`
	EmailVerificationRequired bool   `json:"emailVerificationRequired"`
	MaintenanceMode           bool   `json:"maintenanceMode"`
	MaintenanceMessage        string `json:"maintenanceMessage,omitempty"`
}

type UpdateServerConfigRequest struct {
	MaintenanceEnabled *bool   `json:"maintenanceEnabled,omitempty"`
	MaintenanceMessage *string `json:"maintenanceMessage,omitempty"`
}

type maintenanceData struct {
	Enabled bool   `json:"enabled"`
	Message string `json:"message"`
}

func (s *Service) GetServerInfo(ctx context.Context) *ServerInfo {
	info := &ServerInfo{
		Version:    Version,
		GoVersion:  runtime.Version(),
		OS:         runtime.GOOS,
		Arch:       runtime.GOARCH,
		CPUCount:   runtime.NumCPU(),
		GoRoutines: runtime.NumGoroutine(),
		Uptime:     int64(time.Since(s.startAt).Seconds()),
		StartTime:  s.startAt,
	}

	info.Database = s.checkDatabase(ctx)
	info.Redis = s.checkRedis(ctx)

	buildInfo, ok := debug.ReadBuildInfo()
	if ok && info.Version == "dev" {
		info.Version = buildInfo.Main.Version
	}

	return info
}

func (s *Service) GetServerConfig(ctx context.Context) (*ServerConfigResponse, error) {
	cfg := &ServerConfigResponse{
		RegistrationOpen:          s.cfg.Captcha.Enabled || !s.cfg.Email.VerificationRequired,
		CaptchaEnabled:            s.cfg.Captcha.Enabled,
		EmailVerificationRequired: s.cfg.Email.VerificationRequired,
	}

	maintenance, err := s.getMaintenanceMode(ctx)
	if err != nil {
		log.Warn().Err(err).Msg("Failed to get maintenance mode")
		cfg.MaintenanceMode = false
	} else {
		cfg.MaintenanceMode = maintenance.Enabled
		cfg.MaintenanceMessage = maintenance.Message
	}

	return cfg, nil
}

func (s *Service) UpdateServerConfig(ctx context.Context, req *UpdateServerConfigRequest) (*ServerConfigResponse, error) {
	if req.MaintenanceEnabled != nil || req.MaintenanceMessage != nil {
		current, err := s.getMaintenanceMode(ctx)
		if err != nil {
			current = &maintenanceData{}
		}

		if req.MaintenanceEnabled != nil {
			current.Enabled = *req.MaintenanceEnabled
		}
		if req.MaintenanceMessage != nil {
			current.Message = *req.MaintenanceMessage
		}

		if err := s.setMaintenanceMode(ctx, current); err != nil {
			log.Warn().Err(err).Msg("Failed to set maintenance mode")
			return nil, err
		}
	}

	return s.GetServerConfig(ctx)
}

func (s *Service) getMaintenanceMode(ctx context.Context) (*maintenanceData, error) {
	data, err := s.rdb.Get(ctx, "zentra:maintenance").Bytes()
	if err != nil {
		if err == redis.Nil {
			return &maintenanceData{}, nil
		}
		log.Warn().Err(err).Msg("Failed to get maintenance mode from Redis")
		return &maintenanceData{}, err
	}

	var m maintenanceData
	if err := json.Unmarshal(data, &m); err != nil {
		log.Warn().Err(err).Msg("Failed to unmarshal maintenance data")
		return &maintenanceData{}, nil
	}

	return &m, nil
}

func (s *Service) setMaintenanceMode(ctx context.Context, m *maintenanceData) error {
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}

	return s.rdb.Set(ctx, "zentra:maintenance", data, 0).Err()
}

func (s *Service) checkDatabase(ctx context.Context) ServiceStatus {
	start := time.Now()
	err := s.db.Ping(ctx)
	latency := time.Since(start).Round(time.Microsecond).String()

	if err != nil {
		log.Warn().Err(err).Msg("Database health check failed")
		return ServiceStatus{
			Status:  "error",
			Latency: latency,
			Error:   err.Error(),
		}
	}

	return ServiceStatus{
		Status:  "connected",
		Latency: latency,
	}
}

func (s *Service) checkRedis(ctx context.Context) ServiceStatus {
	start := time.Now()
	err := s.rdb.Ping(ctx).Err()
	latency := time.Since(start).Round(time.Microsecond).String()

	if err != nil {
		log.Warn().Err(err).Msg("Redis health check failed")
		return ServiceStatus{
			Status:  "error",
			Latency: latency,
			Error:   err.Error(),
		}
	}

	return ServiceStatus{
		Status:  "connected",
		Latency: latency,
	}
}
