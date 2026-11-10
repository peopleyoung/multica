#!/usr/bin/env bash
set -euo pipefail

deployment_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
repository_dir="$(cd -- "$deployment_dir/../.." && pwd)"
build_dir="$repository_dir/.multica/harness-deploy"
go_version=1.26.9
go_checksum=42d158b4d8f7b61ac0a830567c940a86098fb7aac52e467a5ebec03ef5cc2f8d

[[ "$(uname -s)/$(uname -m)" == Linux/x86_64 ]] || {
  echo 'This deployment build requires Linux x86_64.' >&2
  exit 1
}
mkdir -p "$build_dir/backend/bin"
archive="$build_dir/go${go_version}.linux-amd64.tar.gz"
if [[ ! -f "$archive" ]]; then
  curl -fL --retry 2 --connect-timeout 10 --max-time 180 \
    "https://dl.google.com/go/go${go_version}.linux-amd64.tar.gz" -o "$archive"
fi
printf '%s  %s\n' "$go_checksum" "$archive" | sha256sum --check
if [[ ! -x "$build_dir/go/bin/go" ]]; then
  tar -xzf "$archive" -C "$build_dir"
fi

export CGO_ENABLED=0 GOOS=linux GOARCH=amd64 GOMAXPROCS=4 GOTOOLCHAIN=local
export GOCACHE="$build_dir/go-cache" GOMODCACHE="$build_dir/go-modules"
export GOPROXY=https://goproxy.cn
commit="$(git -C "$repository_dir" rev-parse --short=9 HEAD)"
version="${MULTICA_BUILD_VERSION:-harness-$commit}"
build_date="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
# Compile a specialized source copy so upstream and the old instance stay intact.
source_dir="$build_dir/server-source"
rm -rf "$source_dir"
mkdir -p "$source_dir"
cp -a "$repository_dir/server/." "$source_dir/"
node "$deployment_dir/namespace-cookies.mjs" backend "$source_dir"
cd "$source_dir"
for binary in server multica migrate native-migration native-reconcile native-jobs account-admin maintenance backfill_task_usage_hourly backfill_codex_usage_cache; do
  echo "Building $binary ($commit)"
  "$build_dir/go/bin/go" build -p 4 \
    -ldflags "-s -w -X main.version=$version -X main.commit=$commit -X main.date=$build_date" \
    -o "$build_dir/backend/bin/$binary" "./cmd/$binary"
done

mkdir -p "$build_dir/backend/migrations"
cp -a "$repository_dir/server/migrations/." "$build_dir/backend/migrations/"
cp "$repository_dir/LICENSE" "$repository_dir/NOTICE" "$repository_dir/docker/entrypoint.sh" "$build_dir/backend/"
cp "$deployment_dir/Dockerfile.backend" "$build_dir/backend/Dockerfile"
"$deployment_dir/compose.sh" build "$@"
