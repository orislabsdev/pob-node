#!/usr/bin/env bash
# scripts/init_node.sh
# ─────────────────────────────────────────────────────────────────────────────
# Initialises and starts a PoB chain node.
# Usage:
#   ./scripts/init_node.sh init          — initialise the data dir from genesis
#   ./scripts/init_node.sh start         — start the node (miner disabled)
#   ./scripts/init_node.sh start-miner   — start as a mining validator
#   ./scripts/init_node.sh account new   — create a new keystore account
# ─────────────────────────────────────────────────────────────────────────────
set -euo pipefail

# ── Config ────────────────────────────────────────────────────────────────────
GETH="${GETH:-geth}"              # path to the patched geth binary
DATADIR="${DATADIR:-./data}"
GENESIS="${GENESIS:-./genesis.json}"
CHAIN_ID=14558
NETWORK_ID=14558
HTTP_PORT=8545
WS_PORT=8546
P2P_PORT=30304
LOG_LEVEL=3                       # 3=info 4=debug 5=trace

# ── Helpers ───────────────────────────────────────────────────────────────────
die() { echo "ERROR: $*" >&2; exit 1; }
info() { echo ">>> $*"; }

cmd_init() {
  info "Initialising data directory: $DATADIR"
  "$GETH" init \
    --datadir "$DATADIR" \
    "$GENESIS"
  info "Done."
}

cmd_account_new() {
  info "Creating new account..."
  "$GETH" account new --datadir "$DATADIR"
}

common_flags=(
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

cmd_start() {
  info "Starting node (non-mining)..."
  "$GETH" "${common_flags[@]}"
}

cmd_start_miner() {
  local ETHERBASE="${ETHERBASE:-}"
  local UNLOCK="${UNLOCK:-}"
  local PASSWORD_FILE="${PASSWORD_FILE:-./password.txt}"

  [[ -z "$ETHERBASE" ]] && die "Set ETHERBASE to your validator address."
  [[ -z "$UNLOCK" ]]    && UNLOCK="$ETHERBASE"

  info "Starting mining node with coinbase $ETHERBASE ..."
  "$GETH" "${common_flags[@]}" \
    --mine \
    --miner.etherbase "$ETHERBASE" \
    --unlock "$UNLOCK" \
    --password "$PASSWORD_FILE" \
    --allow-insecure-unlock
}

# ── Entrypoint ────────────────────────────────────────────────────────────────
case "${1:-}" in
  init)          cmd_init ;;
  account)       cmd_account_new ;;
  start)         cmd_start ;;
  start-miner)   cmd_start_miner ;;
  *)
    echo "Usage: $0 {init|account|start|start-miner}"
    exit 1
    ;;
esac
