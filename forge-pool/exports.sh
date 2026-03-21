#!/bin/bash

# Generate random passwords on first run
if [ ! -f "${APP_DATA_DIR}/.initialized" ]; then
    export NODE_RPC_PASSWORD=$(openssl rand -hex 32)
    export DB_PASSWORD=$(openssl rand -hex 32)

    # Save for future restarts
    echo "NODE_RPC_PASSWORD=${NODE_RPC_PASSWORD}" > "${APP_DATA_DIR}/.env"
    echo "DB_PASSWORD=${DB_PASSWORD}" >> "${APP_DATA_DIR}/.env"

    touch "${APP_DATA_DIR}/.initialized"
else
    # Load existing passwords
    source "${APP_DATA_DIR}/.env"
fi

# User-configurable settings (can be changed in Umbrel UI)
export POOL_NAME="${POOL_NAME:-My Forge Pool}"
export POOL_FEE="${POOL_FEE:-1.0}"
export POOL_ADDRESS="${POOL_ADDRESS:-}"
export MIN_PAYOUT="${MIN_PAYOUT:-5}"
export STRATUM_PORT="${STRATUM_PORT:-3333}"
export APP_PORT="${APP_PORT:-3080}"
