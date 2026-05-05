# PoB Chain — Proof-of-Balance EVM Network

| Property | Value |
|---|---|
| **ChainID / NetworkID** | `14558` |
| **Consensus** | Proof-of-Balance (PoB) |
| **Block time** | ~3 seconds |
| **Max validator pool** | 21 (configurable) |
| **Epoch (pool refresh)** | every 200 blocks |
| **Minimum balance** | 1 native token (1e18 wei) |
| **Block reward** | 1 native token |
| **Base fork** | BNB Smart Chain (BSC) |
| **EVM compatibility** | Full — all BSC forks active at genesis |

---

## 1. Consensus Mechanism: Proof-of-Balance (PoB)

### Core Idea

PoB replaces hash-rate with **economic weight** — your mining power is proportional to your on-chain balance. This creates a meritocratic, capital-backed validator set without requiring a separate staking deposit:

```
vote_weight_i = balance_i / Σ balance_j   (j ∈ active pool)
```

### How a Block is Produced

```
┌─────────────────────────────────────────────────────────────────┐
│  Every epoch (200 blocks)                                       │
│  ┌─────────────────────────────────────────────────────────┐   │
│  │  Scan all known accounts                                │   │
│  │  Filter: balance ≥ 1 token                              │   │
│  │  Sort:   descending by balance                          │   │
│  │  Keep:   top 21 → ACTIVE POOL snapshot                  │   │
│  └─────────────────────────────────────────────────────────┘   │
│                                                                 │
│  For every block N                                              │
│  ┌─────────────────────────────────────────────────────────┐   │
│  │  seed = keccak256(parentHash ++ blockNumber)            │   │
│  │  rng  = rand(seed[:8])                                  │   │
│  │  point = rng.Intn(totalPoolBalance)                     │   │
│  │  Walk pool (desc balance) until Σbal > point            │   │
│  │  → Proposer = that validator                            │   │
│  └─────────────────────────────────────────────────────────┘   │
│                                                                 │
│  Block is valid only if Coinbase == Proposer                    │
│  Difficulty = (proposerBalance / totalBalance) × 1_000_000_000 │
└─────────────────────────────────────────────────────────────────┘
```

### Properties

- **Deterministic** — every node independently computes the same proposer for every block using the deterministic seed.
- **Sybil-resistant** — splitting funds across wallets reduces each wallet's probability linearly; total weight is unchanged.
- **Self-regulating pool** — as balances change due to rewards and transfers, the ranked pool re-sorts automatically each epoch.
- **No slashing contract required** — a validator that misses its turn causes a skip; the next scheduled proposer picks up immediately.

---

## 2. Fork Schedule (BSC-compatible)

All forks are activated at block/timestamp `0` (genesis), giving the chain the full modern EVM feature set from day one.

| Fork | Activation |
|---|---|
| Homestead | Block 0 |
| EIP-150 (TangerineWhistle) | Block 0 |
| EIP-155 (SpuriousDragon) | Block 0 |
| EIP-158 | Block 0 |
| Byzantium | Block 0 |
| Constantinople | Block 0 |
| Petersburg | Block 0 |
| Istanbul | Block 0 |
| MuirGlacier | Block 0 |
| Berlin | Block 0 |
| London (EIP-1559) | Block 0 |
| Ramanujan (BSC) | Block 0 |
| Niels (BSC) | Block 0 |
| MirrorSync (BSC) | Block 0 |
| Bruno (BSC) | Block 0 |
| Euler (BSC) | Block 0 |
| Gibbs (BSC) | Block 0 |
| Nano (BSC) | Block 0 |
| Moran (BSC) | Block 0 |
| Planck (BSC) | Block 0 |
| Luban (BSC) | Block 0 |
| Plato (BSC) | Block 0 |
| Hertz (BSC) | Block 0 |
| HertzFix (BSC) | Block 0 |
| Kepler (timestamp) | Unix 0 |
| Feynman (timestamp) | Unix 0 |
| FeynmanFix (timestamp) | Unix 0 |
| Cancun/Dencun | Unix 0 |

---

## 3. Genesis Configuration

The genesis file (`genesis.json`) is intentionally minimal:

- **No precompiled contract pre-allocs** — addresses `0x01`–`0x09` are not pre-funded. The EVM still has access to all built-in precompiles (sha256, ecrecover, etc.) because those are wired into the EVM itself, not into the state trie.
- **System contract placeholders** — three lightweight addresses (`0x1000`, `0x1001`, `0x1002`) are reserved for optional on-chain validator, slash, and reward contracts. They are empty by default.
- **Genesis validator** — `0x3a7a7c25…` is the initial funded account and first pool member. Replace with your own address before deploying.

---

## 4. Getting Started

### Prerequisites

- Go 1.21+
- Git
- Docker + Docker Compose (for the multi-node testnet)

### Build

```bash
# Clone BSC (the base node)
git clone https://github.com/bnb-chain/bsc.git pob-node
cd pob-node

# Copy our PoB engine
cp -r ../consensus/pob ./consensus/pob
cp ../params/pob_config.go ./params/pob_config.go

# Wire the engine (see §5 below for the required code changes)

# Build
make geth
```

### Single-node quickstart

```bash
# 1. Create a validator account
./build/bin/geth account new --datadir ./data
# Note the address printed; set it as ETHERBASE

# 2. Put your password in a file
echo "your-password" > password.txt

# 3. Initialise the chain
./build/bin/geth init --datadir ./data genesis.json

# 4. Start mining
ETHERBASE=0xYOUR_ADDRESS ./scripts/init_node.sh start-miner
```

### Multi-node testnet (Docker)

```bash
# Set validator addresses
export ETHERBASE_1=0xADDR1
export ETHERBASE_2=0xADDR2
export ETHERBASE_3=0xADDR3

# Start
docker compose up --build
```

---

## 5. Wiring PoB into geth

After copying the consensus package, add the following changes to the BSC source tree:

### `consensus/consensus.go`
No changes needed — PoB implements the standard `Engine` interface.

### `eth/ethconfig/config.go`
```go
import "github.com/yourorg/pob-chain/consensus/pob"

// In CreateConsensusEngine, add:
case config.Pob != nil:
    return pob.New(config.Pob, chainDb)
```

### `internal/ethapi/backend.go`
The `pob_` namespace is already registered via `engine.APIs()`.

---

## 6. JSON-RPC API (`pob` namespace)

### `pob_getPoolStatus`
Returns the full current validator pool with weights.

```json
{
  "blockNumber": 1400,
  "poolSize": 3,
  "totalBalance": "3000000000000000000",
  "nextProposer": "0x3a7a7c...",
  "validators": [
    { "address": "0x3a7a7c...", "balance": "2000000000000000000", "weightPct": 66.67 },
    { "address": "0xabcdef...", "balance": "1000000000000000000", "weightPct": 33.33 }
  ]
}
```

### `pob_getProposerAt`
```bash
curl -X POST http://localhost:8545 \
  -H "Content-Type: application/json" \
  -d '{"jsonrpc":"2.0","method":"pob_getProposerAt","params":[1401],"id":1}'
```

### `pob_getCandidateWeight`
```bash
curl -X POST http://localhost:8545 \
  -H "Content-Type: application/json" \
  -d '{"jsonrpc":"2.0","method":"pob_getCandidateWeight","params":["0x3a7a7c25b7d3cf1e2f4b5c6d8e9f0a1b2c3d4e5f"],"id":1}'
# Returns: { "result": 0.6667 }
```

---

## 7. Security Considerations

| Risk | Mitigation |
|---|---|
| Whale monopoly | Diminishing returns: a wallet holding 99 % of supply wins 99 % of blocks — identical to PoS. Governance can lower `poolSize` to increase competition. |
| Balance flash loan | Balances are snapshotted at epoch boundaries, not per-block, so intra-epoch flash loans have no effect. |
| Nothing-at-stake | Validators sign real blocks; a double-sign can be detected by a SlashIndicator contract (0x1001). |
| Sybil via split | Splitting balance across N wallets keeps total weight but reduces each wallet below `poolSize` cut — net-neutral or harmful. |

---

## 8. Project Layout

```
pob-chain/
├── genesis.json                  ← Chain genesis (no precompile allocs)
├── go.mod
├── Dockerfile
├── docker-compose.yml
├── scripts/
│   └── init_node.sh
├── params/
│   └── pob_config.go             ← PoBConfig struct + PoBChainConfig
└── consensus/
    └── pob/
        ├── pob.go                ← Engine (Seal, Finalize, Prepare…)
        ├── snapshot.go           ← Epoch validator-pool snapshots
        └── api.go                ← pob_* JSON-RPC methods
```
