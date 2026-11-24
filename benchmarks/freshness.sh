#!/usr/bin/env bash
#
# Benchmark: Version freshness — compare available versions between repos
#
# For a set of popular plugins, fetches the version list from both
# WP Packages and WPackagist and compares:
#   - Latest version available
#   - Total version count
#   - Missing versions in either repo
#
# Usage: ./benchmarks/freshness.sh [--plugins N]
#
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
RESULTS_DIR="${SCRIPT_DIR}/results"
mkdir -p "$RESULTS_DIR"

WPC_BASE="https://repo.wp-packages.org"
WPKG_BASE="https://wpackagist.org"
PLUGIN_COUNT=20

while [[ $# -gt 0 ]]; do
  case $1 in
    --plugins) PLUGIN_COUNT="$2"; shift 2 ;;
    *) echo "Unknown option: $1"; exit 1 ;;
  esac
done

PLUGINS=(
  akismet
  classic-editor
  wordpress-seo
  woocommerce
  contact-form-7
  elementor
  wordfence
  updraftplus
  wp-super-cache
  redirection
  advanced-custom-fields
  all-in-one-seo-pack
  jetpack
  duplicate-post
  regenerate-thumbnails
  litespeed-cache
  really-simple-ssl
  query-monitor
  wp-mail-smtp
  google-site-kit
  tablepress
  members
  wp-optimize
  autoptimize
  sucuri-scanner
  ithemes-security-pro
  ninja-forms
  wpforms-lite
  limit-login-attempts-reloaded
  one-click-demo-import
)

# Trim to requested count
PLUGINS=("${PLUGINS[@]:0:$PLUGIN_COUNT}")

# ── Cache packages.json responses ─────────────────────────────────

CACHE_DIR=$(mktemp -d)
trap 'rm -rf "$CACHE_DIR"' EXIT

get_packages_json() {
  local base="$1"
  local cache_key
  cache_key=$(echo "$base" | md5 -q 2>/dev/null || echo "$base" | md5sum | cut -d' ' -f1)
  local cache_file="${CACHE_DIR}/${cache_key}.json"

  if [[ ! -f "$cache_file" ]]; then
    curl -sf -H 'User-Agent: Composer/2.7' "${base}/packages.json" > "$cache_file" 2>/dev/null || return 1
  fi
  cat "$cache_file"
}

# ── Helpers ───────────────────────────────────────────────────────

# Fetch version list via p2 metadata-url (WP Packages)
get_versions_p2() {
  local base="$1"
  local package="$2"

  local pj
  pj=$(get_packages_json "$base") || return 1

  local metadata_url
  metadata_url=$(echo "$pj" | jq -r '.["metadata-url"] // empty' 2>/dev/null)

  if [[ -z "$metadata_url" ]]; then
    return 1
  fi

  local path="${metadata_url/\%package\%/$package}"
  local url
  if [[ "$path" == http* ]]; then
    url="$path"
  else
    url="${base}${path}"
  fi

  local data
  data=$(curl -sf -H 'User-Agent: Composer/2.7' "$url" 2>/dev/null) || return 1

  # p2 lists versions (WP Packages); older repos may still key them by version
  echo "$data" | jq -r ".packages.\"${package}\" | if type == \"array\" then .[].version else keys[] end" 2>/dev/null | sort -V
}

# Fetch version list via provider-includes (WPackagist)
# WPackagist uses: provider-includes → per-package provider hash → per-package JSON
get_versions_providers() {
  local base="$1"
  local package="$2"

  local pj
  pj=$(get_packages_json "$base") || return 1

  # Find which provider group contains this package
  local provider_data
  provider_data=$(echo "$pj" | python3 -c "
import sys, json, urllib.request

d = json.load(sys.stdin)
pi = d.get('provider-includes', {})
base = '${base}'
package = '${package}'

for key, val in pi.items():
    sha = val.get('sha256', '')
    url_path = key.replace('%hash%', sha)
    url = f'{base}/{url_path}'
    try:
        req = urllib.request.Request(url, headers={'User-Agent': 'Composer/2.7'})
        resp = urllib.request.urlopen(req)
        data = json.loads(resp.read())
        providers = data.get('providers', {})
        if package in providers:
            pkg_sha = providers[package]['sha256']
            # Fetch the per-package file
            providers_url = d.get('providers-url', '')
            pkg_path = providers_url.replace('%package%', package).replace('%hash%', pkg_sha)
            pkg_url = f'{base}{pkg_path}'
            req2 = urllib.request.Request(pkg_url, headers={'User-Agent': 'Composer/2.7'})
            resp2 = urllib.request.urlopen(req2)
            pkg_data = json.loads(resp2.read())
            versions = pkg_data.get('packages', {}).get(package, {})
            for v in sorted(versions.keys()):
                print(v)
            break
    except Exception:
        continue
" 2>/dev/null) || return 1

  echo "$provider_data" | sort -V
}

# Fetch versions — tries p2 first, falls back to provider-includes
get_versions() {
  local base="$1"
  local package="$2"

  local result
  result=$(get_versions_p2 "$base" "$package" 2>/dev/null)
  if [[ -n "$result" ]]; then
    echo "$result"
    return 0
  fi

  get_versions_providers "$base" "$package" 2>/dev/null
}

# Get latest version from WordPress.org API (ground truth)
get_wporg_latest() {
  local slug="$1"
  curl -sf "https://api.wordpress.org/plugins/info/1.2/?action=plugin_information&request%5Bslug%5D=${slug}&request%5Bfields%5D%5Bversions%5D=0" 2>/dev/null \
    | jq -r '.version // empty' 2>/dev/null
}

# ── Main ──────────────────────────────────────────────────────────

echo "=== Version Freshness Audit ==="
echo "Checking ${#PLUGINS[@]} plugins"
echo ""

# CSV output
CSV="${RESULTS_DIR}/freshness.csv"
echo "plugin,wporg_latest,wpc_latest,wpc_versions,wpkg_latest,wpkg_versions,wpc_has_latest,wpkg_has_latest" > "$CSV"

printf "%-30s  %-12s  %-12s %-6s  %-12s %-6s  %s\n" \
  "PLUGIN" "WP.ORG" "WPC LATEST" "#" "WPKG LATEST" "#" "STATUS"
printf "%-30s  %-12s  %-12s %-6s  %-12s %-6s  %s\n" \
  "──────" "──────" "──────────" "─" "───────────" "─" "──────"

for plugin in "${PLUGINS[@]}"; do
  # Fetch in parallel
  wporg_latest=$(get_wporg_latest "$plugin" 2>/dev/null || echo "?")

  wpc_versions_raw=$(get_versions "$WPC_BASE" "wp-plugin/${plugin}" 2>/dev/null || echo "")
  wpkg_versions_raw=$(get_versions "$WPKG_BASE" "wpackagist-plugin/${plugin}" 2>/dev/null || echo "")

  wpc_count=0
  wpc_latest="—"
  if [[ -n "$wpc_versions_raw" ]]; then
    wpc_count=$(echo "$wpc_versions_raw" | wc -l | tr -d ' ')
    # Ignore dev/alpha/beta/RC versions when determining latest stable
    wpc_latest=$(echo "$wpc_versions_raw" | grep -vE '^dev-|[-.]*(alpha|beta|a\.|b\.|RC|rc|patch)' | tail -1)
    [[ -z "$wpc_latest" ]] && wpc_latest=$(echo "$wpc_versions_raw" | tail -1)
  fi

  wpkg_count=0
  wpkg_latest="—"
  if [[ -n "$wpkg_versions_raw" ]]; then
    wpkg_count=$(echo "$wpkg_versions_raw" | wc -l | tr -d ' ')
    wpkg_latest=$(echo "$wpkg_versions_raw" | grep -vE '^dev-|[-.]*(alpha|beta|a\.|b\.|RC|rc|patch)' | tail -1)
    [[ -z "$wpkg_latest" ]] && wpkg_latest=$(echo "$wpkg_versions_raw" | tail -1)
  fi

  # Determine status
  wpc_has_latest="no"
  wpkg_has_latest="no"
  status=""

  if [[ "$wporg_latest" != "?" ]]; then
    [[ "$wpc_latest" == "$wporg_latest" ]] && wpc_has_latest="yes"
    [[ "$wpkg_latest" == "$wporg_latest" ]] && wpkg_has_latest="yes"

    if [[ "$wpc_has_latest" == "yes" && "$wpkg_has_latest" == "yes" ]]; then
      status="both current"
    elif [[ "$wpc_has_latest" == "yes" ]]; then
      status="WPC ahead"
    elif [[ "$wpkg_has_latest" == "yes" ]]; then
      status="WPKG ahead"
    else
      status="both stale"
    fi
  else
    status="unknown"
  fi

  printf "%-30s  %-12s  %-12s %-6s  %-12s %-6s  %s\n" \
    "$plugin" "$wporg_latest" "$wpc_latest" "$wpc_count" "$wpkg_latest" "$wpkg_count" "$status"

  echo "${plugin},${wporg_latest},${wpc_latest},${wpc_count},${wpkg_latest},${wpkg_count},${wpc_has_latest},${wpkg_has_latest}" >> "$CSV"
done

# Summary
echo ""
echo "── Summary ──"
wpc_current=$(awk -F',' 'NR>1 && $7=="yes" {n++} END {print n+0}' "$CSV")
wpkg_current=$(awk -F',' 'NR>1 && $8=="yes" {n++} END {print n+0}' "$CSV")
total=${#PLUGINS[@]}

echo "  WP Packages:  ${wpc_current}/${total} plugins have latest version"
echo "  WPackagist:   ${wpkg_current}/${total} plugins have latest version"
echo ""
echo "Raw CSV: ${CSV}"
