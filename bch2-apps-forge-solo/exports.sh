#!/usr/bin/env bash
# Umbrel runs exports.sh before starting the app. We generate a UNIQUE random secret per
# install for every credential (node RPC, 1175 node RPC, database, internal-API token) and
# persist them so they are stable across restarts. Nothing is ever hardcoded or shared.
set -eo pipefail
umask 077   # the secrets file is created 0600 from the start — no world-readable window

# Umbrel does not guarantee APP_DATA_DIR in the exports.sh context and may source this
# script with `set -u` (nounset) active. exports.sh lives in the app data dir, so derive
# APP_DATA_DIR from this file's own location when unset — never abort on an unbound var.
: "${APP_DATA_DIR:=$(cd "$(dirname "${BASH_SOURCE[0]:-$0}")" && pwd)}"

APP_SECRETS_FILE="${APP_DATA_DIR}/.secrets.env"

if [ ! -f "${APP_SECRETS_FILE}" ]; then
  mkdir -p "${APP_DATA_DIR}"
  # Portable, dependency-free CSPRNG: 32 bytes from /dev/urandom hashed to 64 hex chars.
  # Deliberately avoids `openssl`, which is NOT guaranteed in Umbrel's exports.sh context.
  # This block runs ONLY on a fresh install (updaters keep their .secrets.env and skip it),
  # so an unavailable `openssl` here aborts a brand-new install via `set -e` and shows up as
  # a crash at ~1%. `head` + `sha256sum` are universal (busybox + coreutils); the bounded
  # read means no SIGPIPE, so `set -o pipefail` stays happy.
  gen() { local h; h="$(head -c 32 /dev/urandom | sha256sum)"; printf '%s' "${h:0:64}"; }
  {
    echo "APP_NODE_RPC_PASSWORD=$(gen)"
    echo "APP_1175_RPC_PASSWORD=$(gen)"
    echo "APP_DB_PASSWORD=$(gen)"
    echo "APP_INTERNAL_API_TOKEN=$(gen)"
  } > "${APP_SECRETS_FILE}"
  chmod 600 "${APP_SECRETS_FILE}"
fi

# shellcheck disable=SC1090
. "${APP_SECRETS_FILE}"
export APP_NODE_RPC_PASSWORD APP_1175_RPC_PASSWORD APP_DB_PASSWORD APP_INTERNAL_API_TOKEN
