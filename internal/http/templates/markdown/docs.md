<!-- DUAL-SOURCE: keep in sync with templates/docs.html — served as the Markdown rep of `/docs`. -->

# Documentation

Add the WP Packages repository to your project:

```sh
composer config repositories.wp-packages composer https://repo.wp-packages.org
```

Every active plugin and theme from the WordPress.org directory is available as a Composer package. [Search all packages](https://wp-packages.org/).

> Some WordPress plugins have a latest version on WordPress.org that isn't tagged in SVN. These can be installed via `dev-trunk`. [Learn more on the Untagged Plugins page](https://wp-packages.org/untagged).

## Package naming

| Type | Convention | Example |
| --- | --- | --- |
| Plugin | `wp-plugin/plugin-name` | [`wp-plugin/woocommerce`](https://wp-packages.org/packages/wp-plugin/woocommerce) |
| Theme | `wp-theme/theme-name` | [`wp-theme/twentytwentyfive`](https://wp-packages.org/packages/wp-theme/twentytwentyfive) |

## Example `composer.json`

### Traditional WordPress

```json
{
  "repositories": [
    {
      "name": "wp-packages",
      "type": "composer",
      "url": "https://repo.wp-packages.org"
    }
  ],
  "require": {
    "composer/installers": "^2.2",
    "wp-plugin/woocommerce": "^10.0",
    "wp-theme/twentytwentyfive": "^1.0"
  },
  "extra": {
    "installer-paths": {
      "wp-content/plugins/{$name}/": ["type:wordpress-plugin"],
      "wp-content/mu-plugins/{$name}/": ["type:wordpress-muplugin"],
      "wp-content/themes/{$name}/": ["type:wordpress-theme"]
    }
  }
}
```

### Bedrock

[Bedrock](https://roots.io/bedrock/) comes with both WordPress core as a Composer package and WP Packages support out of the box. See [Bedrock's composer.json](https://github.com/roots/bedrock/blob/master/composer.json).

## WordPress core Composer packages

Roots also provides WordPress core as Composer packages:

| Package | Description |
| --- | --- |
| [`roots/wordpress`](https://wp-packages.org/wordpress-core) | Meta-package for installing WordPress core via Composer |
| [`roots/wordpress-full`](https://wp-packages.org/wordpress-core) | Full WordPress build (core + default themes + plugins + betas) |
| [`roots/wordpress-no-content`](https://wp-packages.org/wordpress-core) | Minimal WordPress build (core only) |

[Learn more about the WordPress core Composer packages](https://wp-packages.org/wordpress-core).

## Migrating from WPackagist

[See how WP Packages compares to WPackagist](https://wp-packages.org/wp-packages-vs-wpackagist).

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

## Changelog action

[`roots/wp-packages-changelog-action`](https://github.com/roots/wp-packages-changelog-action) is a GitHub Action that automatically comments on pull requests with a changelog summary for any WP Packages dependencies that changed.

When a PR updates your `composer.lock`, the action compares the before and after versions and posts a comment with the relevant changelog entries from WordPress.org. It also warns when an installed version doesn't match the current [WordPress.org stable tag](https://github.com/roots/wp-packages/issues/78), helping you catch outdated or mismatched dependencies before merging.

### Usage

```yaml
# .github/workflows/changelog.yml
name: Changelog
on:
  pull_request:
    paths:
      - composer.lock

jobs:
  changelog:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v6
      - uses: roots/wp-packages-changelog-action@v3
        env:
          GITHUB_TOKEN: ${{ secrets.GITHUB_TOKEN }}
```

## API

WP Packages provides a public JSON API for install statistics.

### `GET /api/stats`

Returns overall install statistics.

```json
{
  "total_installs": 123456,
  "total_installs_formatted": "123k",
  "installs_30d": 7890,
  "installs_30d_formatted": "8k",
  "active_plugins": 500,
  "active_themes": 200,
  "total_packages": 700
}
```

### `GET /api/stats/packages/{type}/{name}`

Returns monthly install history for a specific package (up to 36 months). The `type` can be `wp-plugin` or `wp-theme`.

```sh
curl https://wp-packages.org/api/stats/packages/wp-plugin/woocommerce
```

```json
[
  { "month": "2026-01", "installs": 142 },
  { "month": "2026-02", "installs": 88 }
]
```

### `GET /api/stats/packages/{type}/{name}/total`

Returns the total Composer install count and the last 30 days of installs for a single package. Designed for use in shields.io badges.

```sh
curl https://wp-packages.org/api/stats/packages/wp-plugin/woocommerce/total
```

```json
{
  "total_installs": 1234567,
  "total_installs_formatted": "1.2M",
  "installs_30d": 45678,
  "installs_30d_formatted": "46k"
}
```

### `GET /api/packages/{type}/closed`

Returns a sorted list of slugs for plugins or themes that are currently closed on wp.org — including both temporary and permanent closures. The `type` can be `wp-plugin` or `wp-theme`.

```sh
curl https://wp-packages.org/api/packages/wp-plugin/closed
```

```json
["closed-plugin-slug-a", "closed-plugin-slug-b"]
```

### `GET /api/packages/{type}/closed/permanent`

Returns the subset of closed packages that have been flagged as permanently closed on wp.org. Use this endpoint when you only want to surface closures unlikely to ever reopen.

```sh
curl https://wp-packages.org/api/packages/wp-plugin/closed/permanent
```

### `GET /api/closures`

Returns a paginated list of mass-closure events detected in rolling 24-hour windows.

```sh
curl https://wp-packages.org/api/closures
```

```json
{
  "events": [
    {
      "id": 42,
      "vendor_name": "WPFactory",
      "vendor_slug": "wpfactory",
      "detected_at": "2026-04-27T13:03:45Z",
      "detected_at_formatted": "April 27, 2026",
      "plugin_slugs": ["plugin-a", "plugin-b"],
      "plugin_count": 2
    }
  ],
  "page": 1,
  "per_page": 50,
  "total": 6,
  "total_pages": 1,
  "documentation_url": "https://wp-packages.org/docs#api-closures"
}
```

### `GET /api/closures/{vendor_slug}`

Returns all historical mass-closure events for a specific vendor.

```sh
curl https://wp-packages.org/api/closures/wpfactory
```

```json
{
  "events": [
    {
      "id": 42,
      "vendor_name": "WPFactory",
      "vendor_slug": "wpfactory",
      "detected_at": "2026-04-27T13:03:45Z",
      "detected_at_formatted": "April 27, 2026",
      "plugin_slugs": ["plugin-a", "plugin-b"],
      "plugin_count": 2
    }
  ],
  "documentation_url": "https://wp-packages.org/docs#api-vendor-closures"
}
```

Stats responses are cached for 5 minutes; the closed-packages and closures endpoints are cached for 1 hour. Returns `404` for inactive or unknown packages, and `429` when rate limited.

## Badges for plugin authors

If you maintain a plugin or theme on WordPress.org, show your Composer install count in your README using [shields.io](https://shields.io). The badge updates automatically as installs grow.

Add this to your README, replacing `your-plugin` with your plugin slug:

```md
[![wp-packages.org installs](https://img.shields.io/badge/dynamic/json.svg?url=https%3A%2F%2Fwp-packages.org%2Fapi%2Fstats%2Fpackages%2Fwp-plugin%2Fyour-plugin%2Ftotal&query=%24.total_installs_formatted&label=wp-packages.org%20installs&logo=roots&logoColor=white&colorB=2b3072&colorA=525ddc&style=flat-square)](https://wp-packages.org/packages/wp-plugin/your-plugin)
```

For themes, change `wp-plugin` to `wp-theme` in the URL. You can customize the label, color, and style with shields.io's URL parameters — see the [shields.io docs](https://shields.io/badges/dynamic-json-badge).
