# Zentra Server

This repo holds the backend server for Zentra, an encrypted community chat application.

## Tech Stack

- **Go 1.23** - Backend language
- **PostgreSQL 16** - Primary database with partitioned tables
- **Redis 7** - Session storage, caching, pub/sub
- **MinIO** - S3-compatible object storage
- **Chi Router** - HTTP routing
- **gorilla/websocket** - WebSocket connections

## Getting Started

### Prerequisites

- Go 1.23+
- Docker & Docker Compose
- Make (optional)

### Quick Start

1. **Clone the repository**

   ```bash
   git clone https://github.com/zentra-chat/server.git
   cd server
   ```

2. **Set up environment**

   ```bash
   cp .env.example .env
   ```

3. **Start infrastructure**

   ```bash
   docker compose up -d postgres redis minio
   ```

4. **Run migrations**

   ```bash
   docker compose run --rm migrate up
   ```

5. **Start the server**
   ```bash
   docker compose up -d --build api
   ```

The API will be available at `http://localhost:63566` (default `API_PORT`).

For full API details, see [API.md](API.md).

## Docker Compose workflow

Use standard Docker Compose commands for local development and most deployments:

```bash
# start full stack
docker compose up -d --build

# inspect state
docker compose ps

# follow API logs
docker compose logs -f api

# stop stack
docker compose down
```

For custom host settings, set values in `.env` before starting:

```bash
API_PORT=8080
POSTGRES_PASSWORD=change-me
JWT_SECRET=change-me
ENCRYPTION_KEY=64_hex_chars_here
GITHUB_TOKEN=optional_personal_access_token
CAPTCHA_ENABLED=true
CAPTCHA_SECRET_KEY=turnstile_secret
EMAIL_SMTP_HOST=smtp.example.com
EMAIL_SMTP_PORT=587
EMAIL_SMTP_USERNAME=mailer@example.com
EMAIL_SMTP_PASSWORD=app_password
EMAIL_FROM_ADDRESS=noreply@example.com
EMAIL_VERIFICATION_URL=http://localhost:5173/verify-email
```

`GITHUB_TOKEN` is optional but recommended so the public GitHub stats endpoint (`/api/v1/public/github/stats`) has more API headroom.

To remove containers and volumes:

```bash
docker compose down -v
```

## Running migrations

Use the dedicated migration container:

```bash
# apply all pending migrations
docker compose run --rm migrate up

# rollback one migration
docker compose run --rm migrate down 1

# rollback three migrations
docker compose run --rm migrate down 3
```

Run migrations after PostgreSQL is up and before launching API changes that depend on new schema.

### Development

```bash
# Install dependencies
go mod download

# Run with hot reload (using air)
air
```
