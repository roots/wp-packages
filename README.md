# WP Packages

![Total Installs](https://img.shields.io/badge/dynamic/json.svg?url=https%3A%2F%2Fwp-packages.org%2Fapi%2Fstats&query=%24.total_installs_formatted&label=composer%20installs&colorB=2b3072&colorA=525ddc&style=flat-square)
[![Status](https://img.shields.io/badge/status-page-blue?style=flat-square)](https://wp-packages.org/status)
[![Build status](https://img.shields.io/github/actions/workflow/status/roots/wp-packages/ci.yml?branch=main&style=flat-square)](https://github.com/roots/wp-packages/actions)
[![Follow Roots](https://img.shields.io/badge/follow%20@rootswp-1da1f2?logo=twitter&logoColor=ffffff&message=&style=flat-square)](https://twitter.com/rootswp)
[![Sponsor Roots](https://img.shields.io/badge/sponsor%20roots-525ddc?logo=github&style=flat-square&logoColor=ffffff&message=)](https://github.com/sponsors/roots)

Manage your WordPress plugins and themes with Composer.

## Support us

Roots is an independent open source org, supported only by developers like you. Your sponsorship funds [WP Packages](https://wp-packages.org/) and the entire Roots ecosystem, and keeps them independent. Support us by purchasing [Radicle](https://roots.io/radicle/) or [sponsoring us on GitHub](https://github.com/sponsors/roots) — sponsors get access to our private Discord.

### Sponsors

<a href="https://carrot.com/"><img src="https://cdn.roots.io/app/uploads/carrot.svg" alt="Carrot" height="90"></a> <a href="https://wordpress.com/"><img src="https://cdn.roots.io/app/uploads/wordpress.svg" alt="WordPress.com" height="90"></a> <a href="https://www.itineris.co.uk/"><img src="https://cdn.roots.io/app/uploads/itineris.svg" alt="Itineris" height="90"></a> <a href="https://kinsta.com/?kaid=OFDHAJIXUDIV"><img src="https://cdn.roots.io/app/uploads/kinsta.svg" alt="Kinsta" height="90"></a> <a href="https://ploi.io/"><img src="https://cdn.roots.io/app/uploads/ploi.svg" alt="Ploi" height="90"></a>

## [WP Packages vs WPackagist](https://wp-packages.org/wp-packages-vs-wpackagist)

|  | WP Packages | WPackagist |
|---|---|---|
| Open source | Fully open source — application, infrastructure, and deployment all in the public repo | Application + Docker build only; production infrastructure remains proprietary, and public repo has historically diverged from production |
| Package naming | `wp-plugin/*` `wp-theme/*` | `wpackagist-plugin/*` `wpackagist-theme/*` |
| Package metadata | Includes authors, description, homepage, and support links | Missing — [requested since 2020](https://github.com/outlandishideas/wpackagist/issues/305) |
| Update frequency | Every 5 minutes | ~1.5 hours (estimated) |
| Composer v2 `metadata-url` | ✅ | ❌ |
| Composer v2 `metadata-changes-url` | ✅ | ❌ |
| Install statistics | ✅ | ❌ |
| Public status page | ✅ [See status page](https://wp-packages.org/status) | ❌ |
| Untagged plugin installs | Immutable — pinned to SVN revision | Mutable, resulting in unexpected plugin updates |
| Closed plugins & themes | Removed in sync with WordPress.org closures | Continues serving closed plugins with no notice |

### Composer resolve times

Cold resolve (no cache) — lower is better:

| Plugins | WP Packages | WPackagist | Speedup |
|---|---|---|---|
| 10 plugins | 0.7s | 12.3s | 17x faster |
| 20 plugins | 1.1s | 19.0s | 17x faster |

## [Documentation](https://wp-packages.org/docs)

See the [documentation](https://wp-packages.org/docs) for usage instructions, example `composer.json` configurations, and more.

### Roots WordPress Packages

Roots provides WordPress core as Composer packages — [`roots/wordpress`](https://github.com/roots/wordpress), [`roots/wordpress-full`](https://github.com/roots/wordpress-full), and [`roots/wordpress-no-content`](https://github.com/roots/wordpress-no-content). [Learn more](https://wp-packages.org/wordpress-core).

## [Migrating from WPackagist](https://wp-packages.org/docs#migrate)

Use the [migration script](scripts/migrate-from-wpackagist.sh) to automatically update your `composer.json`:

```sh
curl -sO https://raw.githubusercontent.com/roots/wp-packages/main/scripts/migrate-from-wpackagist.sh && bash migrate-from-wpackagist.sh
```
## Community

- Join us on Discord by [sponsoring us on GitHub](https://github.com/sponsors/roots)
- Join us on [Roots Discourse](https://discourse.roots.io/)
- Follow [@rootswp on Twitter](https://twitter.com/rootswp)
- Follow the [Roots Blog](https://roots.io/blog/)
- Subscribe to the [Roots Newsletter](https://roots.io/subscribe/)
