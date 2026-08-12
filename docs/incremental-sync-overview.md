# Incremental Sync Architecture

## Motivation

WP Packages currently runs a full pipeline on every sync cycle: discover packages → fetch updates → build ~140k files to disk → deploy via symlink swap → upload to R2. This worked when Composer v1 required a complete provider tree, but since dropping v1 support, the build directory is vestigial overhead. Every run rewrites all files regardless of whether anything changed, and the R2 sync walks the entire build directory doing byte comparisons — O(total packages) instead of O(changed packages).

## Goal

Replace the build-directory pipeline with a DB-driven architecture where SQLite is the single source of truth. Packages get a `content_hash` (what the data looks like) and a `deployed_hash` (what's live on R2). Finding what needs uploading becomes a single query: `WHERE content_hash != deployed_hash`. No intermediate files, no filesystem walking, no manifest.

## How It Works

**Three-step pipeline: Discover → Update → Sync**

- **Discover** checks what packages exist and which ones changed (via SVN revision log). Cheap — no API calls.
- **Update** fetches full metadata from wp.org only for changed packages, normalizes versions, and computes `content_hash`. If the hash changed, the package is marked dirty.
- **Sync** queries for dirty packages, serializes their Composer JSON, uploads to R2 in parallel, then stamps `deployed_hash`. Crash-safe — if interrupted, the next run picks up where it left off.

**DB-backed serving** for local dev: the HTTP server serializes Composer metadata directly from SQLite on each request, eliminating the build step entirely for development.

**Conditional `packages.json` upload**: the root Composer config is effectively static, so it's uploaded with `If-None-Match` — a no-op on most runs.

## Phases

1. **Schema + Content Hash** — ✅ Done ([#74](https://github.com/roots/wp-packages/pull/74)). `content_hash`, `deployed_hash`, `content_changed_at` columns; pure `composer` package; hashes computed at update time.

2. **DB-Backed Serve Layer** — ✅ Done ([#90](https://github.com/roots/wp-packages/pull/90)). `/p2/{type}/{name}.json` and `/packages.json` served from SQLite.

3. **R2 Sync** — 🚧 In progress. `deploy.Sync()` is written and tested ([#96](https://github.com/roots/wp-packages/pull/96)) but **not yet wired into the pipeline** — production still runs the filesystem build + deploy. The remaining work is the cutover itself: fix `content_hash` to cover the full serialized output, shadow-run the new path, flip behind a flag, then delete ~1,200 lines of build/deploy/filesystem code and combine `builds` + `sync_runs` into `pipeline_runs`.

4. **Test Infrastructure** — Partially done. `db_sync_test.go` covers the new sync path; `smoke_test.go` still tests the filesystem architecture. Round-trip test (seed DB → sync to fake S3 → resolve with Composer) not started.

5. **Cleanup** — Partially done ([#108](https://github.com/roots/wp-packages/pull/108), [#109](https://github.com/roots/wp-packages/pull/109)). Deactivated-package R2 deletion ships; set-difference GC and legacy Composer v1 (`p/`) removal still open.

Several items from the original plan are already complete: dev branch split (`~dev.json`), `dev-trunk` dist removal + SVN revision pinning, OG generation refactor, `dev.go` deletion, `provider_group` drop, and the metadata changes feed endpoint (currently backed by an event log table, migrating to `content_changed_at` in Phase 3).

The cutover is staged so nothing is deleted until the new path has run correctly in production for a week, and so the two irreversible steps — the schema migration and the code deletion — come last. See `incremental-sync-plan.md` for the risks and PR sequence.
