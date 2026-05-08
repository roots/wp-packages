<!-- DUAL-SOURCE: keep in sync with templates/compare.html — served as the Markdown rep of `/wp-packages-vs-wpackagist`. -->

# WP Packages vs WPackagist

WP Packages is 17x faster, updates every 5 minutes, and is fully open-source.

For years, WPackagist was the go-to Composer repository for WordPress plugins and themes. In March 2026, WPackagist was acquired by WP Engine, a private-equity backed company. Tooling this central to the WordPress Composer workflow shouldn't be owned by a single corporation — it should be independent and community-funded.

## How they compare

| | WP Packages | WPackagist |
| --- | --- | --- |
| Ownership | Independent, maintained by [Roots](https://roots.io) | WP Engine (private equity) |
| Open source | Fully open source — application, infrastructure, deployment, and operational tooling all live in the public repo. Every commit is what runs in production. [Contributions welcome](https://github.com/roots/wp-packages) | Application source and Docker build are public, but production infrastructure (cloud provisioning, environment configuration, CI/CD) is not — the running service can't be reproduced from the public repo |
| Funding | Community-funded via [GitHub Sponsors](https://github.com/sponsors/roots) | Corporate-funded |
| Package naming | `wp-plugin/*` and `wp-theme/*` | `wpackagist-plugin/*` and `wpackagist-theme/*` |
| Package metadata | Includes plugin/theme authors, description, homepage, and support links in Composer metadata | Missing — [requested since 2020](https://github.com/outlandishideas/wpackagist/issues/305) |
| Update frequency | Every 5 minutes | ~1.5 hours (estimated — infrastructure is not open source) |
| Governance | Open roadmap, community collaboration | Corporate decision-making |
| Untagged plugin installs | Pinned to SVN revision — `composer.lock` is reproducible | Mutable trunk zip with cache-busting timestamp — `composer.lock` is not reproducible. **This can cause unexpected plugin updates** |
| Closed plugins & themes | Removed in lockstep with WordPress.org — matches wp.org's own behavior, where closed plugins are no longer downloadable. Closures are tracked on the [status page](https://wp-packages.org/status) and exposed via the [closures API](https://wp-packages.org/docs#api) | Continues serving closed plugins indefinitely with no notice. **Users can unknowingly install plugins closed years ago for security or abandonment** |
| Install statistics | Per-package install stats — transparency for the community and package authors | No install statistics |
| Public status page | Real-time [status page](https://wp-packages.org/status) with build history, package changes, and closure tracking | No status page |

## Closed plugins

When the WordPress.org plugin directory closes a plugin — typically for security vulnerabilities, author abandonment, or guideline violations — the plugin is no longer downloadable from wp.org. WP Packages mirrors this behavior: closed plugins are removed from the repository, so Composer cannot install or upgrade to a closed version.

WPackagist continues serving closed plugins with no notice to users. We've heard from developers who migrated to WP Packages and discovered they had been depending on plugins that wp.org closed _years_ earlier — including some closed for security reasons. If your `composer.lock` references a plugin wp.org has since closed, WP Packages surfaces it; WPackagist silently keeps installing it.

All currently closed plugins are listed on the [status page](https://wp-packages.org/status) and available via the [closures API](https://wp-packages.org/docs#api).

## Performance

WP Packages supports Composer v2's `metadata-url` protocol, which lets Composer fetch metadata for only the packages it needs. WPackagist still uses the older `provider-includes` approach, which forces Composer to download large index files containing metadata for thousands of packages before it can resolve your dependencies.

### Composer resolve times

Cold resolve (no cache) — lower is better.

| Plugins | WP Packages | WPackagist | Speedup |
| --- | --- | --- | --- |
| 10 plugins | 0.7s | 12.3s | 17x faster |
| 20 plugins | 1.1s | 19.0s | 17x faster |

### Metadata & caching

| | WP Packages | WPackagist |
| --- | --- | --- |
| Composer v2 metadata-url | Yes | No |
| Metadata changes feed (`metadata-changes-url`) | Yes | No |
| CDN caching | `public, max-age=300` | `no-cache, private` |
| Per-package files | Immutable, content-addressed, cached indefinitely | Not content-addressed |

Benchmarks run from a single location using Composer 2.7+. Results may vary by region and network conditions. [Benchmark scripts are open source](https://github.com/roots/wp-packages/tree/main/benchmarks).

## The case for independence

WPackagist was originally built and maintained by [Outlandish](https://outlandish.com/), who operated the service for over a decade. In its later years, the project suffered from neglect — slow updates, limited maintenance, and no meaningful community input into its direction.

Its acquisition by WP Engine raises important questions. When infrastructure this foundational to the WordPress developer workflow is controlled by a single corporation, the community loses its voice. Decisions about availability, pricing, and direction are made in boardrooms, not in the open.

WPackagist publishes its application code, a Dockerfile, and a handful of deployment helpers — but not the production infrastructure that runs the service. Cloud provisioning, environment configuration, CI/CD — all proprietary. The public repo is enough to build the container, not enough to reproduce the running service.

The public repo has also diverged from production at times: gaps the community has had to catch and report (e.g. [#556](https://github.com/wpengine/wpackagist/issues/556), [#557](https://github.com/wpengine/wpackagist/issues/557)) before they were addressed. With infrastructure closed, there's no built-in way for outside observers to know when the public source no longer reflects what's actually running. WP Packages takes the opposite approach: every component that runs the service — the Go application, the Ansible playbooks, the Caddy configuration, the database tooling — lives in the public repo. The codebase is what runs in production, and anyone can clone it and spin up their own instance.

## Built by Roots, maintained since 2011

WP Packages is built by [Roots](https://roots.io), the team behind [Bedrock](https://roots.io/bedrock/), [Sage](https://roots.io/sage/), [Trellis](https://roots.io/trellis/), and [Acorn](https://roots.io/acorn/). Since 2011, Roots has been continuously improving and maintaining open source WordPress tooling. We pioneered the use of Composer in WordPress development and have a proven track record of long-term commitment to the ecosystem.

WP Packages is [fully open source](https://github.com/roots/wp-packages) and designed for community collaboration. Every line of code is public, contributions are welcome, and the project's direction is shaped by the developers who use it.

## Community-funded, community-driven

Unlike corporate-backed alternatives, WP Packages is funded entirely by the community through [GitHub Sponsors](https://github.com/sponsors/roots). Your sponsorship directly supports the infrastructure, development, and maintenance of WP Packages and the broader Roots ecosystem.

## Migrating from WPackagist

Switching from WPackagist takes one command. Use the [migration script](https://github.com/roots/wp-packages/blob/main/scripts/migrate-from-wpackagist.sh) to automatically update your `composer.json`:

```sh
curl -sO https://raw.githubusercontent.com/roots/wp-packages/main/scripts/migrate-from-wpackagist.sh && bash migrate-from-wpackagist.sh
```

### Manually migrate

1. Remove WPackagist packages:

   ```sh
   composer remove wpackagist-plugin/woocommerce
   ```

2. Remove the WPackagist repository and add WP Packages:

   ```sh
   composer config --unset repositories.wpackagist && composer config repositories.wp-packages composer https://repo.wp-packages.org
   ```

3. Require packages with the new naming:

   ```sh
   composer require wp-plugin/woocommerce
   ```
