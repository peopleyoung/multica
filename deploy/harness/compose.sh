#!/usr/bin/env bash
set -euo pipefail

deployment_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
repository_dir="$(cd -- "$deployment_dir/../.." && pwd)"

# Pin the instance configuration even when the caller exports other projects' variables.
set -a
source "$deployment_dir/.env"
set +a

exec docker compose --project-directory "$repository_dir" \
  --env-file "$deployment_dir/.env" --project-name multica-harness \
  -f "$repository_dir/docker-compose.selfhost.yml" \
  -f "$deployment_dir/compose.yml" "$@"
