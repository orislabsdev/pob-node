# PolarysDB v2.1.2 — defect report (for upstream fix)

Version audited: `github.com/polarysfoundation/polarysdb/v2 v2.1.2` (module cache), used as the
`ethdb.KeyValueStore` of this geth fork through `ethdb/polarysdb/polarysdb.go`.

This report collects the defects found while running a real node (PoB devnet, path scheme,
archive mode) and the ones that make geth adopt the non-transactional API instead of
`BeginTransaction`/`CommitTransaction`. Every item cites the upstream file and line of v2.1.2,
the observed symptom, and a suggested fix.

Severity:

| Level | Meaning |
|-------|---------|
| P0 | Data loss, silent corruption, or crash of the host process |
| P1 | Wrong results or a store that cannot be trusted for an embedded DB |
| P2 | Performance/amplification that makes the store unusable at scale |
| P3 | API ergonomics that force workarounds in every caller |

---

## P0-1 — Transaction commits are never written to the WAL

*Where:* `database.go:702` `CommitTransaction` (version check at 714-722, `version.Add(1)` at 738).
WAL appends exist only in `processBatch`: `database.go:779` (create), `809` (write), `845`
(delete), `878` (batch).

*Symptom:* everything committed through `BeginTransaction`/`CommitTransaction` is memory only.
Durability depends exclusively on the next full-database dump (`periodicSave` every
`SaveInterval`, default 5 s, or `Flush`). A `kill -9`, an OOM kill, a power loss, or a `log.Crit`
→ `os.Exit(1)` (geth does this on any store error) discards every transaction since the last
successful dump. On a node under load that is the whole chain since the last dump.

*Why it matters here:* a PoB node lost 497 blocks after a clean stop because no dump had ever
succeeded; the transactional writes had no second chance.

*Confirmed in production* (log of 2026-10-28: one `geth init` followed by three start/stop
cycles). Every key written through the non-transactional API came back after the restart, every
key written through a batch did not:

| key | written by | after restart |
|-----|------------|---------------|
| genesis block and state | `geth init` (`Put` + clean `Close`) | present |
| `dbversion` | `rawdb.WriteDatabaseVersion(chainDb, …)` → `Put` | present (`dbversion=9`) |
| PoB candidate list | `consensus/pob.saveCandidates` → `Put` | present (`loaded candidates from disk count=1`) |
| head block hash, blocks 1..11 | `rawdb.WriteHeadBlockHash(batch, …)` → transaction | **gone** (`Loaded most recent local block number=0`) |

The symptom on every restart: the layer journal (a plain file) restores the diff layers of
blocks 1..11, the KV store still holds the genesis head, so the node re-executes block 1 from the
genesis state and logs `Skip add repeated difflayer` for every block it re-seals.

*Repro:*

```go
db, _ := polarysdb.InitWithConfig(cfg)        // SaveInterval = 1h to isolate the WAL
txn, _ := db.BeginTransaction()
txn.Write("t", "k", "v")
db.CommitTransaction(txn)
os.Exit(1)                                    // no graceful Close
// reopen: "k" is gone, and polarysdb.wal contains nothing about it
```

*Suggested fix:* append one WAL entry per batch inside `CommitTransaction` **before** bumping the
version, and add a WAL entry type able to carry a `map[string]any` (`wal.OpBatch`), so a batch is
one record instead of N. `CommitTransaction` should fail if the append fails, before mutating
`db.data`.

---

## P0-2 — The WAL is never truncated, and rotation silently drops history

*Where:* `modules/wal/wal.go:577` `Truncate()` **has zero callers in the whole module**
(`grep -rn "\.Truncate(" .` → only the definition). `rotate()` at `modules/wal/wal.go:504` is
driven by `rotateWatcher` when the file reaches `MaxSize` (100 MB, hardcoded in
`InitWithConfig`): it renames the current file to `polarysdb.wal.<unix>.backup` and a goroutine
deletes that file 30 s later. `ReadAll()` (wal.go:405) only reads the current path.

*Symptom:*

1. The WAL grows without bound up to 100 MB and is fully replayed at every startup.
2. When it rotates, every entry that is not yet in the state file is **deleted 30 seconds later**,
   so a crash in that window loses data that the WAL was supposed to protect.
3. Because nothing truncates the WAL after a successful snapshot, `recoverFromWAL`
   (`database.go:348`) replays entries that are *older* than the state file. Combined with P0-1
   (transaction writes are not in the WAL) this reverts keys: key `K` is written at `T1`
   (WAL entry, value `V1`), updated at `T2` through a transaction (no WAL entry, `V2`), the state
   file is saved at `T3` (`V2` on disk), and the next start replays the WAL and puts `V1` back.

*Suggested fix:*

* Truncate the WAL right after `storage.Save` succeeds in `flushToDisk`/`Flush`, under the same
  lock, after the file is fsynced.
* Make rotation conditional on a prior successful save (rotate only when the snapshot covers the
  segment).
* Give WAL entries a monotonically increasing sequence number and persist the last persisted
  sequence in the state file, so recovery replays only `> lastPersisted` (idempotent, immune to
  mixed transactional/non-transactional writes).

---

## P0-3 — `OpBatch` rolls back to the wrong key, and cannot delete

*Where:* `database.go:889` inside the `case OpBatch` of `processBatch`:

```go
if err := db.wal.Append(&wal.Entry{... Key: key, Value: value}); err != nil {
    err = fmt.Errorf("WAL write failed: %w", err)
    t[op.Key] = oldVal          // op.Key is "" for OpBatch
}
```

*Symptom:* a WAL append failure writes `oldVal` into the key `""` instead of restoring `key`, so
the batch corrupts a foreign key and leaves the failed one applied. `successCount++` at
`database.go:897` runs regardless, so the caller gets an error but the version/dirty flag still
advance.

Also, `OpBatch` assigns `t[key] = value` unconditionally: a `nil` value is stored instead of
deleting the key (unlike `CommitTransaction`, where `nil` deletes). There is **no batched delete
API at all**, which is what prevents geth from dropping transactions (see "What geth needs").

*Suggested fix:* `t[key] = oldVal`; `if value == nil { delete(t, key) }`; only count success when
every record was applied and logged; add `DeleteBatch(table string, keys []string)` (or
`WriteBatch(table, records, deletes)`).

---

## P0-4 — `Load` returns an empty database with a `nil` error when the file is missing

*Where:* `modules/storage/engine.go:83-90`

```go
info, err := os.Stat(e.path)
if os.IsNotExist(err) {
    return make(map[string]map[string]any), time.Time{}, nil
}
```

*Symptom:* a datadir that was moved, a DB deleted by mistake, or a `DirPath` resolved to a
different absolute path opens as a brand-new empty store **without any error**. For an embedded
chain store this means `geth` prints `Loaded most recent local block number=0` and restarts the
chain from genesis; the operator sees a healthy node that silently lost the chain.

*Suggested fix:* return a distinguishable error (`ErrNotExist`) and let the application decide,
or add `AllowMissingStateFile bool` to `Config` (default false). At minimum log at error level.

---

## P1-1 — Global-version OCC makes transactions unusable under concurrency

*Where:* `database.go:673` `BeginTransaction` → `createSnapshot` (`database.go:691-700`), which
**deep copies every record of every table** and captures `db.version`; `CommitTransaction`
rejects the commit if any other write bumped the version (`database.go:714-722`), including a
write applied by the internal write buffer (`database.go:920`).

*Symptom:* `transaction conflict: data modified since snapshot (expected v80003, current v80004).
Retry the transaction`. With a single global counter and an eager snapshot, a commit only
succeeds if *no* other writer ran between `BeginTransaction` and `CommitTransaction` — for a
geth batch opened and committed around concurrent state writes this is close to never. Worse,
the snapshot is `O(entire database)` in time and memory per transaction: with a multi-GB DB every
block allocates and serializes a copy of the whole store, then immediately discards it.

*Repro:* `go test` with two goroutines looping `BeginTransaction` → `Write` → `CommitTransaction`
on the same `Database`: one of them starves.

*Suggested fix:* per-table or per-key versioning (or a real MVCC/`Badger`-style txn layer). If a
full MVCC is out of scope, document "single writer only" and stop validating against a global
counter (P0-1 gives the durability anyway).

---

## P1-2 — The write ack is delivered before the version is bumped

*Where:* `database.go:913` (`op.ResultCh <- err`) and `database.go:920` (`db.version.Add(1)`)
inside `processBatch`: the ack is sent per operation, the version bump happens after the loop.

*Symptom:* a caller that returns from `Write`/`WriteBatch` and immediately opens a transaction
can snapshot the *previous* version, and the pending bump of the caller that acknowledged first
then invalidates it. This is the residual race behind "conflicting" commits even with a single
application-level writer.

*Suggested fix:* bump the version before acknowledging the drained operations.

---

## P1-3 — Values lose their type on reload: `[]byte` comes back base64-encoded

*Where:* `database.go:1252` `db.to` and `database.go:1296` `deepCopyValue`. Writing a Go struct
goes through a JSON round trip into `map[string]any`; on read the JSON mapper copies the base64
text of a `[]byte` field **verbatim** into the destination `[]byte` instead of decoding it.

*Symptom:* data written and read in the same process is correct (the value is still the struct
until it is persisted), but after a restart every `[]byte` field contains the base64 text:

```
write: []byte("val1") → read before restart: "val1" → read after restart: "dmFsMQ==" (8 bytes)
```

For a KV store this is silent corruption: geth hashes 8-byte base64 text as a 32-byte trie hash
and every lookup misses (`missing trie node ...`, `Genesis state is missing`).

*Repro:* `Write("t","k", struct{ B []byte `json:"b"` }{[]byte("val1")})`, `Close`, reopen, `Read`.

*Suggested fix:* in `db.to`, decode base64 explicitly (`base64.StdEncoding.DecodeString`) for
`[]byte` destinations, or round trip through `json.Unmarshal` for every case instead of the
reflect mapper. Additionally, keep the in-memory representation identical to the persisted one
(store what you will read).

*Workaround in this repo:* `ethdb/polarysdb` stores `map[string]any{"k": base64(key), "v":
base64(value)}` and always decodes through `encoding/json`, so both the old struct layout and
the map layout are readable.

---

## P2-1 — Two full serializations of the whole database every cycle

*Where:* `periodicSave` (`database.go:963`, `SaveInterval` 5 s) → `flushToDisk`
(`database.go:1032`) → `storage.Save` = `json.Marshal(entire data)` + encrypt + temp file +
fsync + rename (`modules/storage/engine.go:43`). `fileOnChange` (`database.go:989`,
`WatchInterval` 3 s) calls `load()` (`database.go:1064`) → `storage.Load` =
read + decrypt + `json.Unmarshal(entire data)` whenever the mtime differs from `lastLoaded`,
which is the case after every save.

*Symptom:* `O(database)` write + `O(database)` read per 5 seconds, on top of GC pressure from the
temporary copies. Fine at 10 MB, unusable at 1 GB (an archive-state geth DB reaches that in
hours). `Flush()` (flush.go:40) additionally reads the DB back to verify, tripling the cost.

*Suggested fix:* incremental persistence (append-only segments / SSTable-like or LSM), or at
least per-table dirty tracking, and stop reloading the file we just wrote (`fileOnChange` should
be a read-only-mode feature, or compare against the last save we performed).

---

## P2-2 — Every single write costs up to 100 ms

*Where:* `processWriteBuffer` (`database.go:747` — `time.NewTicker(100 * time.Millisecond)`,
drains at 50 ops), and `Write`/`Delete`/`WriteBatch` block on `ResultCh`
(`database.go:436`, `470`, `499`).

*Symptom:* a lone `Write` or `Delete` waits for the ticker; measured on this repo's wrapper:
200 sequential `Put` calls took ~7 s (~35 ms each). A geth block batch that ends up as thousands
of individual writes takes minutes per block.

*Suggested fix:* drain when the channel is empty (the `default:` branch already exists for the
second select, use it), or expose a non-blocking `WriteAsync`/`Sync()` pair, and provide the
batched delete from P0-3 so callers never need per-key calls.

---

## P2-3 — `ReadBatch` returns values without keys, and there is no iterator

*Where:* `database.go:560` `ReadBatch(table string, dst any)` appends `deepCopyValue(value)` for
every record, with no way to know the key it belonged to; there is no cursor/iterator API.

*Symptom:* any `ethdb.KeyValueStore` on top must duplicate the key inside the value (extra
bytes on disk and in memory for every record), and every `NewIterator` requires a full table
scan plus a deep copy of every value — geth calls `NewIterator` for range deletes, freezers and
snapshots.

*Suggested fix:* `ReadBatch` into `map[string]any`, and an iterator with `Seek(key)` /
prefix bounds that walks the underlying map without copying values.

---

## P3-1 — No sentinel errors

*Where:* `Read` returns `fmt.Errorf("data with key=%s does not exists")` and
`fmt.Errorf("table does not exist")` (`database.go:525-560`).

*Symptom:* callers cannot distinguish "miss" from "I/O failure" without string matching; the
geth wrapper currently maps *every* error to `errPolarysNotFound`, which hides real failures.

*Suggested fix:* export `ErrKeyNotFound` and `ErrTableNotFound` and wrap them with `%w`.

---

## P3-2 — A library calls `log.Fatalf`

*Where:* `modules/config/config.go:46`, `54`, `65`.

*Symptom:* a bad directory or a missing `$HOME` kills the host process (`os.Exit(1)`) from inside
a directory helper, bypassing deferred cleanup, the DB close (so the last writes are lost) and any
diagnostics the application would print.

*Suggested fix:* return errors; let the application decide.

---

## P3-3 — Backups default to on, inside the data directory

*Where:* `DefaultConfig()` sets `EnableBackup: true`, `BackupInterval: 1 h`, `KeepCount: 10`
(hardcoded in `InitWithConfig`), and `cfg.BackupDir = config.GetHomeSubDir("backups",
cfg.DirPath)` — with an absolute `DirPath` that is `<DirPath>/backups`, i.e. inside the store.

*Symptom:* up to ten full encrypted copies of the database created inside the store directory
(and therefore inside any rsync/snapshot of it), each one another `O(database)` serialization
competing with the periodic save for I/O and disk.

*Suggested fix:* default `EnableBackup` to false, make `KeepCount` configurable, and keep backups
outside `DirPath`.

---

## P3-4 — Relative `DirPath` is silently resolved against `$HOME`

*Where:* `modules/config/config.go:31-70` (`getHomeSubDir`).

*Symptom:* `cfg.DirPath = "data/geth/chaindata"` writes to `$HOME/data/...`. A service started by
systemd with a different `HOME` (or none) reads a different database than the one the operator
initialized, and `polarysdb.wal` (used by geth to detect the engine) ends up in a third place.
Combined with P0-4 the node comes up empty without an error.

*Suggested fix:* require an absolute `DirPath`, or resolve relative paths against the process CWD
and log the result. (This repo now always passes an absolute path.)

---

## What geth needs to drop transactions

> **Status:** the wrapper no longer uses transactions (see the last section). The list below is
> what the library still has to provide so that the tombstone workaround can be dropped.

`ethdb.KeyValueStore` needs: point `Put`/`Delete`, atomic `batch.Write()` with puts **and**
deletes, `NewIterator(prefix, start)` with seek, `Stat`, `Compact`, `SyncKeyValue`. The
non-transactional API covers everything except the batched delete, which is why this repo now
records deletions as tombstones instead of using transactions.

Minimal upstream changes, in priority order:

1. `DeleteBatch` / `nil`-value deletes in `OpBatch` (P0-3) — without it a geth batch cannot be a
   single `WriteBatch` call and has to keep tombstones for the deleted keys.
2. One WAL record per batch, appended by both `WriteBatch` and `CommitTransaction` (P0-1), plus
   truncate-after-save and the persisted sequence number (P0-2).
3. Drain the write buffer when it is empty instead of on a 100 ms ticker (P2-2).
4. `ReadBatch` into `map[string]any` and a real iterator (P2-3).

With 1-3 in place, `ethdb/polarysdb` can implement `batch.Write()` as:

```go
db.WriteBatch(defaultTable, puts)   // one WAL record, no snapshot, no version check
db.DeleteBatch(defaultTable, deletes)
```

…and drop the tombstones without touching anything else.

---

## Measures already applied in `ethdb/polarysdb`

* **Batches are written through the WAL-backed API, not through a transaction.** `batch.Write()`
  encodes the buffered puts and hands them to `WriteBatch` (chunked at 50 000 records, half of the
  library ceiling). This removes the OCC conflict class (P1-1/P1-2), the `O(entire database)`
  snapshot per commit, and — the reason for the change — the durability hole of P0-1: a block is
  on disk as soon as `Write` returns, because the record is already in `polarysdb.wal`.
  `cfg.EnableTransactions` is now `false` so the tx manager is not even created.
* **Batched deletes are tombstones.** `record{"k": base64(key), "d": true}` replaces a deleted
  key inside the same `WriteBatch` call; `Get`/`Has` report it as absent and `NewIterator` skips
  it. A tombstone is the only way to delete a key durably in a batch, because there is no batched
  delete (P0-3) and one `Delete` call per key costs a full write-buffer tick (~100 ms). Point
  `Delete` still removes the record physically.
* `wmu` serializes `Put`/`Delete`/`batch.Write` so the WAL keeps the order the callers produced.
* Absolute `DirPath` (P3-4 workaround).
* `map[string]any` + base64 record layout, so reads after a restart return the original bytes
  despite P1-3.
* `EnableBackup = false` (P3-3 workaround).
* Startup guard: refuse to open a data directory that has chain artifacts but no state file
  (P0-4 workaround), instead of silently restarting the chain from genesis.
* Persistence watchdog: geth-level `ERROR` when mutations are pending but the state file does not
  advance for 60 s, followed by an explicit `Flush()` whose error is logged — the library only
  reports a failing periodic save on its own console channel, which is easily lost.

### Cost of the workaround

* Every `batch.Write()` is one write-buffer round trip, i.e. up to 100 ms (P2-2). With a 3 s block
  time this is fine; it is recorded here so nobody is surprised by it.
* Tombstones are never reclaimed: a key that is deleted and never written again keeps a small
  record forever, and `NewIterator` (which reads the whole table, P2-3) has to skip it.
* `polarysdb.wal` is never truncated (P0-2), so it grows until the 100 MB rotation, and rotation
  drops the segment 30 s later. On a long running node the WAL and the state file have to be
  watched until P0-2 is fixed upstream.
* The state file remains the primary persistence path; when its dump works, everything before the
  last dump is redundant. If a machine ever restores an old copy of `state.rdb`, the WAL replay
  brings back the writes that came after it, which is exactly what makes the restart safe.
