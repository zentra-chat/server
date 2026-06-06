package admin

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog/log"
)

type UpdateTarget string

const (
	UpdateTargetBackend  UpdateTarget = "backend"
	UpdateTargetFrontend UpdateTarget = "frontend"
	UpdateTargetAll      UpdateTarget = "all"
)

type UpdateStatusEnum string

const (
	UpdateStatusPending   UpdateStatusEnum = "pending"
	UpdateStatusRunning   UpdateStatusEnum = "running"
	UpdateStatusCompleted UpdateStatusEnum = "completed"
	UpdateStatusFailed    UpdateStatusEnum = "failed"
)

type UpdateRequest struct {
	Target string `json:"target"`
}

type UpdateStatusResponse struct {
	ID         string           `json:"id"`
	Target     string           `json:"target"`
	Status     UpdateStatusEnum `json:"status"`
	Message    string           `json:"message"`
	Output     string           `json:"output,omitempty"`
	StartedAt  time.Time        `json:"startedAt"`
	FinishedAt *time.Time       `json:"finishedAt,omitempty"`
}

type updateTask struct {
	Status    UpdateStatusResponse
	outputBuf bytes.Buffer
	mu        sync.Mutex
}

func (t *updateTask) setMessage(msg string) {
	t.mu.Lock()
	t.Status.Message = msg
	t.mu.Unlock()
}

func (t *updateTask) appendOutput(out string) {
	t.mu.Lock()
	if out != "" {
		if t.outputBuf.Len() > 0 {
			t.outputBuf.WriteString("\n")
		}
		t.outputBuf.WriteString(out)
	}
	t.mu.Unlock()
}

func (t *updateTask) execCmd(dir, name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func (t *updateTask) runCmd(dir, name string, args ...string) bool {
	out, err := t.execCmd(dir, name, args...)
	if out != "" {
		t.appendOutput(fmt.Sprintf("$ %s %s", name, strings.Join(args, " ")))
		t.appendOutput(out)
	}
	if err != nil {
		t.appendOutput(fmt.Sprintf("Error: %v", err))
		return false
	}
	return true
}

func (t *updateTask) runCmdLive(dir, name string, args ...string) bool {
	t.mu.Lock()
	if t.outputBuf.Len() > 0 {
		t.outputBuf.WriteString("\n")
	}
	t.outputBuf.WriteString(fmt.Sprintf("$ %s %s", name, strings.Join(args, " ")))
	t.mu.Unlock()

	cmd := exec.Command(name, args...)
	cmd.Dir = dir

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.appendOutput(fmt.Sprintf("Error creating stdout pipe: %v", err))
		return false
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.appendOutput(fmt.Sprintf("Error creating stderr pipe: %v", err))
		return false
	}

	if err := cmd.Start(); err != nil {
		t.appendOutput(fmt.Sprintf("Error starting command: %v", err))
		return false
	}

	done := make(chan struct{}, 2)
	readStream := func(r io.Reader) {
		buf := make([]byte, 4096)
		for {
			n, err := r.Read(buf)
			if n > 0 {
				t.appendOutput(string(buf[:n]))
			}
			if err != nil {
				break
			}
		}
		done <- struct{}{}
	}
	go readStream(stdout)
	go readStream(stderr)

	<-done
	<-done

	if err := cmd.Wait(); err != nil {
		t.appendOutput(fmt.Sprintf("Error: %v", err))
		return false
	}
	return true
}

func (s *Service) StartUpdate(target string) (*UpdateStatusResponse, error) {
	validTargets := map[string]bool{
		string(UpdateTargetBackend):  true,
		string(UpdateTargetFrontend): true,
		string(UpdateTargetAll):      true,
	}
	if !validTargets[target] {
		return nil, errors.New("invalid target: must be 'backend', 'frontend', or 'all'")
	}

	s.updateMu.Lock()

	for _, task := range s.updateTasks {
		task.mu.Lock()
		if task.Status.Status == UpdateStatusRunning || task.Status.Status == UpdateStatusPending {
			task.mu.Unlock()
			s.updateMu.Unlock()
			return nil, errors.New("an update is already in progress")
		}
		task.mu.Unlock()
	}

	if len(s.updateTasks) >= 10 {
		var oldestID string
		var oldestTime time.Time
		for id, task := range s.updateTasks {
			task.mu.Lock()
			if task.Status.FinishedAt != nil && (oldestID == "" || task.Status.FinishedAt.Before(oldestTime)) {
				oldestID = id
				oldestTime = *task.Status.FinishedAt
			}
			task.mu.Unlock()
		}
		if oldestID != "" {
			delete(s.updateTasks, oldestID)
		}
	}

	id := uuid.New().String()
	now := time.Now()

	task := &updateTask{
		Status: UpdateStatusResponse{
			ID:        id,
			Target:    target,
			Status:    UpdateStatusRunning,
			Message:   "Starting update...",
			StartedAt: now,
		},
	}

	s.updateTasks[id] = task
	s.updateMu.Unlock()

	log.Info().Str("target", target).Str("task_id", id).Msg("Server update triggered by admin")

	go s.runUpdateTask(task)

	resp := task.Status
	resp.Output = ""
	return &resp, nil
}

func (s *Service) GetUpdateStatus(id string) *UpdateStatusResponse {
	s.updateMu.Lock()
	task, ok := s.updateTasks[id]
	s.updateMu.Unlock()

	if !ok {
		return nil
	}

	task.mu.Lock()
	defer task.mu.Unlock()

	resp := task.Status
	resp.Output = task.outputBuf.String()
	return &resp
}

func (s *Service) ListUpdateStatuses() []UpdateStatusResponse {
	s.updateMu.Lock()
	defer s.updateMu.Unlock()

	statuses := make([]UpdateStatusResponse, 0, len(s.updateTasks))
	for _, task := range s.updateTasks {
		task.mu.Lock()
		resp := task.Status
		resp.Output = task.outputBuf.String()
		task.mu.Unlock()
		statuses = append(statuses, resp)
	}
	return statuses
}

func (s *Service) runUpdateTask(task *updateTask) {
	switch s.updateMethod {
	case "docker":
		s.runDockerUpdate(task)
	case "command":
		s.runCustomCommand(task)
	case "direct":
		s.runDirectUpdate(task)
	default:
		s.failTask(task, fmt.Sprintf(`Unknown update method: %q

Set UPDATE_METHOD to one of:
  docker  - Pull latest code via git, run migrations, rebuild via Docker socket
  command - Execute a custom shell command from UPDATE_COMMAND
  direct  - Pull and build directly on the server (needs git, go, pnpm/npm)

For Docker deployments (recommended), add to your docker-compose.yml:
  environment:
    - UPDATE_METHOD=docker

For custom scripts, set both:
  UPDATE_METHOD=command
  UPDATE_COMMAND=<your shell command>`, s.updateMethod))
	}
}

// runDirectUpdate handles bare-metal updates where git, go, and node are
// available directly on the server (outside of Docker).
func (s *Service) runDirectUpdate(task *updateTask) {
	target := task.Status.Target
	projectRoot := s.backendDir

	task.setMessage("Pulling latest code...")
	task.appendOutput("--- Update started ---")

	currentCommit, _ := task.execCmd(projectRoot, "git", "rev-parse", "--short", "HEAD")
	currentCommit = strings.TrimSpace(currentCommit)
	task.appendOutput(fmt.Sprintf("Current commit: %s", currentCommit))

	if !task.runCmdLive(projectRoot, "git", "fetch", "origin") {
		s.failTask(task, "Failed to fetch from git remote")
		return
	}

	branch, err := task.execCmd(projectRoot, "git", "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		s.failTask(task, fmt.Sprintf("Failed to detect git branch: %s", strings.TrimSpace(branch)))
		return
	}
	branch = strings.TrimSpace(branch)
	task.appendOutput(fmt.Sprintf("Current branch: %s", branch))

	task.setMessage(fmt.Sprintf("Pulling latest changes from origin/%s...", branch))
	if !task.runCmdLive(projectRoot, "git", "pull", "--ff-only", "origin", branch) {
		s.failTask(task, "Failed to pull latest changes. There may be conflicts or divergent branches.")
		return
	}

	newCommit, _ := task.execCmd(projectRoot, "git", "rev-parse", "--short", "HEAD")
	newCommit = strings.TrimSpace(newCommit)
	task.appendOutput(fmt.Sprintf("New commit: %s", newCommit))

	if newCommit == currentCommit {
		task.appendOutput("Already up to date.")
	} else {
		task.setMessage("Updating submodules...")
		task.runCmdLive(projectRoot, "git", "submodule", "update", "--init", "--recursive")

		if err := s.migrateDatabase(task, projectRoot); err != nil {
			log.Warn().Err(err).Msg("Database migration failed, continuing update")
			task.appendOutput(fmt.Sprintf("Warning: Migration failed: %v", err))
		}
	}

	if target == string(UpdateTargetBackend) || target == string(UpdateTargetAll) {
		if err := s.buildBackend(task, projectRoot); err != nil {
			s.failTask(task, err.Error())
			return
		}
	}

	if target == string(UpdateTargetFrontend) || target == string(UpdateTargetAll) {
		err := s.buildFrontend(task, projectRoot)
		if err != nil {
			s.failTask(task, err.Error())
			return
		}
	}

	s.completeTask(task)

	if newCommit != currentCommit {
		task.appendOutput("")
		task.appendOutput("Update complete. Restart the server process to apply the new binary:")
		task.appendOutput(fmt.Sprintf("  systemctl restart zentra-api   # (if using systemd)"))
		task.appendOutput(fmt.Sprintf("  supervisorctl restart zentra   # (if using supervisord)"))
		task.appendOutput(fmt.Sprintf("  kill -TERM %d && exec ./bin/gateway # (if running directly)", os.Getpid()))
	}

	log.Info().Str("target", target).Str("task_id", task.Status.ID).Msg("Server update completed successfully")
}

// runDockerUpdate handles updates for Docker deployments using the Docker socket.
// Requires: git, docker CLI, and project root mounted with .git metadata.
func (s *Service) runDockerUpdate(task *updateTask) {
	target := task.Status.Target
	projectRoot := s.backendDir

	if _, err := exec.LookPath("docker"); err != nil {
		s.failTask(task, "Docker CLI not found. Install docker-cli in the container and try again.")
		return
	}
	if _, err := exec.LookPath("git"); err != nil {
		s.failTask(task, "Git not found. Install git in the container and try again.")
		return
	}
	if _, err := os.Stat(filepath.Join(projectRoot, ".git")); os.IsNotExist(err) {
		s.failTask(task, fmt.Sprintf("Not a git repository at %s. Mount the project root with -v $(pwd):/app", projectRoot))
		return
	}

	if err := exec.Command("docker", "compose", "version").Run(); err != nil {
		s.failTask(task, "Docker Compose not found. Install docker-compose in the container and try again.")
		return
	}

	task.setMessage("Pulling latest code...")
	task.appendOutput("--- Update started ---")

	currentCommit, _ := task.execCmd(projectRoot, "git", "rev-parse", "--short", "HEAD")
	currentCommit = strings.TrimSpace(currentCommit)
	task.appendOutput(fmt.Sprintf("Current commit: %s", currentCommit))

	if !task.runCmdLive(projectRoot, "git", "fetch", "origin") {
		s.failTask(task, "Failed to fetch from git remote")
		return
	}

	branch, err := task.execCmd(projectRoot, "git", "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		s.failTask(task, fmt.Sprintf("Failed to detect git branch: %s", strings.TrimSpace(branch)))
		return
	}
	branch = strings.TrimSpace(branch)
	task.appendOutput(fmt.Sprintf("Current branch: %s", branch))

	task.setMessage(fmt.Sprintf("Pulling latest changes from origin/%s...", branch))
	if !task.runCmdLive(projectRoot, "git", "pull", "--ff-only", "origin", branch) {
		s.failTask(task, "Failed to pull latest changes. There may be conflicts or divergent branches.")
		return
	}

	newCommit, _ := task.execCmd(projectRoot, "git", "rev-parse", "--short", "HEAD")
	newCommit = strings.TrimSpace(newCommit)
	task.appendOutput(fmt.Sprintf("New commit: %s", newCommit))

	if newCommit != currentCommit {
		task.setMessage("Updating submodules...")
		task.runCmdLive(projectRoot, "git", "submodule", "update", "--init", "--recursive")

		task.setMessage("Running database migrations...")
		task.appendOutput("Running `docker compose run --rm migrate up`...")
		if !task.runCmdLive(projectRoot, "docker", "compose", "run", "--rm", "migrate", "up") {
			log.Warn().Str("task_id", task.Status.ID).Msg("Migration failed, continuing with update")
			task.appendOutput("Warning: Migration failed. The database may be out of sync.")
		}
	} else {
		task.appendOutput("Already up to date.")
	}

	if target == string(UpdateTargetBackend) || target == string(UpdateTargetAll) {
		task.setMessage("Shutting down old containers...")
		task.appendOutput("Running `docker compose down`...")
		task.runCmdLive(projectRoot, "docker", "compose", "down")

		task.setMessage("Freeing ports from orphaned containers...")
		task.appendOutput("Checking for orphaned containers on required ports...")
		apiPort := os.Getenv("API_PORT")
		if apiPort == "" {
			apiPort = "63566"
		}
		freePorts := fmt.Sprintf(
			`for port in 5432 6379 9000 9001 %s; do cid=$(docker ps -q --filter "publish=$port"); [ -n "$cid" ] && docker rm -f "$cid"; done`,
			apiPort,
		)
		task.runCmdLive(projectRoot, "sh", "-c", freePorts)

		task.setMessage("Rebuilding and restarting backend...")
		task.appendOutput("Running `docker compose up -d --build api`...")
		if !task.runCmdLive(projectRoot, "docker", "compose", "up", "-d", "--build", "api") {
			s.failTask(task, "Backend rebuild and restart failed. Check the docker compose output above.")
			return
		}
	}

	if target == string(UpdateTargetFrontend) || target == string(UpdateTargetAll) {
		err := s.buildFrontend(task, projectRoot)
		if err != nil {
			s.failTask(task, err.Error())
			return
		}
	}

	s.completeTask(task)

	task.appendOutput("")
	task.appendOutput("Update completed successfully.")

	log.Info().Str("target", target).Str("task_id", task.Status.ID).Msg("Server update completed successfully")
}

// runCustomCommand executes a user-defined shell command for the update.
// The target is passed as the UPDATE_TARGET environment variable.
// For Docker deployments:
//
//	docker compose -f /app/docker-compose.yml up -d --build
func (s *Service) runCustomCommand(task *updateTask) {
	target := task.Status.Target

	task.setMessage("Executing update command...")
	task.appendOutput(fmt.Sprintf("--- Update started (target: %s) ---", target))
	task.appendOutput(fmt.Sprintf("Command: %s", s.updateCommand))

	cmd := exec.Command("sh", "-c", s.updateCommand)
	cmd.Dir = s.backendDir
	cmd.Env = append(os.Environ(), fmt.Sprintf("UPDATE_TARGET=%s", target))

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		s.failTask(task, fmt.Sprintf("Failed to create stdout pipe: %v", err))
		return
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		s.failTask(task, fmt.Sprintf("Failed to create stderr pipe: %v", err))
		return
	}

	if err := cmd.Start(); err != nil {
		s.failTask(task, fmt.Sprintf("Failed to start command: %v", err))
		return
	}

	done := make(chan struct{}, 2)
	readStream := func(r io.Reader) {
		buf := make([]byte, 4096)
		for {
			n, err := r.Read(buf)
			if n > 0 {
				task.appendOutput(string(buf[:n]))
			}
			if err != nil {
				break
			}
		}
		done <- struct{}{}
	}
	go readStream(stdout)
	go readStream(stderr)

	<-done
	<-done

	if err := cmd.Wait(); err != nil {
		s.failTask(task, fmt.Sprintf("Update command failed: %v", err))
		return
	}

	s.completeTask(task)
	log.Info().Str("target", target).Str("task_id", task.Status.ID).Msg("Update command completed successfully")
}

// migrateDatabase runs database migrations using the golang-migrate Docker image,
// or falls back to the migrate CLI if available.
func (s *Service) migrateDatabase(task *updateTask, projectRoot string) error {
	task.setMessage("Running database migrations...")

	if _, err := exec.LookPath("docker"); err == nil {
		if exec.Command("docker", "compose", "version").Run() == nil {
			if task.runCmdLive(projectRoot, "docker", "compose", "run", "--rm", "migrate", "up") {
				return nil
			}
			task.appendOutput("docker compose migration failed, trying migrate CLI...")
		}
	}

	if _, err := exec.LookPath("migrate"); err == nil {
		task.appendOutput("Running `migrate -path migrations -database ... up`...")
		dbURL := os.Getenv("DATABASE_URL")
		if dbURL == "" {
			dbURL = fmt.Sprintf("postgres://%s:%s@%s:%s/%s?sslmode=%s",
				os.Getenv("POSTGRES_USER"),
				os.Getenv("POSTGRES_PASSWORD"),
				os.Getenv("POSTGRES_HOST"),
				os.Getenv("POSTGRES_PORT"),
				os.Getenv("POSTGRES_DB"),
				os.Getenv("POSTGRES_SSLMODE"),
			)
		}
		migrationsDir := filepath.Join(projectRoot, "migrations")
		if _, err := os.Stat(migrationsDir); os.IsNotExist(err) {
			return fmt.Errorf("migrations directory not found at %s", migrationsDir)
		}
		if task.runCmdLive(projectRoot, "migrate", "-path", migrationsDir, "-database", dbURL, "up") {
			return nil
		}
		return errors.New("migrate CLI failed")
	}

	task.appendOutput("Neither docker compose nor migrate CLI is available for running migrations.")
	task.appendOutput("Run migrations manually after the update:")
	task.appendOutput("  docker compose run --rm migrate up")
	return errors.New("no migration tool available")
}

func (s *Service) buildBackend(task *updateTask, projectRoot string) error {
	task.setMessage("Building backend...")
	backendDir := s.findBackendDir(projectRoot)

	if _, err := exec.LookPath("make"); err == nil {
		if _, err := os.Stat(filepath.Join(backendDir, "Makefile")); err == nil {
			if task.runCmdLive(backendDir, "make", "build") {
				task.appendOutput("Backend build complete")
				return nil
			}
			task.appendOutput("make build failed, trying go build directly")
		}
	}

	if _, err := exec.LookPath("go"); err != nil {
		return errors.New("go not found. Install Go to build the backend, or use Docker mode.")
	}

	if task.runCmdLive(backendDir, "go", "build", "-o", "bin/gateway", "./cmd/gateway/") {
		task.appendOutput("Backend build complete (binary: bin/gateway)")
		return nil
	}

	return errors.New("failed to build backend")
}

func (s *Service) findBackendDir(projectRoot string) string {
	candidates := []string{
		projectRoot,
		filepath.Join(projectRoot, "backend"),
		filepath.Join(projectRoot, "server"),
		filepath.Join(projectRoot, "api"),
		filepath.Join(projectRoot, "live-api"),
	}

	for _, dir := range candidates {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
	}

	return projectRoot
}

func (s *Service) findFrontendDir(projectRoot string) string {
	if s.frontendDir == "" {
		return ""
	}

	info, err := os.Stat(s.frontendDir)
	if err != nil || !info.IsDir() {
		return ""
	}

	return s.frontendDir
}

func (s *Service) buildFrontend(task *updateTask, projectRoot string) error {
	frontendDir := s.findFrontendDir(projectRoot)
	if frontendDir == "" {
		task.appendOutput("Frontend directory not found. It should be a sibling of the backend repo.")
		task.appendOutput("Set FRONTEND_DIR in .env or mount the frontend directory into the container.")
		return nil
	}

	task.setMessage("Building frontend...")
	task.appendOutput(fmt.Sprintf("Frontend directory: %s", frontendDir))

	deployScript := filepath.Join(frontendDir, "scripts", "deploy-frontend.sh")
	if _, err := os.Stat(deployScript); err == nil {
		task.appendOutput("Using deploy-frontend.sh for update...")
		task.runCmdLive(frontendDir, "git", "fetch", "origin")

		frontendBranch, err := task.execCmd(frontendDir, "git", "rev-parse", "--abbrev-ref", "HEAD")
		if err == nil {
			frontendBranch = strings.TrimSpace(frontendBranch)
			task.runCmdLive(frontendDir, "git", "pull", "--ff-only", "origin", frontendBranch)
		}

		task.runCmdLive(frontendDir, "git", "submodule", "update", "--init", "--recursive")

		if !task.runCmdLive(frontendDir, "bash", "scripts/deploy-frontend.sh", "update") {
			return errors.New("frontend deploy script failed")
		}
		task.appendOutput("Frontend build complete via deploy-frontend.sh")
		return nil
	}

	pnpmPath, _ := exec.LookPath("pnpm")
	npmPath, _ := exec.LookPath("npm")

	if pnpmPath != "" {
		task.setMessage("Installing frontend dependencies with pnpm...")
		if !task.runCmdLive(frontendDir, "pnpm", "install", "--frozen-lockfile") {
			task.appendOutput("Frozen lockfile install failed, trying regular install...")
			if !task.runCmdLive(frontendDir, "pnpm", "install") {
				return errors.New("failed to install frontend dependencies with pnpm")
			}
		}
		task.setMessage("Building frontend with pnpm...")
		if !task.runCmdLive(frontendDir, "pnpm", "run", "build") {
			return errors.New("failed to build frontend with pnpm")
		}
	} else if npmPath != "" {
		task.setMessage("Installing frontend dependencies with npm...")
		if !task.runCmdLive(frontendDir, "npm", "ci") {
			task.appendOutput("npm ci failed, trying npm install...")
			if !task.runCmdLive(frontendDir, "npm", "install") {
				return errors.New("failed to install frontend dependencies with npm")
			}
		}
		task.setMessage("Building frontend with npm...")
		if !task.runCmdLive(frontendDir, "npm", "run", "build") {
			return errors.New("failed to build frontend with npm")
		}
	} else {
		task.appendOutput("Node.js not found in this container. Frontend build skipped.")
		task.appendOutput("To enable frontend builds, add nodejs+npm to the container or build externally.")
		return nil
	}

	task.appendOutput("Frontend build complete")
	return nil
}

func (s *Service) failTask(task *updateTask, msg string) {
	task.mu.Lock()
	task.Status.Status = UpdateStatusFailed
	task.Status.Message = msg
	now := time.Now()
	task.Status.FinishedAt = &now
	log.Warn().Str("task_id", task.Status.ID).Str("target", task.Status.Target).Msg(msg)
	task.mu.Unlock()
}

func (s *Service) completeTask(task *updateTask) {
	task.mu.Lock()
	task.Status.Status = UpdateStatusCompleted
	task.Status.Message = "Update completed successfully"
	now := time.Now()
	task.Status.FinishedAt = &now
	task.mu.Unlock()
}
