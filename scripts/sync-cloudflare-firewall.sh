#!/usr/bin/env bash
#
# Syncs a DigitalOcean firewall's inbound TCP 80/443 rules with Cloudflare's
# published IP ranges, so only Cloudflare can reach the origin's web ports.
#
# Missing ranges are added before stale ones are removed, so allowed traffic
# is never interrupted. Uses the add/remove rules endpoints (firewall:update
# scope only) rather than PUT, which needs droplet:read and resets any
# attribute left out of the request.
#
# Usage: DO_TOKEN=... FIREWALL_ID=... scripts/sync-cloudflare-firewall.sh [--dry-run]

set -euo pipefail

: "${DO_TOKEN:?DO_TOKEN is required}"
: "${FIREWALL_ID:?FIREWALL_ID is required}"

dry_run=false
if [[ "${1:-}" == "--dry-run" ]]; then
  dry_run=true
fi

api="https://api.digitalocean.com/v2/firewalls/$FIREWALL_ID"
ports=(80 443)

do_api() {
  curl -sS --fail-with-body --retry 3 \
    -H "Authorization: Bearer $DO_TOKEN" \
    -H "Content-Type: application/json" \
    "$@"
}

# Addresses currently allowed on a TCP port, across all matching rules
current_for() {
  jq -c --arg port "$1" '
    [.firewall.inbound_rules[]
      | select(.protocol == "tcp" and .ports == $port)
      | (.sources.addresses // [])[]]
    | unique' <<<"$2"
}

cloudflare=$(curl -sS --fail-with-body --retry 3 https://api.cloudflare.com/client/v4/ips)
desired=$(jq -c 'select(.success == true) | .result | (.ipv4_cidrs + .ipv6_cidrs) | unique' <<<"$cloudflare")

# An empty or truncated list would lock Cloudflare out, so stop instead
if [[ -z "$desired" ]] || (($(jq length <<<"$desired") < 10)); then
  echo "Unexpected response from Cloudflare, refusing to sync:" >&2
  echo "$cloudflare" >&2
  exit 1
fi
echo "Cloudflare publishes $(jq length <<<"$desired") ranges"

firewall=$(do_api "$api")
echo "Firewall: $(jq -r '.firewall.name' <<<"$firewall") ($FIREWALL_ID)"

add='[]'
remove='[]'
for port in "${ports[@]}"; do
  current=$(current_for "$port" "$firewall")
  to_add=$(jq -c --argjson cur "$current" '. - $cur' <<<"$desired")
  to_remove=$(jq -c --argjson want "$desired" '. - $want' <<<"$current")

  echo "TCP $port: add $(jq -c . <<<"$to_add"), remove $(jq -c . <<<"$to_remove")"

  add=$(jq -c --arg port "$port" --argjson addrs "$to_add" '
    if ($addrs | length) > 0
    then . + [{protocol: "tcp", ports: $port, sources: {addresses: $addrs}}]
    else . end' <<<"$add")
  remove=$(jq -c --arg port "$port" --argjson addrs "$to_remove" '
    if ($addrs | length) > 0
    then . + [{protocol: "tcp", ports: $port, sources: {addresses: $addrs}}]
    else . end' <<<"$remove")
done

if [[ "$add" == '[]' && "$remove" == '[]' ]]; then
  echo "Already in sync"
  exit 0
fi

if $dry_run; then
  echo "Dry run, not changing the firewall"
  exit 0
fi

if [[ "$add" != '[]' ]]; then
  do_api -X POST "$api/rules" -d "$(jq -c '{inbound_rules: .}' <<<"$add")"
  echo "Added rules"
fi

if [[ "$remove" != '[]' ]]; then
  do_api -X DELETE "$api/rules" -d "$(jq -c '{inbound_rules: .}' <<<"$remove")"
  echo "Removed rules"
fi

# Verify the result instead of trusting how the API merged the changes
firewall=$(do_api "$api")
for port in "${ports[@]}"; do
  current=$(current_for "$port" "$firewall")
  if [[ "$current" != "$desired" ]]; then
    echo "TCP $port is out of sync after update:" >&2
    echo "  have: $current" >&2
    echo "  want: $desired" >&2
    exit 1
  fi
done
echo "In sync"
