#!/bin/sh
set -eu

CONFIG_PATH="${MKV_PROXY_CONFIG_PATH:-/config.json}"
ROOT_PATH="${GOSTREAM_ROOT_PATH:-/usr/local}"
SOURCE_PATH="${GOSTREAM_SOURCE_PATH:-/mnt/gostream-mkv-real}"
MOUNT_PATH="${GOSTREAM_MOUNT_PATH:-/mnt/gostream-mkv-virtual}"
STATE_DIR="${GOSTREAM_STATE_DIR:-$ROOT_PATH/STATE}"
LOG_DIR="${GOSTREAM_LOG_DIR:-$ROOT_PATH/logs}"
HOST_MOUNT_HINT="${GOSTREAM_HOST_MOUNT_HINT:-}"

# Optional split-config sources (Kubernetes ConfigMap + Secret). When BOTH are
# set, the two JSON fragments are deep-merged into $CONFIG_PATH below. Unset for
# single-file installs (Docker/systemd), which use $CONFIG_PATH verbatim.
CONFIG_TUNING_PATH="${MKV_PROXY_CONFIG_TUNING_PATH:-}"
CONFIG_SECRET_PATH="${MKV_PROXY_CONFIG_SECRET_PATH:-}"

mkdir -p "$SOURCE_PATH" "$MOUNT_PATH" "$ROOT_PATH" "$STATE_DIR" "$LOG_DIR"

mount_is_readable() {
  ls -ld "$1" >/dev/null 2>&1
}

emit_lazy_unmount_hint() {
  hint_path="$MOUNT_PATH"
  if [ -n "$HOST_MOUNT_HINT" ]; then
    hint_path="$HOST_MOUNT_HINT"
  fi

  echo "Mount at $MOUNT_PATH is stale or unreadable." >&2
  echo "If Docker restart/start still fails, lazily unmount the host bind source and retry:" >&2
  echo "  sudo umount -l $hint_path" >&2
}

# Clean up only stale FUSE layers at startup.
# Docker's bind mount for $MOUNT_PATH is always a mountpoint, so a bare
# mountpoint check is not enough. We must leave the bind mount intact and only
# remove an inherited fuse.* layer sitting on top of it.
if mountpoint -q "$MOUNT_PATH" 2>/dev/null; then
  if grep -q " $MOUNT_PATH fuse" /proc/mounts 2>/dev/null; then
    if mount_is_readable "$MOUNT_PATH"; then
      echo "Stale FUSE mount detected at $MOUNT_PATH, cleaning up..." >&2
    else
      echo "Unreadable stale FUSE mount detected at $MOUNT_PATH, cleaning up..." >&2
      emit_lazy_unmount_hint
    fi
    fusermount3 -uz "$MOUNT_PATH" 2>/dev/null || true
    if ! mount_is_readable "$MOUNT_PATH"; then
      emit_lazy_unmount_hint
      exit 1
    fi
  else
    echo "Non-FUSE mountpoint at $MOUNT_PATH (Docker bind), leaving intact." >&2
  fi
fi

# Split-config merge (Kubernetes ConfigMap + Secret).
# When the deployment mounts the non-secret tuning fragment and the secret
# fragment separately, deep-merge them into a single $CONFIG_PATH so gostream
# keeps loading exactly one config.json. The merge is recursive (jq '.[0] * .[1]')
# and the secret fragment wins, so a split object like "plex" (url/library_id in
# tuning, token in the secret) recombines into one object. No secret VALUES are
# ever logged. Skipped entirely when the vars are unset.
if [ -n "$CONFIG_TUNING_PATH" ] || [ -n "$CONFIG_SECRET_PATH" ]; then
  if [ -z "$CONFIG_TUNING_PATH" ] || [ -z "$CONFIG_SECRET_PATH" ]; then
    echo "Config merge needs BOTH MKV_PROXY_CONFIG_TUNING_PATH and MKV_PROXY_CONFIG_SECRET_PATH set" >&2
    exit 1
  fi
  if ! command -v jq >/dev/null 2>&1; then
    echo "Config merge requires jq, which is not installed in this image" >&2
    exit 1
  fi
  for merge_src in "$CONFIG_TUNING_PATH" "$CONFIG_SECRET_PATH"; do
    if [ ! -f "$merge_src" ]; then
      echo "Config merge source not found: $merge_src" >&2
      exit 1
    fi
  done
  mkdir -p "$(dirname "$CONFIG_PATH")"
  merge_tmp="$(mktemp "${CONFIG_PATH}.XXXXXX")"
  if ! jq -s '.[0] * .[1]' "$CONFIG_TUNING_PATH" "$CONFIG_SECRET_PATH" >"$merge_tmp"; then
    echo "Config merge failed combining tuning + secret into $CONFIG_PATH" >&2
    rm -f "$merge_tmp"
    exit 1
  fi
  mv "$merge_tmp" "$CONFIG_PATH"
  echo "Merged tuning ($CONFIG_TUNING_PATH) + secret into $CONFIG_PATH" >&2
fi

if [ ! -f "$CONFIG_PATH" ]; then
  echo "Missing required config file at $CONFIG_PATH" >&2
  exit 1
fi

gostream_pid=""

shutdown() {
  trap - INT TERM EXIT

  if [ -n "$gostream_pid" ] && kill -0 "$gostream_pid" 2>/dev/null; then
    kill -TERM "$gostream_pid" 2>/dev/null || true
  fi

  wait ${gostream_pid:+"$gostream_pid"} 2>/dev/null || true
  fusermount3 -uz "$MOUNT_PATH" 2>/dev/null || true
}

trap shutdown INT TERM EXIT

echo "Starting gostream" >&2
/usr/local/bin/gostream --path "$ROOT_PATH" "$SOURCE_PATH" "$MOUNT_PATH" &
gostream_pid="$!"

wait "$gostream_pid"
exit_code=$?
fusermount3 -uz "$MOUNT_PATH" 2>/dev/null || true
exit "$exit_code"
