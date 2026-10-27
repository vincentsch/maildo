#!/bin/sh
# Usage: curl -fsSL https://raw.githubusercontent.com/vincentsch/maildo/main/install.sh | sh
set -eu

fail() { printf '%s\n' "maildo: $*" >&2; exit 1; }
work=
staged=
cleanup() {
    [ -z "$staged" ] || rm -f "$staged"
    [ -z "$work" ] || rm -rf "$work"
    return 0
}
trap cleanup 0
trap 'exit 1' HUP INT TERM

os=$(uname -s)
case "$os" in
    Linux) os=linux ;;
    Darwin) os=darwin ;;
    MINGW*|MSYS*|CYGWIN*) fail 'maildo does not support Windows yet. Install it inside WSL (Windows Subsystem for Linux) instead' ;;
    *) fail "there is no maildo build for $os; maildo runs on Linux and macOS" ;;
esac
arch=$(uname -m)
case "$arch" in
    x86_64|amd64|AMD64) arch=amd64 ;;
    aarch64|arm64|ARM64) arch=arm64 ;;
    *) fail "there is no maildo build for $arch; maildo needs an amd64 or arm64 processor" ;;
esac
asset=maildo-$os-$arch
binary=maildo
base=${MAILDO_DOWNLOAD_BASE:-https://github.com/vincentsch/maildo}
if [ -n "${MAILDO_VERSION:-}" ]; then
    # Release tags start with v; accept 0.1.0 as well as v0.1.0.
    case "$MAILDO_VERSION" in v*) ;; *) MAILDO_VERSION=v$MAILDO_VERSION ;; esac
    url=${base%/}/releases/download/$MAILDO_VERSION
else
    url=${base%/}/releases/latest/download
fi
command -v curl >/dev/null 2>&1 || fail 'the installer needs curl to download maildo'
work=$(mktemp -d) || fail 'could not create a temporary folder for the download'
curl -fsSL "$url/$asset" -o "$work/$asset" || fail "could not download $url/$asset"
curl -fsSL "$url/checksums.txt" -o "$work/checksums.txt" || fail "could not download $url/checksums.txt"
expected=$(awk -v name="$asset" '$2 == name {print $1}' "$work/checksums.txt")
[ -n "$expected" ] || fail "checksums.txt has no checksum for $asset"
[ "${#expected}" -eq 64 ] || fail "checksums.txt has a broken checksum for $asset"
case "$expected" in *[!0-9a-fA-F]*) fail "checksums.txt has a broken checksum for $asset" ;; esac
if command -v sha256sum >/dev/null 2>&1; then
    digest=$(sha256sum "$work/$asset") || fail 'could not compute the SHA-256 checksum of the download'
elif command -v shasum >/dev/null 2>&1; then
    digest=$(shasum -a 256 "$work/$asset") || fail 'could not compute the SHA-256 checksum of the download'
else
    fail 'the installer needs sha256sum or shasum to check the download'
fi
actual=${digest%% *}
[ "$actual" = "$expected" ] || fail "the download of $asset does not match its checksum, so nothing was installed"

install_dir=${INSTALL_DIR:-$HOME/.local/bin}
mkdir -p "$install_dir" || fail "could not create $install_dir; set INSTALL_DIR to a folder you can write to"
# Copy the file next to its final place first, so the last step is a
# rename on the same file system and never leaves half a binary behind.
staged=$(mktemp "$install_dir/.maildo.XXXXXX") || fail "could not write to $install_dir; set INSTALL_DIR to a folder you can write to"
cp "$work/$asset" "$staged" || fail "could not copy maildo into $install_dir"
chmod 755 "$staged" || fail 'could not make maildo executable'
mv -f "$staged" "$install_dir/$binary" || fail "could not replace $install_dir/$binary; if maildo is running, stop it and try again"
staged=
printf 'Installed %s in %s.\n' "$binary" "$install_dir"
case ":${PATH:-}:" in
    *":$install_dir:"*) ;;
    *) printf '%s is not on your PATH yet. To add it, run:\n  export PATH="%s:$PATH"\n' "$install_dir" "$install_dir" ;;
esac
printf 'Next: maildo login\n'
