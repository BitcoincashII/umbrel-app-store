#!/usr/bin/env bash
# Umbrel runs exports.sh before starting the app. We generate a UNIQUE random secret per
# install for every credential (node RPC, 1175 node RPC, database, internal-API token) and
# persist them so they are stable across restarts. Nothing is ever hardcoded or shared.
#
# umbreld SOURCES this file into its own script, so nothing here may change that shell's settings:
# the secrets are made in a subshell with its own umask and options. (A top-level umask 077 here
# made every file umbreld wrote afterwards owner-only.)

# Umbrel does not guarantee APP_DATA_DIR in the exports.sh context and may source this
# script with `set -u` (nounset) active. exports.sh lives in the app data dir, so derive
# APP_DATA_DIR from this file's own location when unset; never abort on an unbound var.
: "${APP_DATA_DIR:=$(cd "$(dirname "${BASH_SOURCE[0]:-$0}")" && pwd)}"

APP_SECRETS_FILE="${APP_DATA_DIR}/.secrets.env"

# Made when missing, and remade when unusable (a first install cut short can leave empty values)
# as long as no database exists yet. A database made with the old passwords could not be opened
# with new ones, so then the file is left as it is for a person to look at.
if ! (for k in APP_NODE_RPC_PASSWORD APP_1175_RPC_PASSWORD APP_DB_PASSWORD APP_INTERNAL_API_TOKEN; do
        grep -Eq "^${k}=[0-9a-f]{64}$" "${APP_SECRETS_FILE}" 2>/dev/null || exit 1
      done) && [ ! -e "${APP_DATA_DIR}/postgres/PG_VERSION" ]; then
  (
    set -eo pipefail
    umask 077   # the secrets file is created 0600 from the start: no world-readable window
    mkdir -p "${APP_DATA_DIR}"
    # Portable, dependency-free CSPRNG: 32 bytes from /dev/urandom hashed to 64 hex chars.
    # Deliberately avoids `openssl`, which is NOT guaranteed in Umbrel's exports.sh context:
    # an unavailable tool here aborts a brand-new install. `head` + `sha256sum` are universal
    # (busybox + coreutils); the bounded read means no SIGPIPE under pipefail.
    gen() { local h; h="$(head -c 32 /dev/urandom | sha256sum)"; printf '%s' "${h:0:64}"; }
    # Written whole, then renamed into place: a file cut short by a power cut is never left behind.
    tmp="${APP_SECRETS_FILE}.tmp.$$"
    {
      echo "APP_NODE_RPC_PASSWORD=$(gen)"
      echo "APP_1175_RPC_PASSWORD=$(gen)"
      echo "APP_DB_PASSWORD=$(gen)"
      echo "APP_INTERNAL_API_TOKEN=$(gen)"
    } > "${tmp}"
    mv -f "${tmp}" "${APP_SECRETS_FILE}"
  )
fi

# shellcheck disable=SC1090
. "${APP_SECRETS_FILE}"
export APP_NODE_RPC_PASSWORD APP_1175_RPC_PASSWORD APP_DB_PASSWORD APP_INTERNAL_API_TOKEN
