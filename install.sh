#!/bin/sh

set -eu

repository="rockingsoft/lamplight"
requested_version="${LAMPLIGHT_VERSION:-latest}"

fail() {
  printf 'lamplight installer: %s\n' "$*" >&2
  exit 1
}

command -v curl >/dev/null 2>&1 || fail "curl is required"

case "$(uname -s)" in
  Darwin) os="darwin" ;;
  Linux) os="linux" ;;
  *) fail "unsupported operating system: $(uname -s)" ;;
esac

case "$(uname -m)" in
  x86_64 | amd64) arch="x86_64" ;;
  arm64 | aarch64) arch="arm64" ;;
  *) fail "unsupported architecture: $(uname -m)" ;;
esac

if [ "$requested_version" = "latest" ]; then
  latest_url="$(curl -fsSIL -o /dev/null -w '%{url_effective}' "https://github.com/${repository}/releases/latest")" ||
    fail "could not resolve the latest release"
  tag="${latest_url##*/}"
  tag="${tag%%\?*}"
  [ -n "$tag" ] && [ "$tag" != "latest" ] || fail "could not resolve the latest release"
else
  tag="$requested_version"
  case "$tag" in
    v*) ;;
    *) tag="v${tag}" ;;
  esac
fi

version="${tag#v}"
archive="lamplight_${version}_${os}_${arch}.tar.gz"
download_base="https://github.com/${repository}/releases/download/${tag}"

tmp_dir="$(mktemp -d 2>/dev/null || mktemp -d -t lamplight)" || fail "could not create a temporary directory"
trap 'rm -rf "$tmp_dir"' EXIT HUP INT TERM

curl -fsSL "${download_base}/${archive}" -o "${tmp_dir}/${archive}" || fail "could not download ${archive}"
curl -fsSL "${download_base}/checksums.txt" -o "${tmp_dir}/checksums.txt" || fail "could not download checksums.txt"

expected="$(awk -v file="$archive" '$2 == file { print $1; exit }' "${tmp_dir}/checksums.txt")"
[ -n "$expected" ] || fail "${archive} is missing from checksums.txt"

if command -v sha256sum >/dev/null 2>&1; then
  actual="$(sha256sum "${tmp_dir}/${archive}" | awk '{ print $1 }')"
elif command -v shasum >/dev/null 2>&1; then
  actual="$(shasum -a 256 "${tmp_dir}/${archive}" | awk '{ print $1 }')"
else
  fail "sha256sum or shasum is required to verify the download"
fi
[ "$actual" = "$expected" ] || fail "checksum verification failed for ${archive}"

tar -xzf "${tmp_dir}/${archive}" -C "$tmp_dir" lamplight || fail "could not extract ${archive}"

if [ -n "${LAMPLIGHT_INSTALL_DIR:-}" ]; then
  install_dir="$LAMPLIGHT_INSTALL_DIR"
elif [ -d /usr/local/bin ] && [ -w /usr/local/bin ]; then
  install_dir="/usr/local/bin"
else
  install_dir="${HOME}/.local/bin"
fi

mkdir -p "$install_dir" || fail "could not create ${install_dir}"
install -m 0755 "${tmp_dir}/lamplight" "${install_dir}/lamplight" || fail "could not install into ${install_dir}"

printf 'Installed Lamplight %s at %s/lamplight\n' "$tag" "$install_dir"
case ":${PATH}:" in
  *":${install_dir}:"*) ;;
  *) printf 'Add %s to PATH to run lamplight from any directory.\n' "$install_dir" ;;
esac
