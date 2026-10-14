# Operations

Day-to-day operation of WP Packages in production-like environments.

## Prerequisites

- Ubuntu 24.04 LTS server
- Go binary (`wppackages`) deployed to the server
- SQLite database at configured path (default `./storage/wppackages.db`)
- Caddy configured as reverse proxy for app routes
- systemd service for `wppackages serve`
- systemd timers or cron for periodic commands

All prerequisites are automated via Ansible. See [Server Provisioning](#server-provisioning) below.

## Core Commands

### Migrate

```bash
wppackages migrate
```

Apply pending database migrations. Run this before first use and after upgrades.

### Discover

```bash
wppackages discover --source=svn
wppackages discover --source=config --type=plugin --limit=100
```

- `--source=svn` for full WordPress.org directory discovery.
- `--source=config` for curated seed lists. It doesn't read the SVN changelog, so changed tags don't reset `published-time`; production must use `--source=svn`.
- `--type plugin|theme|all` to filter by package type.
- `--concurrency` to control parallel fetches.

### Update

```bash
wppackages update
wppackages update --type=plugin --name=akismet --force
wppackages update --include-inactive
```

- Fetches full package payloads from WordPress.org and normalizes versions.
- `update` and `discover` take the pipeline lock when run on their own, so they fail fast instead of interleaving with a running pipeline.
- Stamps `last_sync_run_id` for snapshot-consistent builds.
- `--force` re-fetches even if package hasn't changed.
- `--concurrency` to control parallel fetches.

### Build

```bash
wppackages build
wppackages build --force
wppackages build --package=wp-plugin/akismet
```

- Generates build artifacts (`p2/` files, `packages.json`, `manifest.json`).
- Validates artifact integrity before marking build as successful.
- `--output` to specify a custom build output directory.

### Deploy

```bash
wppackages deploy
wppackages deploy 20260313-140000
wppackages deploy --rollback
wppackages deploy --rollback=20260313-130000
wppackages deploy --to-r2
wppackages deploy --cleanup
wppackages deploy --cleanup --retain 10
```

- Validates build artifacts before any sync or promotion.
- `--to-r2` uploads `p2/` files and `packages.json` to R2, then promotes locally. If R2 sync fails, the local symlink is not updated.
- Rollback validates target build, syncs to R2 if enabled, then promotes.
- `--cleanup` removes old local builds beyond retention (default: 5 beyond current).
- `--retain N` controls how many local builds to keep beyond current (minimum: 5).

### Full Pipeline

```bash
wppackages pipeline
wppackages pipeline --skip-discover
wppackages pipeline --skip-deploy
wppackages pipeline --discover-source=config
```

Runs discover → update → build → deploy sequentially, stopping on failure. After a successful deploy, automatically cleans up old local builds (keeps 5 most recent). Cleanup is best-effort — failures are logged as warnings but do not fail the pipeline.

### Backfill Published Times

```bash
wppackages backfill-published-times
wppackages backfill-published-times --days=30 --type=plugin
```

One-time step after deploying migration 031. Populates `published_times_json` (served as Composer's `published-time`, used by the Composer 2.11+ cooldown policy) for packages that don't have it yet. Scans the SVN changelog backwards from the current SVN head for `--days` (default 30): versions whose tag (or whole tags directory) was created or changed in that window get that commit date, all others get the start of the window. Each chunk must contain every revision in its range; if the changelog endpoint lags or truncates, the run fails and can simply be rerun. `--chunk-size` is capped at 50000, the per-request log limit. Until a package is backfilled, `update` leaves its published times unset and its metadata omits `published-time`.

Runs under the pipeline lock and recomputes `content_hash`, so the next pipeline deploy re-uploads every synced package to R2. Stop `wppackages-pipeline.timer` for the duration (a 30-day plugin scan downloads over a GB of changelog); otherwise each scheduled run fails on the lock and fires the failure notifier. Start it again afterwards.

### Aggregate Installs

```bash
wppackages aggregate-installs
```

Recomputes `wp_packages_installs_total`, `wp_packages_installs_30d`, and `last_installed_at` on all packages.

### Check Status

```bash
wppackages check-status
wppackages check-status --type=plugin
wppackages check-status --concurrency=20
```

Re-checks all packages against the WordPress.org API to detect closures and re-openings. Deactivates packages that return `closed` and reactivates inactive packages that return valid data. Results are recorded in the `status_checks` and `status_check_changes` tables and visible on the public `/status` page.

### Cleanup Sessions

```bash
wppackages cleanup-sessions
```

Deletes expired rows from the `sessions` table.

## Scheduled Tasks

Configure via systemd timers or cron:

| Command | Schedule | Notes |
|---------|----------|-------|
| `wppackages pipeline` | Every 5 minutes | Main data refresh cycle |
| `wppackages aggregate-installs` | Hourly | Telemetry counter rollups |
| `wppackages check-status` | Hourly | Detect closed/reopened packages on wp.org |
| `wppackages cleanup-sessions` | Daily | Expired session cleanup |
| `wppackages generate-og` | Daily | Regenerate OG images where install counts changed |

Alternatively, run `wppackages serve --with-scheduler` to handle scheduling in-process.

## Global Flags

All commands accept:

- `--config <path>` — optional config file path
- `--db <path>` — database path (default `./storage/wppackages.db`)
- `--log-level info|debug|warn|error` — log verbosity

## Key Environment Variables

| Variable | Purpose |
|----------|---------|
| `APP_URL` | Used in `notify-batch` URL in `packages.json` |
| `DB_PATH` | SQLite database file path |
| `WP_COMPOSER_DEPLOY_R2` | Enable R2 sync on deploy |
| `R2_ACCESS_KEY_ID` | R2 API credentials |
| `R2_SECRET_ACCESS_KEY` | R2 API credentials |
| `R2_BUCKET` | R2 bucket name |
| `R2_ENDPOINT` | R2 S3-compatible endpoint URL |
| `LITESTREAM_BUCKET` | R2 bucket for Litestream DB backup |
| `DISCORD_WEBHOOK_URL` | Discord webhook for pipeline failure notifications (optional) |

## Admin Bootstrap

### Create initial admin user

```bash
echo 'secure-password' | wppackages admin create --email admin@example.com --name "Admin" --password-stdin
```

`--password-stdin` is required. Password must be piped via stdin to avoid shell history exposure.

### Promote existing user to admin

```bash
wppackages admin promote --email user@example.com
```

### Reset admin password

```bash
echo 'new-password' | wppackages admin reset-password --email admin@example.com --password-stdin
```

## Admin Access Model

Admin access is protected by in-app authentication (email/password) and admin authorization for all `/admin/*` routes.

## Server Provisioning

Ansible playbooks handle server setup and application deployment. Playbooks are in `deploy/ansible/`.

### Setup

```bash
cd deploy/ansible
python3 -m venv .venv
.venv/bin/pip install -r requirements.txt
source .venv/bin/activate
```

### Provision (full server setup)

```bash
ansible-playbook provision.yml
```

Provisions: Go binary, SQLite, Caddy (reverse proxy + TLS), systemd service, systemd timers.

### Deploy (code only)

```bash
ansible-playbook deploy.yml
```

### Inventory

The production inventory defines the target server. It is gitignored.

```bash
cp inventory/hosts/production.example.yml inventory/hosts/production.yml
```

Edit `production.yml` and fill in `ansible_host` (server IP or hostname) and `ansible_user`.

### Ansible Vault

Secrets used by Ansible are stored in an encrypted vault file. The vault password file (`.vault_pass`) and decrypted vault (`vault.yml`) are gitignored.

All `ansible-vault` commands require the venv:

```bash
cd deploy/ansible
source .venv/bin/activate
```

**Decrypt** (to view or edit as plain YAML):

```bash
ansible-vault decrypt group_vars/production/vault.yml
```

**Encrypt** (after editing):

```bash
ansible-vault encrypt group_vars/production/vault.yml
```

**Edit in-place** (decrypts, opens in `$EDITOR`, re-encrypts on save):

```bash
ansible-vault edit group_vars/production/vault.yml
```

**View** (prints decrypted contents without modifying the file):

```bash
ansible-vault view group_vars/production/vault.yml
```

See `vault.example.yml` for the expected keys.

### GitHub Secrets

The deploy workflow materializes secrets at runtime. These are stored as environment secrets under the `production` environment:

| Secret | Source |
|--------|--------|
| `ANSIBLE_VAULT_PASSWORD` | Contents of `.vault_pass` |
| `PROD_INVENTORY_YML_B64` | Base64-encoded `inventory/hosts/production.yml` (contains server host/IP) |
| `PROD_SSH_PRIVATE_KEY` | SSH private key for the deploy user |
| `PROD_VAULT_YML_B64` | Base64-encoded encrypted `group_vars/production/vault.yml` |

**Updating secrets after a vault change:**

```bash
# Re-encrypt vault first if decrypted
ansible-vault encrypt group_vars/production/vault.yml

# Update the base64-encoded vault secret
base64 < group_vars/production/vault.yml | gh secret set PROD_VAULT_YML_B64 --env production
```

**Updating other secrets:**

```bash
# Vault password
gh secret set ANSIBLE_VAULT_PASSWORD --env production < .vault_pass

# Inventory (e.g. after server IP change)
base64 < inventory/hosts/production.yml | gh secret set PROD_INVENTORY_YML_B64 --env production

# SSH key
gh secret set PROD_SSH_PRIVATE_KEY --env production < /path/to/private/key
```

## Litestream (SQLite Backup to R2)

[Litestream](https://litestream.io/) continuously replicates the SQLite database to R2 by streaming WAL changes. In production it wraps `wppackages serve` as a sidecar process — if the child dies, Litestream exits and systemd restarts both.

### How it works

The systemd service runs:

```
litestream replicate -config /srv/wp-packages/shared/litestream.yml -exec "wppackages serve ..."
```

WAL segments are uploaded to R2 every 60 seconds. A full snapshot is taken every 24 hours. Segments older than 24 hours are automatically purged.

### Restore locally

Pull a production snapshot to bootstrap your local database:

```bash
make db-restore
```

Or directly:

```bash
wppackages db restore --force
```

Requires `litestream` in PATH (`brew install litestream` on macOS) and the following env vars set:
- `LITESTREAM_BUCKET`
- `R2_ENDPOINT`
- `R2_ACCESS_KEY_ID`
- `R2_SECRET_ACCESS_KEY`

The `--output`/`-o` flag overrides the restore path. The `--force` flag is required if the target DB already exists. Migrations run automatically after restore.

### Check snapshots

```bash
litestream snapshots -config litestream.yml
```

## SQLite Operations

### Backup

SQLite in WAL mode supports safe file-level backups:

```bash
sqlite3 /path/to/wppackages.db ".backup '/path/to/backup.db'"
```

Or use filesystem snapshots. The WAL file (`wppackages.db-wal`) must be included in any backup.

### Runtime Settings

Applied automatically on startup:

```sql
PRAGMA journal_mode=WAL;
PRAGMA synchronous=NORMAL;
PRAGMA foreign_keys=ON;
PRAGMA busy_timeout=5000;
```

## Common Recovery Steps

1. **Build fails integrity validation:**
   - Re-run with `--force`, inspect build output and manifest.

2. **Bad promotion:**
   - Use `wppackages deploy --rollback` or rollback to an explicit build ID.

3. **R2 sync failed mid-upload:**
   - Local symlink was not updated, so local state is consistent.
   - Re-run `wppackages deploy --to-r2` to retry.

4. **Telemetry counters stale:**
   - Run `wppackages aggregate-installs` manually.

5. **Expired sessions accumulating:**
   - Run `wppackages cleanup-sessions` manually or verify the timer/cron is active.

6. **Database locked errors:**
   - Verify WAL mode is active: `sqlite3 wppackages.db "PRAGMA journal_mode;"`
   - Check `busy_timeout` is set.
   - Ensure only one `wppackages serve` process is running.
