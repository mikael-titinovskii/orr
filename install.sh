#!/bin/sh
set -eu

REPOSITORY_URL=${ORR_REPOSITORY_URL:-https://github.com/mikael-titinovskii/orr.git}
MIN_GO_MAJOR=1
MIN_GO_MINOR=23

fail() {
  printf 'orr installer: %s\n' "$*" >&2
  exit 1
}

command -v git >/dev/null 2>&1 || fail "Git is required. Install Git and run this command again."

builder=
go_problem="Go 1.23 or newer was not found."
if command -v go >/dev/null 2>&1; then
  go_version=$(go env GOVERSION 2>/dev/null || true)
  go_numbers=${go_version#go}
  go_major=${go_numbers%%.*}
  go_rest=${go_numbers#*.}
  go_minor=${go_rest%%.*}
  case "$go_major.$go_minor" in
    .* | *. | *[!0-9.]*) go_problem="Could not understand installed Go version: $go_version." ;;
    *)
      if [ "$go_major" -gt "$MIN_GO_MAJOR" ] || { [ "$go_major" -eq "$MIN_GO_MAJOR" ] && [ "$go_minor" -ge "$MIN_GO_MINOR" ]; }; then
        builder=go
      else
        go_problem="Go 1.23 or newer is required; found $go_version."
      fi
      ;;
  esac
fi
if [ -z "$builder" ]; then
  command -v docker >/dev/null 2>&1 || fail "$go_problem Install Go 1.23 or newer from https://go.dev/dl/, or install Docker to build orr in a container."
  docker info >/dev/null 2>&1 || fail "$go_problem Docker is installed but not reachable; start Docker or install Go 1.23 or newer from https://go.dev/dl/."
  docker buildx version >/dev/null 2>&1 || fail "$go_problem Building with Docker requires Docker Buildx; install it, or install Go 1.23 or newer from https://go.dev/dl/."
  case "$(uname -s)" in
    Darwin) target_os=darwin ;;
    Linux) target_os=linux ;;
    *) fail "$go_problem Building with Docker supports macOS and Linux only; install Go 1.23 or newer from https://go.dev/dl/." ;;
  esac
  case "$(uname -m)" in
    x86_64 | amd64) target_arch=amd64 ;;
    arm64 | aarch64) target_arch=arm64 ;;
    *) fail "$go_problem Building with Docker supports amd64 and arm64 only; install Go 1.23 or newer from https://go.dev/dl/." ;;
  esac
  builder=docker
fi

key_format_message="OPENROUTER_API_KEY must be 'sk-or-v1-' followed by exactly 64 lowercase hexadecimal characters."

key_is_valid() {
  case "$1" in
    sk-or-v1-*) ;;
    *) return 1 ;;
  esac
  key_payload=${1#sk-or-v1-}
  [ "${#key_payload}" -eq 64 ] || return 1
  case "$key_payload" in
    *[!0-9a-f]*) return 1 ;;
  esac
  return 0
}

openrouter_key=${ORR_OPENROUTER_API_KEY:-}
if [ -n "$openrouter_key" ]; then
  key_is_valid "$openrouter_key" || fail "$key_format_message"
else
  if ! (exec 3<>/dev/tty) 2>/dev/null; then
    fail "An interactive terminal is required to enter OPENROUTER_API_KEY. Set ORR_OPENROUTER_API_KEY to install without a terminal."
  fi
  while :; do
    printf 'OPENROUTER_API_KEY (required): ' >/dev/tty
    stty -echo </dev/tty
    trap 'stty echo </dev/tty 2>/dev/null || true' EXIT HUP INT TERM
    if ! IFS= read -r openrouter_key </dev/tty; then
      stty echo </dev/tty
      trap - EXIT HUP INT TERM
      printf '\n' >/dev/tty
      fail "Could not read OPENROUTER_API_KEY."
    fi
    stty echo </dev/tty
    trap - EXIT HUP INT TERM
    printf '\n' >/dev/tty
    key_is_valid "$openrouter_key" && break
    printf '%s\n' "$key_format_message" >/dev/tty
  done
fi
case "$(uname -s)" in
  Darwin) config_root="$HOME/Library/Application Support" ;;
  *) config_root=${XDG_CONFIG_HOME:-"$HOME/.config"} ;;
esac
config_dir="$config_root/orr"
bin_dir=${XDG_BIN_HOME:-"$HOME/.local/bin"}
script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
tmp_dir=$(mktemp -d 2>/dev/null || mktemp -d -t orr-install)
stage_dir=
cleanup() {
  rm -rf "$tmp_dir"
  if [ -n "$stage_dir" ]; then
    rm -rf "$stage_dir"
  fi
}
trap cleanup EXIT HUP INT TERM

if [ -f "$script_dir/go.mod" ] && [ -d "$script_dir/cmd/orr" ]; then
  source_dir=$script_dir
  printf 'Installing from %s...\n' "$source_dir"
else
  printf 'Downloading orr...\n'
  git clone --depth 1 --quiet "$REPOSITORY_URL" "$tmp_dir/source"
  source_dir="$tmp_dir/source"
fi
mkdir -p "$config_dir" "$bin_dir"

if [ ! -f "$config_dir/.env" ]; then
  cp "$source_dir/.env.example" "$config_dir/.env"
fi
env_tmp="$tmp_dir/env"
found_key=false
while IFS= read -r line || [ -n "$line" ]; do
  case "$line" in
    OPENROUTER_API_KEY=*)
      printf 'OPENROUTER_API_KEY=%s\n' "$openrouter_key" >>"$env_tmp"
      found_key=true
      ;;
    *) printf '%s\n' "$line" >>"$env_tmp" ;;
  esac
done <"$config_dir/.env"
if [ "$found_key" = false ]; then
  printf 'OPENROUTER_API_KEY=%s\n' "$openrouter_key" >>"$env_tmp"
fi
mv "$env_tmp" "$config_dir/.env"
chmod 600 "$config_dir/.env"

if [ "$builder" = go ]; then
  printf 'Building orr with %s...\n' "$go_version"
  (cd "$source_dir" && go build -trimpath -o "$bin_dir/orr" ./cmd/orr)
else
  printf 'Building orr for %s/%s with Docker...\n' "$target_os" "$target_arch"
  stage_dir=$(mktemp -d "$bin_dir/.orr-build.XXXXXX")
  docker build --platform "$target_os/$target_arch" --output "type=local,dest=$stage_dir" "$source_dir"
  mv -f "$stage_dir/orr" "$bin_dir/orr"
fi
chmod 755 "$bin_dir/orr"

printf 'Checking Kimi Code and OpenCode configuration...\n'
"$bin_dir/orr" integrate --env "$config_dir/.env"

case ":$PATH:" in
  *":$bin_dir:"*) ;;
  *) printf '\nAdd %s to PATH, then open a new terminal.\n' "$bin_dir" ;;
esac
printf '\nInstalled orr to %s\nConfiguration: %s\nStart it with: orr serve\n' "$bin_dir/orr" "$config_dir"
printf 'Tab completion: run "orr completion" for setup instructions.\n'
