# Development

## Prerequisites

- Go 1.27+
- [air](https://github.com/air-verse/air) — `go install github.com/air-verse/air@latest`
- [golangci-lint](https://github.com/golangci/golangci-lint) — `go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest` (for `make lint`)

## Quick Start

The fastest way to get a working local environment:

```bash
make dev
```

This builds the binary, runs migrations, creates an admin user (`admin@localhost` / `admin`), discovers and fetches all seed packages, builds repository artifacts, and starts the HTTP server.

## Manual Setup

```bash
# Build
make build

# Run migrations
./wppackages migrate

# Create admin user
echo 'your-password' | ./wppackages admin create \
  --email admin@example.com \
  --name "Admin" \
  --password-stdin

# Start server
./wppackages serve
```

## Commands

| Command | Purpose |
|---------|---------|
| `wppackages dev` | Bootstrap and start dev server (all-in-one) |
| `wppackages serve` | Start HTTP server |
| `wppackages migrate` | Apply database migrations |
| `wppackages discover` | Discover package slugs from WordPress.org |
| `wppackages update` | Fetch/store package metadata |
| `wppackages build` | Generate Composer repository artifacts |
| `wppackages deploy` | Promote build (local symlink and/or R2 sync) |
| `wppackages pipeline` | Run full discover → update → build → deploy |
| `wppackages aggregate-installs` | Recompute install counters |
| `wppackages cleanup-sessions` | Remove expired admin sessions |
| `wppackages admin create` | Create admin user |
| `wppackages admin promote` | Grant admin role to existing user |
| `wppackages admin reset-password` | Reset user password |

All commands accept `--config`, `--db`, and `--log-level` flags. See [`operations.md`](operations.md) for usage details.

## Configuration

Env-first with optional YAML config file (env overrides YAML).

| Variable | Default | Purpose |
|----------|---------|---------|
| `APP_URL` | — | App domain for `notify-batch` URL |
| `DB_PATH` | `./storage/wppackages.db` | SQLite database path |
| `WP_COMPOSER_DEPLOY_R2` | `false` | Enable R2 deploy |
| `R2_ACCESS_KEY_ID` | — | R2 credentials |
| `R2_SECRET_ACCESS_KEY` | — | R2 credentials |
| `R2_BUCKET` | — | R2 bucket name |
| `R2_ENDPOINT` | — | R2 S3-compatible endpoint |
| `SESSION_LIFETIME_MINUTES` | `7200` | Admin session lifetime |

## Technical Decisions

| Area | Choice |
|------|--------|
| CLI | Cobra |
| HTTP router | `net/http` (stdlib) |
| Migrations | Goose (SQL-first) |
| Templates | `html/template` + Tailwind |
| Logging | `log/slog` |
| SQLite driver | `modernc.org/sqlite` |
| R2 | AWS SDK for Go v2 |
| Config | env-first + optional YAML |
| Admin access | In-app auth (email/password + session) |

## Testing with a Local WordPress Install

You can test the local dev server against a live [Bedrock](https://github.com/roots/bedrock) WordPress install:

```bash
composer create-project roots/bedrock
```

Then configure the Bedrock project to use your local packages server:

```bash
composer config repositories.wp-packages composer http://localhost:8080
composer config secure-http false
```

This points the Bedrock install at your local dev server and allows non-HTTPS requests.

## Make Targets

```bash
make dev    # Build + bootstrap + serve (live-reload via air)
make test   # Run all tests
make build  # Build binary
make clean  # Remove artifacts
```
