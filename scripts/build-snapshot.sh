#!/bin/sh
set -eu

goreleaser release --snapshot --clean --skip=docker

version=$(sed -n 's/.*"version"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' dist/metadata.json)
if [ -z "$version" ]; then
  echo "error: could not read snapshot version from dist/metadata.json" >&2
  exit 1
fi

build_context=$(mktemp -d)
trap 'rm -rf -- "$build_context"' EXIT INT TERM

for arch in amd64 arm64; do
  binary=$(find dist -maxdepth 2 -type f -path "*/lamplight_linux_${arch}*/lamplight" -print)
  count=$(printf '%s\n' "$binary" | sed '/^$/d' | wc -l | tr -d ' ')
  if [ "$count" -ne 1 ]; then
    echo "error: expected one linux/$arch Lamplight binary, found $count" >&2
    exit 1
  fi
  mkdir -p "$build_context/linux/$arch"
  install -m 0755 "$binary" "$build_context/linux/$arch/lamplight"
done

install -m 0644 Dockerfile "$build_context/Dockerfile"

image="ghcr.io/rockingsoft/lamplight:$version"
oci_archive="$(pwd)/dist/lamplight_${version}_executor_multiarch.oci.tar"

docker buildx build \
  --file "$build_context/Dockerfile" \
  --platform linux/amd64,linux/arm64 \
  --output "type=oci,dest=$oci_archive" \
  "$build_context"

server_arch=$(docker version --format '{{.Server.Arch}}')
case "$server_arch" in
  amd64|x86_64) platform=linux/amd64 ;;
  arm64|aarch64) platform=linux/arm64 ;;
  *)
    echo "error: unsupported Docker Engine architecture: $server_arch" >&2
    exit 1
    ;;
esac

docker buildx build \
  --file "$build_context/Dockerfile" \
  --platform "$platform" \
  --load \
  --tag "$image" \
  "$build_context"

printf 'Built CLI artifacts, multi-architecture executor OCI image %s, and local image %s\n' "$oci_archive" "$image"
