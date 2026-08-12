# Incremental Sync Architecture Plan

Replaces the current build-directory pipeline (discover → update → build → deploy → R2 sync) with a manifest-free, DB-driven architecture that uploads directly from SQLite to R2 with no intermediate files.

Composer v1 support is already dropped. Only Composer v2's `metadata-url` resolution is supported. Each package produces two p2/ files: tagged versions in `{name}.json` and dev/branch versions in `{name}~dev.json` (see [#24](https://github.com/roots/wp-packages/issues/24)).

## Current state

The project was renamed from `wpcomposer` to `wppackages`. The binary, CLI commands, module path (`github.com/roots/wp-packages`), and telemetry columns (`wp_packages_installs_*`) all reflect this rename.

### What's already done

- **Composer v1 dropped**: No p/ files generated, no provider-includes, no provider groups served. Integration tests validate v2-only output.
- **`provider_group` dropped** (migrations 017 + 019): Index dropped in 017, column formally dropped in 019.
- **Telemetry columns renamed** (migration 018): `wp_composer_installs_total` → `wp_packages_installs_total`, `wp_composer_installs_30d` → `wp_packages_installs_30d`.
- **Test infrastructure largely built** (Phase 4a, 4b, partial 4c/4d):
  - `internal/wporg/mock_server.go` — reusable mock wp.org API server serving fixtures by slug.
  - `internal/wporg/testdata/` — fixtures for akismet, classic-editor, contact-form-7, astra, twentytwentyfive.
  - `internal/testutil/testdb.go` — `OpenTestDB()` (in-memory DB + migrations) and `SeedFromFixtures()` (discover + update against mock server).
  - `internal/integration/smoke_test.go` — end-to-end: build repo → serve → validate Composer metadata format + `composer install` + notify-batch webhook.
  - `internal/integration/sync_test.go` — R2 sync against gofakes3: validates only p2/ + packages.json uploaded, change detection skips unchanged files, no p/ files synced.
  - `internal/integration/helpers_test.go` — shared test utilities (HTTP helpers, composer runner).
  - `gofakes3` already in `go.mod`.
- **OG image generation refactored** (merged):
  - Moved from on-demand HTTP handler generation to batch generation in pipeline (after build, before deploy).
  - New `og.GenerateNew()` generates first-time OG images for packages where `og_image_generated_at IS NULL`.
  - Injected `ImageRenderer` function type for testability.
  - Fixed SQLite deadlock pattern (collect rows before writing).
  - Removed `ogSem` semaphore and `generatePackageOG()` from handlers.
  - New test suite in `internal/og/generate_test.go`.
  - CI gains separate `gofmt` and `vet` jobs.
- **Dev branch split** ([#65](https://github.com/roots/wp-packages/pull/65)):
  - Plugins produce `{name}.json` (tagged versions) and `{name}~dev.json` (`dev-trunk`).
  - Themes produce only `{name}.json` (no `~dev.json`) — themes don't have trunk in their versions map.
  - Split happens at build time in `internal/repository/builder.go`.
- **`dev-trunk` dist removed, SVN revision pinning** ([#69](https://github.com/roots/wp-packages/pull/69)):
  - `dev-trunk` entries omit `dist` — source-only, so Composer uses SVN checkout.
  - SVN references pin to revision (`trunk@<rev>`) for reproducible `composer.lock`.
  - New `trunk_revision` column on `packages` table (migration 023).
- **Metadata changes feed** (Phase 6, partially done):
  - `metadata_changes` event log table (migration 020) populated by `build.go` after filesystem change detection.
  - `GET /metadata/changes.json` endpoint with dedup, 24h retention, resync action (`internal/http/changes.go`).
  - `metadata-changes-url` included in generated `packages.json`.
  - Transitional implementation — tightly coupled to the build step's filesystem byte comparison. Will be replaced by a `content_changed_at` query when Phase 3 eliminates the build step (see Phase 6).
- **`monthly_installs` table** (migration 022): Tracks monthly installation counts for telemetry.
- **`dev.go` deleted, `make dev` split** (Phase 2c, done early since it's independent of the refactor):
  - `cmd/wppackages/cmd/dev.go` deleted — orchestration moved to Makefile.
  - `make dev-bootstrap`: one-time setup (migrate → admin create → discover → update → build → deploy).
  - `make dev` + air: rebuild binary → migrate → serve on file changes (no pipeline re-execution).
  - `admin create` made idempotent (returns early if user exists).
  - Stale `ADMIN_ALLOW_CIDR` env var removed from `.air.toml`.

- **Phase 1 complete** ([#74](https://github.com/roots/wp-packages/pull/74)): migration 024 adds `content_hash` / `deployed_hash` / `content_changed_at`. `internal/composer/` extracted as pure serialization + hashing. `content_hash` computed in the update step.
- **Phase 2 complete** ([#90](https://github.com/roots/wp-packages/pull/90)): `/packages.json` and `/p2/{vendor}/{file}` served from SQLite. `packages.json` content embedded at `internal/composer/packages.json`.
- **`content_changed_at` on deactivate/reactivate** ([#93](https://github.com/roots/wp-packages/pull/93)): closures and re-openings now reach the changes feed.
- **`deploy.Sync()` written and tested** ([#96](https://github.com/roots/wp-packages/pull/96)): DB-driven R2 sync, plus `composer.PackageFiles()`, `Package.ComposerMeta()`, `GetDirtyPackages()`, `withRetry()`, configurable concurrency. **Additive — not yet called by anything in production.**
- **R2 deletion for closed packages** ([#108](https://github.com/roots/wp-packages/pull/108), [#109](https://github.com/roots/wp-packages/pull/109)): Phase 5a/5b, landed early against the old deploy path.

### What's NOT done yet

- **The Phase 3 cutover has not happened** — `deploy.Sync()` is referenced only by `internal/integration/db_sync_test.go`. The pipeline still runs discover → update → build → deploy, and `cmd/deploy.go` still calls `SyncToR2()` against the build directory.
- **`content_hash` does not cover the full serialized output** — hashes `versions_json` + `trunk_revision` only, while the p2 files also embed `description`, `homepage`, `author`, `last_committed`. Blocks cutover; see [Cutover risks](#cutover-risks).
- **Build step still exists** — `cmd/wppackages/cmd/build.go` materializes p2/ files on disk.
- **`internal/deploy/local.go` still exists** — symlinks, build promotion, rollback.
- **`internal/repository/` still exists** — superseded by `internal/composer/` but still the build path's serializer.
- **`builds` / `sync_runs` not yet merged into `pipeline_runs`.**
- **No set-difference GC for orphaned R2 objects, no legacy `p/` cleanup.**
- **`smoke_test.go` still tests the filesystem architecture** — Phase 2 shipped DB-backed serving but the test was not migrated to it.
- **Metadata changes feed is transitional** — the `metadata_changes` event log and `/metadata/changes.json` endpoint work, but change detection is coupled to the build step's filesystem comparison. Moves to a `content_changed_at` query when `metadata_changes` is dropped (Phase 3f).

## Current problems

- **Build dir is vestigial**: The pipeline materializes ~140k files on disk (p2/) per build. This was the original source of truth for R2 uploads but is no longer necessary since Composer v1 was dropped.
- **Manifest is a metrics summary, not a source of truth**: `manifest.json` stores timing/counts but not per-package hashes. Every operation rediscovers state by walking the filesystem.
- **R2 sync is O(total), not O(changed)**: Walks the full build directory, byte-compares p2/ files. With ~70k packages, that's ~70k stat/read operations per sync.
- **p2/ files always rewritten**: Every build writes all p2/ files even when identical to the previous build.
- **Metadata change detection is coupled to the build step**: The `metadata_changes` event log is populated by filesystem byte comparison in `build.go`. When the build step is removed, this change detection mechanism disappears.

## New architecture

### Pipeline steps

The pipeline has three logical steps, each with a distinct role:

- **Discover** = "what packages exist, and which ones changed?" Creates shell rows (type, name, last_committed) with no metadata. Two sources: seed config file (small, for dev/CI) or SVN listing (~70k slugs, production). SVN discovery also fetches the SVN revision log to detect which packages changed since the last run, marking them for update. Discover is cheap — it never fetches full API data.

- **Update** = "fetch full metadata from wp.org for packages that need it." Takes packages marked as needing update (new, changed, or forced), fetches their full wp.org API data, normalizes versions, computes `content_hash`, writes to DB. This is the expensive step — bounded by wp.org API throughput. In production, discover might find 70k slugs but only mark ~200 as changed, so update only fetches those ~200.

- **Sync** = "push what changed to R2." Diff query finds packages where `content_hash != deployed_hash`, serializes them into two files (tagged `{name}.json` + dev `{name}~dev.json`), uploads to R2, stamps `deployed_hash`. Replaces the old build + deploy steps.

### Core principle: the DB is the source of truth

Three new columns on the `packages` table:

| Column | Set when | Purpose |
|--------|----------|---------|
| `content_hash` | `update` step (after version normalization) | SHA256 of the full deterministic `versions_json` (covers both tagged + dev output files) |
| `deployed_hash` | After successful R2 sync | Hash as of last deploy |
| `content_changed_at` | `update` step (only when `content_hash` changes) | When the composer-facing metadata last changed |

The "build" is a diff query:

```sql
SELECT type, name, versions_json, content_hash, ...
FROM packages
WHERE is_active = 1
  AND (deployed_hash IS NULL OR content_hash != deployed_hash)
```

No build directories. No manifest file. No filesystem walking.

### Dev branch split — IMPLEMENTED

Composer v2 resolves tagged versions from `/p2/{name}.json` and dev/branch versions from `/p2/{name}~dev.json`. Each package produces up to two R2 objects:

| File | Versions included | `dist` | `source` |
|------|-------------------|--------|----------|
| `p2/wp-plugin/akismet.json` | Tagged (`1.0.0`, `5.3.7`, etc.) | zip URL | SVN reference (`tags/1.0.0`) |
| `p2/wp-plugin/akismet~dev.json` | `dev-trunk` | **omitted** | SVN reference (`trunk@<rev>`) |

Themes produce only `{name}.json` — they don't have trunk in their versions map.

Omitting `dist` on trunk is a correctness fix: trunk is mutable, so the zip URL isn't stable. Composer falls back to `source` (SVN checkout) and locks the SVN revision via `trunk@<rev>`, giving users a reproducible `composer.lock`. The `trunk_revision` column (migration 023) stores the pinned SVN revision.

**Split happens at build time.** The DB stores one `versions_json` blob per package (all versions). `internal/repository/builder.go` splits versions at build time — `dev-trunk` goes to `~dev.json`, everything else to the main file. When the `internal/composer/` package is extracted (Phase 1b), this moves to `composer.SerializePackage()`.

**One hash pair, not two.** `content_hash` will be computed over the full `versions_json` (all versions). If any version changes, both output files are re-uploaded. This trades a slight over-upload (~1 extra small file per changed package) for much simpler tracking — no need for 4 hash columns or split-aware diff queries. With ~200 packages changing per production run, the cost is negligible.

**Packages with no dev versions** (common — many plugins never have trunk in their versions map) simply don't get a `~dev.json` file. The sync step skips the upload; the serve layer returns 404. If a package previously had dev versions and loses them, the sync step deletes the orphaned `~dev.json`.

### Full packages table schema

```
packages
────────────────────────────────────────────────────────────
-- Identity
id                          INTEGER PRIMARY KEY
type                        TEXT NOT NULL (plugin|theme)
name                        TEXT NOT NULL
UNIQUE(type, name)

-- Display metadata (from wp.org API, used by web UI)
display_name                TEXT
description                 TEXT
author                      TEXT
homepage                    TEXT
slug_url                    TEXT
rating                      REAL
num_ratings                 INTEGER NOT NULL DEFAULT 0
downloads                   INTEGER NOT NULL DEFAULT 0
active_installs             INTEGER NOT NULL DEFAULT 0

-- Composer metadata (drives the repository)
versions_json               TEXT NOT NULL DEFAULT '{}'
current_version             TEXT
trunk_revision              INTEGER         ← EXISTS (migration 023)
content_hash                TEXT            ← NEW
deployed_hash               TEXT            ← NEW
content_changed_at          TEXT            ← NEW

-- Sync state
is_active                   INTEGER NOT NULL DEFAULT 1
last_committed              TEXT            wp.org's last_updated timestamp
last_synced_at              TEXT            when we last fetched from wp.org API
last_sync_run_id            INTEGER         which sync run touched this row

-- Telemetry (our install tracking)
wp_packages_installs_total  INTEGER NOT NULL DEFAULT 0
wp_packages_installs_30d    INTEGER NOT NULL DEFAULT 0
last_installed_at           TEXT

-- OG images
og_image_generated_at       TEXT
og_image_installs           INTEGER NOT NULL DEFAULT 0
og_image_wp_installs        INTEGER NOT NULL DEFAULT 0

-- Housekeeping
created_at                  TEXT NOT NULL
updated_at                  TEXT NOT NULL
```

The `provider_group` column has been fully dropped (index in migration 017, column in migration 019).

### Pipeline runs table

The current `builds` and `sync_runs` tables are combined into a single `pipeline_runs` table. Currently `builds` tracks pipeline execution (step durations, status, PID) *and* build artifacts (root_hash, manifest_json, artifact_count) — the artifact half goes away when the build step is deleted. And `sync_runs` tracks wp.org API fetch batches separately, but there's no use case for a standalone update without syncing — the whole point of fetching is to serve updated data.

One table, one concept: "a pipeline run happened, here's what it fetched, what it synced, how long each step took."

```
pipeline_runs
────────────────────────────────────────────────────────────
-- Identity + execution
id                  TEXT PRIMARY KEY        (timestamp-based, e.g. "20260319-143022")
started_at          TEXT NOT NULL
finished_at         TEXT
status              TEXT NOT NULL            (running | completed | failed | cancelled)
pid                 INTEGER
error_message       TEXT

-- Snapshot (point-in-time, not derivable from current DB state)
packages_total      INTEGER NOT NULL DEFAULT 0   ← active packages at time of run

-- What changed
packages_updated    INTEGER NOT NULL DEFAULT 0   ← packages whose content_hash changed during update
packages_uploaded   INTEGER NOT NULL DEFAULT 0   ← p2/ files uploaded to R2 (1-2 per package: tagged + optional dev)
packages_deleted    INTEGER NOT NULL DEFAULT 0   ← deactivated packages removed from R2
packages_skipped    INTEGER NOT NULL DEFAULT 0   ← packages checked but unchanged
og_images_generated INTEGER NOT NULL DEFAULT 0   ← OG images created this run

-- Step durations (seconds, nullable = step didn't run)
discover_seconds    INTEGER
update_seconds      INTEGER
sync_seconds        INTEGER                      ← replaces build_seconds + deploy_seconds + r2_upload_seconds
og_seconds          INTEGER
total_seconds       INTEGER
```

The `packages.last_sync_run_id` FK points at this table instead of the old `sync_runs` table.

The admin UI stays nearly the same — the builds page becomes "Pipeline Runs" with updated column headers (Updated/Uploaded instead of Changed/Artifacts). The PID-based locking, stale run detection, and admin trigger-via-subprocess pattern all work unchanged.

`make dev` does not create pipeline_runs rows — individual CLI commands (discover, update) run without run tracking. Only the `pipeline` command creates and records runs.

### Update step logic for the new columns

```go
newHash, _, _ := composer.SerializePackage(...)  // only need the hash here, not the files
if newHash != pkg.ContentHash {
    pkg.ContentHash = newHash
    pkg.ContentChangedAt = time.Now().UTC()
}
pkg.LastSyncedAt = time.Now().UTC()
```

`last_synced_at` = "when we last fetched from wp.org" (can happen without content changing).
`content_changed_at` = "when the composed output last changed" (only advances on actual diff).
`deployed_hash` = "what's live on R2" (stamped after successful sync).

### Sync

```
Step 1: Upload changed p2/ files
        For each changed package, upload up to 2 files:
          - {name}.json (tagged versions — always)
          - {name}~dev.json (dev versions — only if package has dev-* versions)
        Composer 2 clients see updates immediately via metadata-url.

Step 2: Delete p2/ files for deactivated packages.
        Delete both {name}.json and {name}~dev.json.

Step 3: Upload packages.json (conditional)
        PUT with If-None-Match: <ETag> — skips the upload if content hasn't changed.
        Costs one HEAD/PUT per sync but ensures any change to the static
        content (new field, URL change, etc.) propagates automatically.

Step 4: Stamp deployed_hash
        UPDATE packages SET deployed_hash = content_hash
        WHERE is_active = 1 AND content_hash != deployed_hash;

        UPDATE packages SET deployed_hash = NULL
        WHERE is_active = 0 AND deployed_hash IS NOT NULL;
```

### Crash safety

- **Crash during step 1**: Some p2/ files updated, others stale. Not corrupt, just partial. A package might have its tagged file updated but not its dev file (or vice versa) — both are independently valid. Next run completes.
- **Crash between 1 and 4**: R2 has updated files but DB hasn't been stamped. Next run redundantly re-uploads, then stamps. No inconsistency — p2/ files are independent, not linked through a hash tree.

Invariant: `deployed_hash` is always stamped *after* all R2 uploads succeed. The DB can lag behind R2 (causing one redundant sync) but never lead it.

### Root `packages.json`

Effectively static — changes rarely (only when we add a field or change a URL):

```json
{
  "packages": [],
  "metadata-url": "/p2/%package%.json",
  "metadata-changes-url": "/metadata/changes.json?since=%since%",
  "notify-batch": "/downloads",
  "available-package-patterns": ["wp-plugin/*", "wp-theme/*"]
}
```

`metadata-changes-url` is already included — the changes feed endpoint exists (see Phase 6).

Uploaded conditionally on every sync via `If-None-Match` against the existing R2 object's ETag. In practice this is a no-op (one cheap HEAD) on every run, but means any change to the generated content propagates automatically without a manual step.

### Local serving (dev + CI)

The HTTP server serves composer metadata directly from the DB — no build directory or symlink needed:

- `GET /p2/{type}/{name}.json` — serialize tagged versions from `versions_json` on the fly
- `GET /p2/{type}/{name}~dev.json` — serialize dev versions (404 if none exist)
- `GET /packages.json` — return static JSON (hardcoded or from config)
- `GET /metadata/changes.json` — currently queries `metadata_changes` event log; will switch to `content_changed_at` query (Phase 6)

### Prior art

This architecture draws from several well-known systems:

- **Nix Store**: Content-addressed outputs, closures as sets of store paths, GC by reachability from roots. Builds as pure functions of inputs — same `versions_json` always produces the same hash.
- **Terraform State**: `content_hash` = desired state, `deployed_hash` = last-known state. The diff query = plan. The sync = apply. Idempotent apply means crash recovery is just re-run.

---

## Package structure

### Current → new mapping

| Current package | Current purpose | New reality |
|---|---|---|
| `internal/repository/` | `builder.go` (build orchestration), `composer.go` (format helpers), `hasher.go` (deterministic JSON) | Builder deleted. Renamed to `internal/composer/` — pure functions for Composer format, serialization, hashing. No I/O. |
| `internal/deploy/` | `local.go` (symlinks, build dirs) + `r2.go` (R2 sync, layout detection, file walking) | `local.go` deleted. R2 gutted and rebuilt around DB-driven sync. |
| `internal/packages/` | Entity, DB ops, API mapping, seeds, site_meta | Gains `content_hash` / `deployed_hash` / `content_changed_at` columns. |
| `internal/http/` | Web UI handlers + serves repo files from filesystem | Gains `composer.go` with DB-backed composer metadata handlers. |
| `cmd/.../build.go` | Orchestrates filesystem build | Deleted. |
| `cmd/.../deploy.go` | Promote symlink + R2 sync + rollback + cleanup | Simplified — default action is sync to R2, flag for cleanup. |
| `cmd/.../pipeline.go` | discover → update → build → deploy → OG | discover → update → sync → OG. |
| `cmd/.../dev.go` | 7-step bootstrap: migrate → admin → discover → update → build → promote → serve | **Deleted.** `make dev-bootstrap` + `make dev` compose existing CLI commands. |

### New directory layout

```
internal/
  ├── app/              unchanged
  ├── auth/             unchanged
  ├── composer/         NEW NAME (replaces repository/)
  │   ├── format.go       ComposerVersion, ComposerName, DownloadURL, PackageMeta
  │   ├── serialize.go    SerializePackage → (hash, PackageFiles) — splits tagged/dev, omits dist on dev
  │   └── hash.go         DeterministicJSON, HashJSON
  ├── config/           unchanged
  ├── db/               unchanged
  ├── deploy/           RESTRUCTURED
  │   ├── sync.go         Sync() — diff query → serialize → upload → stamp
  │   ├── r2.go           putObjectWithRetry, newS3Client, CacheControlForPath
  │   └── cleanup.go      CleanupOrphanedP2Files()
  ├── http/             GAINS composer handlers
  │   ├── composer.go     NEW — /packages.json, /p2/* handlers
  │   ├── router.go       updated — new handlers replace filesystem block
  │   ├── handlers.go     web UI (OG generation removed on og-generation-refactor branch)
  │   └── ...             rest unchanged
  ├── og/               REFACTORED (merged)
  │   ├── og.go           image rendering (unchanged)
  │   ├── generate.go     GenerateAll, GenerateNew (batch pipeline generation)
  │   ├── generate_test.go  tests with injected ImageRenderer
  │   └── upload.go       R2/local upload
  ├── packages/         GAINS hash columns, DROPS sync_runs
  │   ├── package.go      + content_hash, deployed_hash, content_changed_at
  │   ├── sync.go         reworked — AllocateSyncRunID/FinishSyncRun operate on pipeline_runs
  │   ├── api_mapper.go   unchanged
  │   ├── seeds.go        unchanged
  │   └── site_meta.go    unchanged
  ├── packagist/        unchanged
  ├── telemetry/        unchanged
  ├── testutil/         EXISTS — testdb.go (OpenTestDB, SeedFromFixtures)
  ├── version/          unchanged
  └── wporg/            EXISTS — mock + fixtures already built
      ├── client.go       unchanged
      ├── svn.go          unchanged
      ├── mock_server.go  EXISTS — reusable mock server for tests
      └── testdata/       EXISTS — plugins/ and themes/ fixtures

cmd/wppackages/cmd/
  ├── root.go               unchanged
  ├── serve.go              unchanged
  ├── discover.go           unchanged
  ├── update.go             gains content_hash + content_changed_at computation
  ├── deploy.go             SIMPLIFIED — default action is sync, flag for cleanup
  ├── pipeline.go           SIMPLIFIED — discover → update → sync → OG; records pipeline_runs
  ├── dev.go                DELETED — orchestration moved to `make dev-bootstrap` + `make dev`
  ├── admin.go              unchanged
  ├── migrate.go            unchanged
  ├── aggregate_installs.go unchanged
  ├── cleanup_sessions.go   unchanged
  ├── generate_og.go        unchanged
  └── build.go              DELETED

internal/integration/       EXISTS — needs updating for new architecture
  ├── smoke_test.go         currently tests filesystem-based build+serve
  ├── sync_test.go          currently tests filesystem-based R2 sync via gofakes3
  ├── helpers_test.go       shared utilities
  └── wporg_live_test.go    live API test
```

### CLI surface

```
wppackages pipeline            # discover → update → sync → OG (production)
wppackages sync                # diff DB → upload changed p2/ files to R2
wppackages sync --dry-run      # report what would be uploaded/deleted, touch nothing
wppackages rehash              # recompute content_hash for all active packages (no network)
wppackages cleanup-r2-orphans  # bulk-delete R2 files for deactivated packages
make dev-bootstrap             # migrate → admin → discover → update (one-time setup)
make dev                       # air: rebuild → migrate → serve (live-reload)
```

### Key design decisions

**Dev branch split at build/serialization time, not storage** (implemented): The DB stores one `versions_json` per package. The split into `{name}.json` (tagged) and `{name}~dev.json` (dev-trunk) happens in the builder (currently `internal/repository/builder.go`, moving to `composer.SerializePackage()` in Phase 1b). Plugins only — themes have no `~dev.json`. One `content_hash` will cover both files — simpler than 4 hash columns, and the over-upload cost (~1 extra file per changed package) is negligible at ~200 changes per run.

**`dist` omitted on dev-trunk** (implemented): Trunk is mutable — a `dist` zip URL for `dev-trunk` isn't stable across SVN commits. Omitting `dist` forces Composer to use `source` (SVN checkout at `trunk@<rev>`), which locks the SVN revision for reproducible installs.

**`repository/` → `composer/`**: "Repository" is overloaded (Go repo pattern, data repository, Composer repository). `composer/` is precise — it formats data into Composer's JSON structure and hashes it. Pure functions, no I/O.

**`deploy/sync.go` as orchestration layer**: The `Sync()` function queries the DB, calls `composer.SerializePackage()`, uploads via R2 helpers, and stamps `deployed_hash`. No need for a separate package.

**No rollback mechanism**: Without v1's hash tree, there's no atomic pointer swap to roll back. Every p2/ file is independently addressable. If a bad update goes out, re-run the update step to fix the data, then sync again. For catastrophic cases, restore the DB from backup and sync.

**`http/composer.go` alongside web UI**: Both are HTTP handlers sharing the router, middleware, and app context. One new file is clean enough.

**`wporg/mock_server.go` in the same package**: The mock needs to understand wp.org API response format. Keeping it together avoids duplicating that knowledge.

### Dependency graph

```
cmd/               → deploy, composer, packages, http, wporg, og, config, app, db
internal/http/     → composer, packages, app, config
internal/deploy/   → composer, packages, config
internal/og/       → packages, config              (batch generation + upload)
internal/composer/ → version                        (pure functions, minimal deps)
internal/packages/ → version                        (entity + DB ops)
internal/wporg/    → config                         (API client)
```

Clean and acyclic. `composer/` is a leaf with no heavy dependencies.

### Deleted code

| File | Reason |
|---|---|
| `internal/repository/builder.go` | Build step eliminated. Serialization extracted to `composer/serialize.go`. |
| `internal/deploy/local.go` | No more build directories, symlinks, or local promotion. |
| `cmd/.../build.go` | No build command. |
| R2 layout detection (`fetchLiveRelease`, `liveReleaseResult`) | Already removed — was transitional code for old layout. |
| `collectSharedFiles`, `fileUnchanged`, `loadPreviousBuildHashes` | Filesystem diffing replaced by DB diff query. |
| `sortBuildFiles`, `buildFile` struct | No filesystem walking. |
| All p/ file generation, provider group file generation | v1-only artifacts (already not generated). |
| `cmd/.../dev.go` | **Already deleted.** Orchestration moved to `make dev-bootstrap` + `make dev`. |
| `builds` table | Replaced by `pipeline_runs`. |
| `sync_runs` table | Combined into `pipeline_runs`. |
| `metadata_changes` table | Change feed switches to `content_changed_at` column query. |
| `persistMetadataChanges` in `build.go` | Event log writer eliminated with build step. |
| `internal/repository/builder_test.go` | Tests filesystem-based builds. |
| `internal/deploy/local_test.go` | Tests symlink promotion, rollback, cleanup. |

**Net impact:** ~1,200 lines deleted, ~166 lines extracted and renamed (composer.go + hasher.go → `internal/composer/`), ~250 lines new (sync.go, composer handlers with tagged/dev split, pipeline_runs migration). No config changes required.

---

## Phase 1: Schema + Content Hash

**Goal:** Compute and store hashes at update time. Foundation for everything else.

**Status: DONE** — [#74](https://github.com/roots/wp-packages/pull/74), merged 2026-04-01.

Landed as described below, with two deviations from the original design:

- `SerializePackage` produces **one file per call**, not a `PackageFiles` struct — the name encodes the filter (`akismet` = tagged, `akismet~dev` = dev). `FileOutput`/`PackageFiles` as designed here never existed. A `PackageFiles()` helper returning `[]PackageFile{Key, Data}` was added later in [#96](https://github.com/roots/wp-packages/pull/96).
- Hashing is a standalone `composer.HashVersions(versionsJSON, trunkRevision)`, decoupled from serialization. **This hash does not cover everything that gets serialized** — see [Cutover risks](#cutover-risks).

`DeterministicJSON` was removed in [#90](https://github.com/roots/wp-packages/pull/90) — `json.Marshal` already sorts map keys.

### 1a. Migration: add columns

```sql
ALTER TABLE packages ADD COLUMN content_hash TEXT;
ALTER TABLE packages ADD COLUMN deployed_hash TEXT;
ALTER TABLE packages ADD COLUMN content_changed_at TEXT;
```

`provider_group` is already fully dropped (migrations 017 + 019). This migration will be 024.

### 1b. Extract serialization logic

Create the shared serialization function in the new `composer` package:

```go
// internal/composer/serialize.go

type FileOutput struct {
    Data []byte
    Key  string  // R2 object key, e.g. "p2/wp-plugin/akismet.json"
}

type PackageFiles struct {
    Tagged FileOutput  // p2/wp-plugin/akismet.json (always present)
    Dev    FileOutput  // p2/wp-plugin/akismet~dev.json (empty if no dev versions)
}

// PackageMeta includes TrunkRevision (from packages.trunk_revision) for SVN pinning
func SerializePackage(pkgType, name string, versionsJSON string, meta PackageMeta) (hash string, files PackageFiles, err error)
```

Splits `versions_json` into tagged vs dev versions (`dev-trunk` → dev, everything else → tagged). Builds the `{packages: {composerName: {version: ...}}}` payload for each, runs through `DeterministicJSON`. Dev versions omit `dist` — only `source` with SVN reference (`trunk@<rev>`). Plugins produce both files; themes produce only the tagged file (no `~dev.json`). The hash is SHA256 of the full deterministic `versions_json` (not the output files), so it's stable regardless of presentation format changes.

Used by the update step (hash only), the serve layer (return appropriate file), and the sync step (upload both files).

The logic currently lives in `internal/repository/` across `composer.go` (format helpers, ComposerName, DownloadURL, ComposerVersion with dev-trunk dist omission), `builder.go` (build orchestration, p2/ file generation, tagged/dev split), and `hasher.go` (deterministic JSON). The rename extracts and consolidates the pure serialization/hashing functions — builder orchestration is deleted. The existing dev-trunk split and dist omission logic in `builder.go` and `composer.go` moves into `SerializePackage()` as-is.

### 1c. Compute `content_hash` in the update step

After `NormalizeAndStoreVersions()` succeeds, call `composer.SerializePackage()` and compute the hash. If the hash differs from the existing `content_hash`, update both `content_hash` and `content_changed_at`. Add to `UpsertPackage` / `BatchUpsertPackages` write path.

### 1d. Backfill existing rows

On first run after deploy, the update step will naturally recompute all packages (since `content_hash` is NULL). Alternatively, add a `wppackages backfill-hashes` command that queries all active packages and stamps `content_hash` without fetching from wp.org.

### Files touched

- New migration in `migrations/` (024)
- `internal/composer/` (new package — `format.go`, `serialize.go`, `hash.go` extracted from `internal/repository/`)
- `internal/packages/package.go` (`UpsertPackage`, `BatchUpsertPackages` include `content_hash`, `content_changed_at`)
- `cmd/wppackages/cmd/update.go` (compute hash after normalization)

---

## Phase 2: DB-Backed Serve Layer

**Goal:** Serve composer metadata directly from the DB. Eliminates build directory dependency for local dev.

**Status: DONE** — [#90](https://github.com/roots/wp-packages/pull/90), merged 2026-04-03.

Production still serves from R2/CDN; these handlers cover local dev and define the serialization the sync step reuses. `packages.json` content moved to an embedded `internal/composer/packages.json` as the single source of truth, replacing the inline map literals that had been duplicated between builder and handler. `build`/`deploy` were dropped from `make dev-bootstrap`.

### 2a. Package endpoints: `GET /p2/{type}/{name}.json` and `GET /p2/{type}/{name}~dev.json`

Composer 2's resolution path. One handler parses the `~dev` suffix to determine which file to serve. Query `packages` by type+name, call `composer.SerializePackage()`, return the appropriate `PackageFiles` member. 404 for `~dev.json` when no dev versions exist. One row lookup + serialize per request.

### 2b. Root endpoint: `GET /packages.json`

Return static JSON:

```json
{
  "packages": [],
  "metadata-url": "/p2/%package%.json",
  "metadata-changes-url": "/metadata/changes.json?since=%since%",
  "notify-batch": "/downloads",
  "available-package-patterns": ["wp-plugin/*", "wp-theme/*"]
}
```

No DB query needed. Hardcode or load from config. `metadata-changes-url` is already generated by the build step.

### 2c. Delete `dev.go`, move to Makefile — DONE

Completed early (independent of the refactor). `dev.go` deleted, `make dev` split into `dev-bootstrap` (one-time: migrate → admin create → discover → update) and `dev` (air: rebuild → migrate → serve). `admin create` made idempotent. Stale `ADMIN_ALLOW_CIDR` removed. `build` + `deploy` dropped from bootstrap in #90 once serving no longer needed filesystem artifacts.

### 2d. Update router

Replace the filesystem-based serving block in `router.go` with the new handler functions. Currently `router.go` serves `/packages.json` and `/p2/` as static files from the `storage/repository/builds/current/` directory.

### Files touched

- `internal/http/composer.go` (new — handler implementations)
- `internal/http/router.go` (replace filesystem block with new handlers)
- `cmd/wppackages/cmd/admin.go` — ~~make `admin create` idempotent~~ done
- `cmd/wppackages/cmd/dev.go` — ~~delete~~ done
- `Makefile` — ~~split into `dev-bootstrap` + `dev`~~ done
- `.air.toml` — ~~update to `migrate && serve`~~ done

---

## Phase 3: R2 Sync (replaces build + deploy)

**Goal:** Upload directly from DB to R2. No intermediate build directory.

**Status: PARTIALLY DONE** — 3a–3d landed in [#96](https://github.com/roots/wp-packages/pull/96) (merged 2026-04-11). 3e/3f/3g are the remaining cutover.

`deploy.Sync()` exists at `internal/deploy/sync.go` and is covered by an integration test against gofakes3 (`internal/integration/db_sync_test.go`), but **nothing in production calls it**. The pipeline still runs discover → update → build → deploy, and `cmd/deploy.go` still calls the filesystem-walking `SyncToR2()`. #96 was deliberately additive.

Supporting pieces that landed with it: `composer.PackageFiles()`, `composer.ObjectKeys()`, `Package.ComposerMeta()`, `packages.GetDirtyPackages()`, `packages.GetDeactivatedDeployedPackages()`, a shared `withRetry()` helper, and configurable `R2Config.Concurrency` (default 50).

### 3a. Diff query — DONE

`packages.GetDirtyPackages()`. Note the shipped query adds `content_hash IS NOT NULL`, which the original design did not have — rows that have never been through an `update` since #74 are invisible to the sync step. See [Cutover risks](#cutover-risks).

```sql
SELECT id, type, name, versions_json, content_hash,
       description, homepage, author, last_committed, trunk_revision
FROM packages
WHERE is_active = 1
  AND content_hash IS NOT NULL
  AND (deployed_hash IS NULL OR content_hash != deployed_hash)
```

### 3b. New sync function — DONE

```go
// internal/deploy/sync.go
func Sync(ctx context.Context, db *sql.DB, cfg config.R2Config, appURL string, logger *slog.Logger) (*SyncResult, error)

type SyncResult struct {
    Uploaded int64
    Deleted  int64
    Skipped  int64
    Duration time.Duration
}
```

`Skipped` is currently always 0 — nothing increments it. Either wire it up or drop the field.

### 3c. Parallel upload with concurrency control — DONE

`errgroup` with limit from `cfg.Concurrency` (default 50). Each changed package produces 1-2 uploads (tagged always, dev only if dev versions exist).

### 3d. Conditional `packages.json` upload — DONE

Implemented as a `HeadObject` ETag comparison against `md5(packagesData)` rather than an `If-None-Match` conditional PUT. Equivalent cost (one cheap HEAD per run), and it keeps the skip decision explicit in our code rather than depending on R2's conditional-request semantics. The MD5 comparison is only valid while `packages.json` stays single-part — multipart ETags are not plain MD5 — which holds at its current size.

## Cutover risks

Three issues make a straight flip from `SyncToR2` to `Sync()` riskier than it looks. All three come from the same root cause: **the old path decided what to upload by byte-comparing generated files; the new path decides from a hash that does not cover the same inputs.**

### Risk 1: `content_hash` does not cover everything that gets serialized — BLOCKING

`composer.HashVersions()` (`internal/composer/serialize.go:71`) hashes `versions_json` + `trunk_revision`. But the serialized p2 entry (`internal/composer/format.go:77-92`) also embeds `description`, `homepage`, `authors` (from `author`), and `time` (from `last_committed`).

So a package whose description, homepage, author, or `last_committed` changes — without any version change — **never becomes dirty and its R2 file goes permanently stale.** The old build path caught this, because it regenerated every file and byte-compared. wp.org readme-only commits move `last_updated` without touching versions, so this is a routine occurrence, not a corner case.

**Fix before cutover:** hash over exactly what `PackageFiles()` serializes. Either hash the serialized bytes directly, or extend `HashVersions` to fold in the `PackageMeta` fields. Hashing the output bytes is the stronger invariant — it makes "hash changed" and "file changed" the same statement by construction, which is the property the whole design rests on.

**Consequence:** changing the hash formula makes every package dirty at once, so the first run after deploy uploads all ~70k packages instead of ~200. That is a one-time cost and it doubles as a full reconcile, but it must be a deliberate, scheduled run — not a surprise during a routine pipeline.

### Risk 2: `content_hash IS NOT NULL` silently excludes rows

`GetDirtyPackages` filters on `content_hash IS NOT NULL`. `content_hash` is only written by the update step (`cmd/update.go:176`), which only touches packages it actually fetched. Any active package that has not been through an update since #74 landed still has `content_hash = NULL` and is invisible to `Sync()` — it will never be uploaded, and no error is raised.

Meanwhile the old `SyncToR2` stamps `deployed_hash = COALESCE(content_hash, '1')` (`internal/deploy/r2.go:105`), so those same rows have the sentinel `'1'` in `deployed_hash` — which *looks* deployed. They are in fact deployed (the old build path uploaded every file), so nothing is currently broken; the hazard is that after cutover they are frozen forever and the diff query reports zero work to do.

**Before cutover, count them:** `SELECT COUNT(*) FROM packages WHERE is_active = 1 AND content_hash IS NULL`. Fixing Risk 1 requires a full rehash anyway, so run that rehash over *all* active packages from stored `versions_json` — no wp.org fetches needed — which clears this at the same time.

### Risk 3: unbounded deletion on mass deactivation

`Sync()` step 2 deletes R2 files for every row matching `is_active = 0 AND deployed_hash IS NOT NULL`, with no cap. This repo already has a mass-closure concept (mass closure history page, closure events) — a wp.org API degradation that mass-deactivates packages would translate directly into mass R2 deletion, and the files are only restored by a subsequent full re-upload.

**Fix:** refuse to delete when the deactivated count exceeds a threshold (a few hundred) unless an explicit `--allow-mass-delete` flag is passed. Log and skip rather than fail the run — uploads are the important half.

---

### 3e. Staged cutover

The cutover is sequenced so that each step is independently verifiable and reversible, and so that **no code is deleted until the new path has run correctly in production.**

**Step 1 — Fix the hash (PR 1). ✅ DONE.** `composer.HashContent()` replaces `HashVersions()` and hashes the serialized output bytes plus their object keys, so "hash changed" and "R2 is stale" are the same statement by construction (Risk 1). Shipped alongside:

- `wppackages rehash [--dry-run]` — recomputes `content_hash` for all active packages from stored data, no network calls. Clears Risk 2 at the same time.
- `packages.CountUnhashedActive()` + `SyncResult.Unhashed` — `Sync()` now warns about active rows it cannot see instead of silently reporting no work (Risk 2).
- `SyncOptions.MaxDeletes`, default 250 — refuses mass deletion and reports `DeletesSkipped` rather than failing the run, so uploads still complete (Risk 3).
- `wppackages sync [--dry-run] [--max-deletes N]` — makes the new path reachable without touching the pipeline.
- Per-package `deployed_hash` clearing on delete, replacing a blanket `UPDATE ... WHERE is_active = 0` that cleared the hash even for packages whose deletion had failed — those became permanent R2 orphans, since clearing the hash removes them from the retry query.
- Update step now mirrors `UpsertPackage`'s monotonic `last_committed` rule before hashing. Without this, a wp.org `last_updated` that moves backwards produces a hash the sync step can never satisfy, re-uploading that package on every run forever.

Behaviour is unchanged in production: nothing reads `content_hash` for upload decisions until Step 4.

**Step 2 — Byte-parity test (PR 1, same PR). ✅ DONE.** `internal/integration/serializer_parity_test.go` asserts `composer.PackageFiles()` output is byte-identical to what `repository.Build()` writes, in both directions (no missing files, no extra files), across the fixture set. Currently 8 files across 5 packages, all matching — the two serializers agree, so the new sync uploading a file is equivalent to the old build writing it.

`internal/composer/hash_test.go` and `internal/integration/sync_safety_test.go` cover the three risks directly. The metadata-only test was verified to fail against the old hash before being kept.

**Step 3 — Shadow mode (PR 2).** Run `wppackages rehash --dry-run` in production first to size the reconcile, then `rehash` for real, then `sync --dry-run` immediately after a normal build+deploy. Expected steady state: dry-run reports ~0 uploads right after a deploy. A large or persistent non-zero count means the hash and file content still disagree — investigate before going further.

Note the first real `sync` after the rehash uploads **all ~70k packages**, because the hash formula changed. That is a one-time full reconcile and should be a scheduled run, not a surprise mid-pipeline.

**Step 4 — Flip behind a flag (PR 3).** Add `--sync-mode=build|db` to the pipeline, defaulting to `build`. Flipping to `db` swaps `SyncToR2` for `Sync()` and skips the build step. Both paths stay present. Rollback is a flag change, not a revert and redeploy — this is the main reason to stage it this way.

**Step 5 — Run on `db` for a week.** Watch R2 request volume drop from O(70k) to O(changed) and confirm `composer install` resolution stays correct against the CDN. Keep the old path warm the whole time.

**Step 6 — Delete (PR 4).** Only now remove `internal/repository/`, `deploy/local.go`, `cmd/build.go`, the `SyncToR2` half of `r2.go`, the build directory handling, `serializer_parity_test.go` (its counterparty is gone), `sync_test.go`, and the `--sync-mode` flag itself. Migrate `smoke_test.go` to the DB-backed serve layer.

The `pipeline_runs` migration is deliberately **not** in this sequence — see 3f.

### 3f. Combined `pipeline_runs` table — sequence separately

The `builds` and `sync_runs` tables are replaced by a single `pipeline_runs` table (see schema in "Pipeline runs table" section above):

- Create `pipeline_runs` with the new schema
- Migrate historical data from `builds` (map `packages_changed` → `packages_updated`, drop artifact columns)
- Drop `sync_runs` table
- Drop `metadata_changes` table (change detection moves to `content_changed_at` — see Phase 6)
- Update `packages.last_sync_run_id` FK to reference `pipeline_runs`

The admin UI builds page becomes "Pipeline Runs" — same layout, updated column headers.

**Do this after Step 6, as its own PR.** It is a destructive, non-reversible schema change (drops two tables) with no rollback story, while the code cutover in 3e is a flag flip. Coupling them means a schema problem forces a code revert and vice versa. The observability change also lands better once the new pipeline shape is settled and the column set is known to be right — this table records what the pipeline does, so it should be defined after the pipeline stops moving.

Note that dropping `metadata_changes` also switches the changes feed backend (Phase 6), so that endpoint's tests need to pass against the `content_changed_at` query in the same PR.

### 3g. Remove old code (Step 6)

- Delete `internal/repository/` (replaced by `internal/composer/`)
- Delete `internal/deploy/local.go`
- Gut `internal/deploy/r2.go` — keep `putObjectWithRetry`, `deleteObjectWithRetry`, `headObject`, `withRetry`, `CacheControlForPath`, `newS3Client`; remove `SyncToR2` and `fileUnchanged`
- Delete `cmd/wppackages/cmd/build.go`
- Simplify `cmd/wppackages/cmd/deploy.go` (default action is sync, flag for cleanup; drop rollback, promote, `previousBuildDirFor`)
- Simplify `cmd/wppackages/cmd/pipeline.go`, drop `--sync-mode`
- Remove build dir cleanup, `storage/repository/builds/`, `current` symlink references
- Remove `persistMetadataChanges` from `build.go`

### Files touched

- `internal/composer/serialize.go` (hash covers serialized output — Step 1)
- `internal/deploy/sync.go` (mass-delete guard, `--dry-run` support)
- `internal/deploy/r2.go` (gutted — Step 6)
- `internal/deploy/local.go` (delete — Step 6)
- `internal/repository/` (delete entire package — Step 6)
- `internal/packages/sync.go` (reworked for pipeline_runs — 3f)
- `cmd/wppackages/cmd/rehash.go` (new — Step 1)
- `cmd/wppackages/cmd/sync.go` (new — Step 3)
- `cmd/wppackages/cmd/pipeline.go` (`--sync-mode` in Step 4, simplified in Step 6)
- `cmd/wppackages/cmd/build.go` (delete — Step 6)
- `cmd/wppackages/cmd/deploy.go` (simplify — Step 6)
- New migration: create `pipeline_runs`, migrate `builds` data, drop `sync_runs`, drop `metadata_changes` (3f)
- `internal/http/handlers.go` + `admin_builds.html` (update for pipeline_runs schema — 3f)

---

## Phase 4: Test Infrastructure

**Goal:** Mock wp.org API for CI stability, in-process S3 fake for sync tests, composer end-to-end test.

**Status: PARTIALLY COMPLETE**

### 4a. wp.org API fixtures + mock server — DONE

`internal/wporg/mock_server.go` serves fixtures by slug, returns 404 for unknown slugs. Routes handle both full info requests and `last_updated`-only requests (versions=false).

Fixtures in `internal/wporg/testdata/`:
- `plugins/akismet.json`, `plugins/classic-editor.json`, `plugins/contact-form-7.json`
- `themes/astra.json`, `themes/twentytwentyfive.json`

### 4b. Test helper: seed DB from fixtures — DONE

`internal/testutil/testdb.go` provides:
- `OpenTestDB(t)` — in-memory SQLite + all migrations
- `SeedFromFixtures(t, db, mockURL)` — runs discover + update pipeline against mock server, populating 5 seed packages

Note: `SeedFromFixtures` does not yet compute `content_hash` or `trunk_revision` since the hash columns don't exist yet. This will need updating when Phase 1 lands.

### 4c. Integration test: build + serve + composer — EXISTS (needs updating)

`internal/integration/smoke_test.go` still tests the filesystem-based architecture: builds to disk, serves static files, validates Composer metadata format and `composer install`. Phase 2 shipped the DB-backed serve layer but this test was not migrated to it. Update at Step 6, when the build path is deleted and this test stops compiling anyway.

### 4d. Integration test: R2 sync with gofakes3 — BOTH EXIST

`internal/integration/sync_test.go` covers the old filesystem sync; `internal/integration/db_sync_test.go` (added in #96) covers `deploy.Sync()` against gofakes3 — full upload, idempotent re-sync, deletion on deactivation. Keeping both is correct during the staged cutover: they are the regression net for whichever path `--sync-mode` selects. Delete `sync_test.go` at Step 6.

Gap worth closing at Step 1: neither test covers a metadata-only change (description/homepage/author/`last_committed` moving without a version change). That is exactly the Risk 1 failure mode, and it currently passes silently.

### 4e. Integration test: full round-trip — NOT STARTED

Sync to fake S3, point composer at it, verify resolution works for both tagged and dev versions:

```go
// internal/integration/roundtrip_test.go
func TestFullRoundTrip(t *testing.T) {
    db := testutil.OpenTestDB(t)
    mockSrv := wporg.NewMockServer(fixtureDir)
    testutil.SeedFromFixtures(t, db, mockSrv.URL)
    s3srv := startFakeS3(t)
    deploy.Sync(ctx, db, r2Config, logger)

    // Verify tagged version resolution
    // composer require wp-plugin/akismet:5.3.7 --dry-run against s3srv endpoint
    // assert exit 0, assert dist URL present

    // Verify dev version resolution
    // composer require wp-plugin/akismet:dev-trunk --dry-run against s3srv endpoint
    // assert exit 0, assert source-only (no dist)

    // Verify both {name}.json and {name}~dev.json exist on fake S3
    // Verify packages with no dev versions have no ~dev.json
}
```

### Files remaining

- `internal/integration/roundtrip_test.go` (new)
- Update `internal/integration/smoke_test.go` for DB-backed serve (Phase 2)
- Update `internal/integration/sync_test.go` for DB-driven sync (Phase 3)
- Update `internal/testutil/testdb.go` to compute `content_hash` (Phase 1)

---

## Phase 5: Cleanup

**Goal:** Clean up orphaned R2 objects.

**Status: PARTIALLY DONE** — 5a and 5b landed early, out of sequence, in [#108](https://github.com/roots/wp-packages/pull/108) and [#109](https://github.com/roots/wp-packages/pull/109) (June 2026).

Both were built against the old deploy path but operate on `deployed_hash`, so they carry over to the DB-driven model unchanged. `internal/deploy/cleanup.go` does batched `DeleteObjects` with per-package `deployed_hash` clearing; `cmd/cleanup_r2_orphans.go` exposes it as a bulk command.

### 5a. Deactivated package cleanup — DONE

Part of both sync paths: after stamping `deployed_hash`, `DeleteObject` for p2/ files of deactivated packages (both `{name}.json` and `{name}~dev.json`). Serial loop, which is fine for the expected trickle — but see Risk 3, it needs a cap for the mass-closure case.

### 5b. Orphaned p2/ file GC — DONE (as bulk delete)

`cleanup-r2-orphans` deletes R2 files for all packages where `is_active = 0 AND deployed_hash IS NOT NULL`. Note this is the *deactivated-row* cleanup, not the full set-difference GC described below — it cannot find R2 objects with no corresponding DB row at all (e.g. left behind by a crashed run, or renamed packages). The set-difference sweep is still unimplemented:

Infrequent operation (weekly/monthly):

1. Query all active package type+name pairs from DB → expected p2/ keys (both `{name}.json` and `{name}~dev.json` variants)
2. List all `p2/` objects on R2
3. Delete any not in the expected set

Much simpler than the old plan — no content-addressed hashes to trace through, no provider group references to follow. Just a set difference between R2 keys and DB rows. The expected set includes `~dev.json` files only for packages that actually have dev versions.

### 5c. Legacy p/ file cleanup

One-time migration: delete all `p/` prefixed objects from R2 since they're no longer served.

### Files touched

- `internal/deploy/cleanup.go` (new or refactored from existing cleanup code)
- `internal/deploy/sync.go` (add deactivated package p2/ deletion)
- `cmd/wppackages/cmd/deploy.go` (cleanup flag)

---

## Phase 6: Metadata Changes Feed — Simplify

**Goal:** Replace the transitional event log implementation with a simpler `content_changed_at` query.

**Status: ENDPOINT EXISTS, BACKEND NEEDS MIGRATION**

The endpoint and Packagist-compatible response format are already implemented:
- `internal/http/changes.go` — `GET /metadata/changes.json` handler with dedup, 24h retention, resync action
- `internal/http/changes_test.go` — full test coverage
- `metadata-changes-url` already included in generated `packages.json`
- Route registered in `router.go`

### Current implementation (transitional)

The changes feed is backed by a `metadata_changes` event log table (migration 020), populated by `build.go` after filesystem byte comparison. This works but is tightly coupled to the build step — when Phase 3 eliminates builds, the event log loses its writer.

### Target implementation

After Phase 1 adds `content_changed_at` and Phase 3 removes the build step, replace the event log query with a direct column query:

```sql
SELECT type, name, content_changed_at, is_active
FROM packages
WHERE content_changed_at > ?
ORDER BY content_changed_at
LIMIT 5000
```

Active packages → `"update"` action. Deactivated packages (where `is_active = 0` and `content_changed_at` reflects deactivation time) → `"delete"` action.

**Advantages over the event log:**
- Zero maintenance — no table growth, no cleanup job
- Single source of truth — the column IS the state
- No separate table, no FK to builds/pipeline_runs
- Works naturally with the DB-driven architecture

**When a package is deactivated**, set `content_changed_at = now()` so it appears in the feed as a delete. The 24h retention window and resync action from the current implementation carry over unchanged.

The `metadata_changes` table is dropped in Phase 3f's migration. The handler in `changes.go` is updated to query `packages` directly instead. Response format, caching headers, and test coverage remain the same.

### Files touched

- `internal/http/changes.go` (update query from `metadata_changes` table to `packages.content_changed_at`)
- Migration in Phase 3f drops `metadata_changes` table

For R2/CDN: this endpoint is served by the app server (same as `notify-batch`), not static R2 files.

---

## Execution order

Phases 1 and 2 are done. Phase 5a/5b landed early. What remains is the Phase 3 cutover, sequenced as four PRs plus a soak:

```
PR 1  Fix content_hash to cover serialized output      ← blocking correctness fix
      + rehash command (full reconcile, no network)
      + mass-delete guard
      + byte-parity test vs old builder
      + metadata-only-change test
  ↓   no behaviour change — old pipeline still in charge
PR 2  sync --dry-run (shadow mode)
  ↓   run in production, compare against old path's actual uploads
PR 3  pipeline --sync-mode=build|db, default build
  ↓   flip to db; rollback is a flag change, not a revert
SOAK  one week on --sync-mode=db, old path kept warm
  ↓
PR 4  Delete build/deploy/repository code, drop the flag  ← Phase 3g
  ↓   also: migrate smoke_test.go, delete sync_test.go (Phase 4c/4d)
PR 5  pipeline_runs migration                            ← Phase 3f + Phase 6
      destructive + irreversible, deliberately last
```

The ordering principle: every step before PR 4 is reversible, and nothing is deleted until the replacement has run correctly in production for a week. The two irreversible things — the schema migration and the code deletion — happen after the risk is gone, not before.

Phase 4e (round-trip test) can land any time after PR 1. Phase 5c (legacy `p/` cleanup) is independent of all of this.
