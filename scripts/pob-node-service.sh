#!/usr/bin/env bash
# scripts/pob-node-service.sh
# -----------------------------------------------------------------------------
# Robust wrapper script for running the PoB node as a background service.
# -----------------------------------------------------------------------------
set -euo pipefail

# -- Default Configuration (Overridable via Environment Variables) -------------
GETH_BIN="${GETH_BIN:-pob-node}"
DATADIR="${DATADIR:-$HOME/.pob-node}"
NETWORK_ID="${NETWORK_ID:-14558}"
HTTP_PORT="${HTTP_PORT:-8545}"
WS_PORT="${WS_PORT:-8546}"
P2P_PORT="${P2P_PORT:-30304}"
LOG_LEVEL="${LOG_LEVEL:-3}"
ETHERBASE="${ETHERBASE:-}"
UNLOCK="${UNLOCK:-}"
PASSWORD_FILE="${PASSWORD_FILE:-$DATADIR/password.txt}"

# Create datadir if it doesn't exist
mkdir -p "$DATADIR"

# -- Helper Functions ----------------------------------------------------------
info() { echo ">>> [$(date +'%Y-%m-%dT%H:%M:%S%z')] $*"; }
error() { echo "ERROR: $*" >&2; }

# -- Flags --------------------------------------------------------------------
flags=(
  --datadir "$DATADIR"
  --networkid "$NETWORK_ID"
  --port "$P2P_PORT"
  --http
  --http.addr "0.0.0.0"
  --http.port "$HTTP_PORT"
  --http.api "eth,net,web3,debug,pob"
  --http.corsdomain "*"
  --ws
  --ws.addr "0.0.0.0"
  --ws.port "$WS_PORT"
  --ws.api "eth,net,web3,debug,pob"
  --ws.origins "*"
  --verbosity "$LOG_LEVEL"
  --syncmode "full"
  --gcmode "archive"
)

# Add mining flags if ETHERBASE is set
if [[ -n "$ETHERBASE" ]]; then
  info "Configuring node as miner with etherbase: $ETHERBASE"
  flags+=(
    --mine
    --miner.etherbase "$ETHERBASE"
    --allow-insecure-unlock
  )
  
  if [[ -n "$UNLOCK" ]]; then
    flags+=(--unlock "$UNLOCK")
  else
    flags+=(--unlock "$ETHERBASE")
  fi

  if [[ -f "$PASSWORD_FILE" ]]; then
    flags+=(--password "$PASSWORD_FILE")
  else
    info "Warning: Mining enabled but PASSWORD_FILE not found at $PASSWORD_FILE"
  fi
fi

# -- Execution -----------------------------------------------------------------
info "Starting PoB Node with binary: $GETH_BIN"
info "Data Directory: $DATADIR"
info "Network ID: $NETWORK_ID"

# Execute geth with the flags
exec "$GETH_BIN" "${flags[@]}" "$@"
