<!-- DUAL-SOURCE: keep in sync with templates/index.html — served as the Markdown rep of `/`. -->

# WP Packages — WordPress Packages for Composer

Composer repository for WordPress.org plugins and themes. A 17x faster, fully open-source alternative to WPackagist that updates every 5 minutes.

## Add the repository

```sh
composer config repositories.wp-packages composer https://repo.wp-packages.org
```

Every active plugin and theme from the WordPress.org directory is available as a Composer package.

## Install a plugin or theme

```sh
composer require wp-plugin/woocommerce
composer require wp-theme/twentytwentyfive
```

Plugins live under `wp-plugin/*` and themes under `wp-theme/*`. The slug after the prefix matches the plugin or theme slug on WordPress.org.

## Find a package

- Browse and search packages at <https://wp-packages.org/>
- Each package has a Markdown representation at `https://wp-packages.org/packages/wp-{plugin,theme}/{slug}.md` — install command, current version, available versions, author, and description
- Programmatic install stats are available via the [JSON API](https://wp-packages.org/docs#api)

## Learn more

- [Documentation](https://wp-packages.org/docs) — full setup, Bedrock examples, API reference, and shields.io badges
- [WP Packages vs WPackagist](https://wp-packages.org/wp-packages-vs-wpackagist) — why WP Packages exists and how it compares
- [WordPress core via Composer](https://wp-packages.org/wordpress-core) — install WordPress itself with `roots/wordpress`
- [Status](https://wp-packages.org/status) — build history and status check activity
- [Untagged Plugins](https://wp-packages.org/untagged) — plugins whose latest version isn't tagged in SVN

## Independent and community-funded

WP Packages is built and maintained by [Roots](https://roots.io), the team behind [Bedrock](https://roots.io/bedrock/), [Sage](https://roots.io/sage/), [Trellis](https://roots.io/trellis/), and [Acorn](https://roots.io/acorn/). The entire stack — application, infrastructure, deployment — lives in the [public repository](https://github.com/roots/wp-packages). Funded entirely by the community via [GitHub Sponsors](https://github.com/sponsors/roots).
