// Copyright 2024 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package polarysdb

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/log"
	"github.com/polarysfoundation/polarysdb/v2"
)

const (
	defaultTable = "pob_chain"

	// maxWriteBatch bounds how many records are handed to the backing store in a
	// single call. PolarysDB rejects a call with more than 100000 records, so the
	// limit leaves headroom below that ceiling.
	maxWriteBatch = 50000

	// persistenceWatchInterval is how often the watchdog compares the writes
	// handed to the backing store with the progress of its state file.
	persistenceWatchInterval = 15 * time.Second

	// persistenceStallChecks is the number of consecutive checks without any
	// progress of the state file that are tolerated before the stall is
	// reported. Reported stalls repeat at the same cadence.
	persistenceStallChecks = 4
)

var (
	errPolarysNotFound = errors.New("not found")
)

// Database is a persistent key-value store based on PolarysDB.
type Database struct {
	db   *polarysdb.Database
	path string

	// wmu serializes the writes issued by this process, which keeps the WAL in
	// the order the callers produced it and stops the chunks of one batch from
	// being interleaved with the writes of another goroutine.
	wmu sync.Mutex

	// stateFile is the file PolarysDB dumps its in-memory tables to. Mutations
	// are only durable once that file has been refreshed, so its progress is
	// watched to surface a backing store that stopped persisting.
	stateFile string

	// pending counts the mutations accepted since the process started.
	pending atomic.Uint64

	// closeOnce guards closeCh, which terminates the persistence watchdog.
	closeOnce sync.Once
	closeCh   chan struct{}
}

// stateFile returns the file PolarysDB dumps its tables to. The layout mirrors
// the path resolution of the backing store itself.
func stateFile(dir string) string {
	return filepath.Join(dir, "state", "state.rdb")
}

// checkPreviousSession refuses to open a brand new database in a directory that
// already holds chain data.
//
// PolarysDB loads a missing state file as an empty database without reporting an
// error, so a database that was moved, deleted or written under a different
// path comes up empty and the node silently restarts the chain from genesis
// instead of failing. Only the combination of a missing state file and chain
// artifacts of a previous run is treated as an error, a genuinely fresh data
// directory opens normally.
func checkPreviousSession(dir string) error {
	file := stateFile(dir)
	if _, err := os.Stat(file); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	// Without a state file, look for leftovers of a previous session. The ancient
	// store is per store, while the layer journal lives in the triedb directory
	// of the geth data directory the store belongs to.
	for _, artifact := range []struct {
		name     string
		isDir    bool
		contains bool
	}{
		{filepath.Join(dir, "ancient", "chain"), true, true},
		{filepath.Join(filepath.Dir(dir), "triedb", "merkle.journal"), false, false},
	} {
		var exists bool
		if artifact.isDir {
			entries, err := os.ReadDir(artifact.name)
			exists = err == nil && (!artifact.contains || len(entries) > 0)
		} else {
			_, err := os.Stat(artifact.name)
			exists = err == nil
		}
		if exists {
			return fmt.Errorf("polarysdb state file %s is missing although %s already holds data of a previous session: refusing to start on an empty database, check the data directory and the user the node runs as", file, artifact.name)
		}
	}
	return nil
}

// New returns a wrapped PolarysDB object.
func New(path string, cache int, handles int, namespace string, readonly bool) (*Database, error) {
	// Use absolute path for stable key derivation regardless of working directory
	absPath, _ := filepath.Abs(path)
	h := sha256.Sum256([]byte(absPath))
	encryptionKey := polarysdb.GenerateKeyFromBytes(h[:])

	if err := checkPreviousSession(absPath); err != nil {
		return nil, err
	}

	// The backing store resolves relative directories under $HOME, which silently
	// relocates the database (and its WAL) when the node is started by a service
	// running as a different user than the one that initialized it. Always hand
	// it the absolute directory instead.
	cfg := polarysdb.DefaultConfig()
	cfg.DirPath = absPath
	cfg.EncryptionKey = encryptionKey

	// PolarysDB manages WAL and synchronization automatically. Explicit
	// transactions are not used: the backing store never logs a commit to its
	// WAL, so everything written through them is only as durable as the next
	// full dump of the state file.
	cfg.EnableTransactions = false
	cfg.EnableIndexes = false
	cfg.EnableCompression = true
	cfg.Debug = true

	// Background backups serialize the whole database into an unencrypted copy
	// under the backup directory (up to ten of them per database) on every
	// backup interval. The node keeps its own chain backups and the extra copies
	// only compete with the periodic save for disk space and I/O.
	cfg.EnableBackup = false

	db, err := polarysdb.InitWithConfig(cfg)
	if err != nil {
		return nil, err
	}

	// Ensure the default table exists
	exists, err := db.Exist(defaultTable)
	if err != nil {
		return nil, err
	}

	if !exists {
		if err := db.Create(defaultTable); err != nil {
			db.Close()
			return nil, err
		}
	}

	database := &Database{
		db:        db,
		path:      absPath,
		stateFile: stateFile(absPath),
		closeCh:   make(chan struct{}),
	}
	log.Info("Allocated PolarysDB storage", "dir", absPath, "state_file", database.stateFile)
	go database.watchPersistence()

	return database, nil
}

// watchPersistence reports a backing store that stops writing our mutations to
// its state file.
//
// PolarysDB keeps the whole database in memory and appends to the WAL only on
// its non-transactional write path, so everything committed through a batch
// becomes durable with the next periodic dump of the state file. That dump
// fails silently from the perspective of the node (the library logs it through
// its own console logger, which is easily lost in service logs), which means a
// full disk or a permission problem costs every block written since the last
// successful dump without any visible symptom.
func (d *Database) watchPersistence() {
	ticker := time.NewTicker(persistenceWatchInterval)
	defer ticker.Stop()

	var (
		lastSize int64
		lastMod  time.Time
		saved    uint64
		stalled  int
	)
	for {
		select {
		case <-d.closeCh:
			return
		case <-ticker.C:
		}

		writes := d.pending.Load()
		info, err := os.Stat(d.stateFile)
		if err != nil {
			if writes > 0 {
				log.Error("PolarysDB state file is unreadable, writes are not being persisted", "file", d.stateFile, "err", err, "pending_writes", writes)
			}
			continue
		}
		if info.Size() != lastSize || !info.ModTime().Equal(lastMod) {
			// The backing store saved, remember how far it got.
			lastSize, lastMod, saved, stalled = info.Size(), info.ModTime(), writes, 0
			continue
		}
		if writes == saved {
			// Nothing was written since the last save, so an unchanged state file
			// is expected.
			stalled = 0
			continue
		}
		stalled++
		if stalled%persistenceStallChecks != 0 {
			continue
		}
		log.Error("PolarysDB is not persisting writes",
			"file", d.stateFile, "size", info.Size(), "last_save", info.ModTime(),
			"pending_writes", writes-saved, "hint", "check the free space and the permissions of the data directory")
		// The backing store reports the reason its periodic dump stopped on its
		// own console channel, which is easily lost in the service logs. Force a
		// save through the public API so the failure makes it into the node log.
		start := time.Now()
		if err := d.db.Flush(); err != nil {
			log.Error("PolarysDB flush failed", "file", d.stateFile, "err", err, "elapsed", time.Since(start))
		} else {
			log.Warn("PolarysDB flushed after a persistence stall", "file", d.stateFile, "elapsed", time.Since(start))
		}
	}
}

// Close closes the database.
func (d *Database) Close() error {
	d.closeOnce.Do(func() { close(d.closeCh) })

	err := d.db.Close()
	if err != nil {
		// The final dump of the state file failed, everything written since the
		// last successful save is lost if the process exits now.
		log.Error("Failed to persist PolarysDB on close", "file", d.stateFile, "err", err)
	}
	return err
}

// polarysRecord is used to store both key and value in PolarysDB,
// since ReadBatch only returns values and we need keys for iteration.
type polarysRecord struct {
	Key []byte `json:"k"`
	Val []byte `json:"v"`

	// Del marks a tombstone, a key that was deleted and must be reported as
	// absent. Deleting for real is only possible one key at a time, so batched
	// deletions are recorded as tombstones instead (see encodeTombstone).
	Del bool `json:"d,omitempty"`
}

// Field names under which the key and the value are stored.
const (
	recordKeyField = "k"
	recordValField = "v"
	recordDelField = "d"
)

// encodeRecord wraps a key/value pair for storage.
//
// The pair is stored as a JSON friendly map instead of a struct because
// PolarysDB only decodes JSON maps into Go values: a value written as a struct
// is read back correctly while the process lives, but is handed out as a
// map[string]any once it has been reloaded from disk, and the library's JSON
// mapper copies the base64 text into []byte fields verbatim instead of decoding
// it. Keeping both representations identical avoids that. Keys and values are
// base64 encoded explicitly, which is also how encoding/json represents []byte
// fields, so databases written by earlier versions remain readable.
func encodeRecord(key, value []byte) map[string]any {
	return map[string]any{
		recordKeyField: base64.StdEncoding.EncodeToString(key),
		recordValField: base64.StdEncoding.EncodeToString(value),
	}
}

// encodeTombstone marks a key as deleted without removing it.
//
// PolarysDB deletes one key per call and every call waits for the next drain of
// its write buffer, so deleting the keys of a batch one by one would turn a
// block write into minutes of waiting. A tombstone travels in the same
// WAL-backed batch as the writes and is filtered out on read.
func encodeTombstone(key []byte) map[string]any {
	return map[string]any{
		recordKeyField: base64.StdEncoding.EncodeToString(key),
		recordDelField: true,
	}
}

// decodeRecord unpacks a stored value. Values written as structs by an earlier
// version of this package are still accepted.
func decodeRecord(raw any) (polarysRecord, error) {
	if record, ok := raw.(polarysRecord); ok {
		return record, nil
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return polarysRecord{}, err
	}
	var record polarysRecord
	if err := json.Unmarshal(encoded, &record); err != nil {
		return polarysRecord{}, err
	}
	return record, nil
}

// Has retrieves if a key is present in the key-value store.
func (d *Database) Has(key []byte) (bool, error) {
	var raw map[string]any
	if err := d.db.Read(defaultTable, hex.EncodeToString(key), &raw); err != nil {
		return false, nil
	}
	record, err := decodeRecord(raw)
	if err != nil {
		return false, err
	}
	return !record.Del, nil
}

// Get retrieves the given key if it's present in the key-value store.
func (d *Database) Get(key []byte) ([]byte, error) {
	var raw map[string]any
	if err := d.db.Read(defaultTable, hex.EncodeToString(key), &raw); err != nil {
		return nil, errPolarysNotFound
	}
	record, err := decodeRecord(raw)
	if err != nil {
		return nil, err
	}
	if record.Del {
		return nil, errPolarysNotFound
	}
	return record.Val, nil
}

// Put inserts the given value into the key-value store.
func (d *Database) Put(key []byte, value []byte) error {
	d.wmu.Lock()
	defer d.wmu.Unlock()

	err := d.db.Write(defaultTable, hex.EncodeToString(key), encodeRecord(key, value))
	if err == nil {
		d.pending.Add(1)
	}
	return err
}

// Delete removes the key from the key-value store.
func (d *Database) Delete(key []byte) error {
	d.wmu.Lock()
	defer d.wmu.Unlock()

	err := d.db.Delete(defaultTable, hex.EncodeToString(key))
	if err == nil {
		d.pending.Add(1)
	}
	return err
}

// writeRecords applies encoded records through the non-transactional write API,
// which appends every record to the WAL before touching the in-memory tables.
// The caller must hold wmu, the order of the records is the order of the WAL.
func (d *Database) writeRecords(records map[string]any) error {
	if len(records) == 0 {
		return nil
	}
	capacity := len(records)
	if capacity > maxWriteBatch {
		capacity = maxWriteBatch
	}
	chunk := make(map[string]any, capacity)
	for key, value := range records {
		chunk[key] = value
		if len(chunk) < maxWriteBatch {
			continue
		}
		if err := d.db.WriteBatch(defaultTable, chunk); err != nil {
			return err
		}
		chunk = make(map[string]any, maxWriteBatch)
	}
	if len(chunk) > 0 {
		if err := d.db.WriteBatch(defaultTable, chunk); err != nil {
			return err
		}
	}
	d.pending.Add(1)
	return nil
}

// DeleteRange deletes all of the keys (and values) in the range [start,end)
// (inclusive on start, exclusive on end).
func (d *Database) DeleteRange(start, end []byte) error {
	it := d.NewIterator(nil, start)
	defer it.Release()

	for it.Next() && (end == nil || bytes.Compare(it.Key(), end) < 0) {
		if err := d.Delete(it.Key()); err != nil {
			return err
		}
	}
	return nil
}

// NewBatch creates a write-only key-value store that buffers changes.
//
// The batch is committed through the non-transactional write API. Transactions
// are avoided on purpose: PolarysDB never logs a commit of an explicit
// transaction to its WAL, so such a write is only as durable as the next full
// dump of the state file, and every commit also deep copies the whole database
// as its snapshot. The plain write calls are appended to the WAL, which is
// replayed on startup, and that is what keeps a block on disk even when the
// dump never happens before the process dies.
func (d *Database) NewBatch() ethdb.Batch {
	return &batch{db: d}
}

// NewBatchWithSize creates a write-only database batch with pre-allocated buffer.
func (d *Database) NewBatchWithSize(size int) ethdb.Batch {
	return d.NewBatch()
}

// Stat returns internal metrics.
func (d *Database) Stat() (string, error) {
	status := d.db.GetStatus()
	return fmt.Sprintf("%+v", status), nil
}

// Compact flattens the underlying data store (No-op for PolarysDB).
func (d *Database) Compact(start []byte, limit []byte) error {
	return nil
}

// Path returns the path to the database directory.
func (d *Database) Path() string {
	return d.path
}

// SyncKeyValue flushes all pending writes to disk.
func (d *Database) SyncKeyValue() error {
	return d.db.Flush()
}

// batchOp is a single buffered mutation. The operations are retained so that a
// rejected commit can be replayed onto a fresh transaction.
type batchOp struct {
	key   []byte
	value []byte
	del   bool
}

// batch is a write-only batch that commits changes to its host database.
type batch struct {
	db   *Database
	ops  []batchOp
	size int
}

func (b *batch) Put(key, value []byte) error {
	b.ops = append(b.ops, batchOp{key: key, value: value})
	b.size += len(key) + len(value)
	return nil
}

func (b *batch) Delete(key []byte) error {
	b.ops = append(b.ops, batchOp{key: key, del: true})
	b.size += len(key)
	return nil
}

func (b *batch) DeleteRange(start, end []byte) error {
	// Simple implementation: find keys and delete them
	// In a real implementation we might want a more efficient way
	it := b.db.NewIterator(nil, start)
	defer it.Release()

	for it.Next() && (end == nil || bytes.Compare(it.Key(), end) < 0) {
		if err := b.Delete(it.Key()); err != nil {
			return err
		}
	}
	return nil
}

func (b *batch) ValueSize() int {
	return b.size
}

// Write commits all buffered operations.
//
// Everything is handed to the WAL-backed write API, so the batch is durable as
// soon as Write returns instead of waiting for the backing store to dump its
// in-memory tables to the state file. Deletions have no batched counterpart in
// the backing store and are recorded as tombstones within the same call.
func (b *batch) Write() error {
	if len(b.ops) == 0 {
		return nil
	}
	b.db.wmu.Lock()
	defer b.db.wmu.Unlock()

	records := make(map[string]any, len(b.ops))
	for _, op := range b.ops {
		key := hex.EncodeToString(op.key)
		if op.del {
			records[key] = encodeTombstone(op.key)
			continue
		}
		records[key] = encodeRecord(op.key, op.value)
	}
	// A key put and deleted by the same batch collapses into a tombstone, which
	// is the same result as applying the operations in order.
	return b.db.writeRecords(records)
}

func (b *batch) Reset() {
	b.ops = nil
	b.size = 0
}

func (b *batch) Replay(w ethdb.KeyValueWriter) error {
	for _, op := range b.ops {
		if op.del {
			if err := w.Delete(op.key); err != nil {
				return err
			}
			continue
		}
		if err := w.Put(op.key, op.value); err != nil {
			return err
		}
	}
	return nil
}

// Iterator implementation
type polarysIterator struct {
	records []polarysRecord
	index   int
	err     error
}

func (d *Database) NewIterator(prefix []byte, start []byte) ethdb.Iterator {
	var stored []any
	if err := d.db.ReadBatch(defaultTable, &stored); err != nil {
		return &polarysIterator{index: -1, err: err}
	}
	// Seek to the first key matching prefix+start. The concatenation is copied
	// because appending to the caller's prefix would overwrite its spare
	// capacity in place.
	seek := make([]byte, 0, len(prefix)+len(start))
	seek = append(seek, prefix...)
	seek = append(seek, start...)

	// Collect the matching records into a separate slice, the source must not be
	// appended to while it is being ranged over.
	var records []polarysRecord
	for _, raw := range stored {
		record, err := decodeRecord(raw)
		if err != nil {
			continue
		}
		// Deleted keys are kept as tombstones, iteration must not surface them.
		if record.Del {
			continue
		}
		// Filter by prefix and start
		if bytes.HasPrefix(record.Key, prefix) && bytes.Compare(record.Key, seek) >= 0 {
			records = append(records, record)
		}
	}
	// Sort records by key
	sort.Slice(records, func(i, j int) bool {
		return bytes.Compare(records[i].Key, records[j].Key) < 0
	})

	return &polarysIterator{
		records: records,
		index:   -1,
	}
}

func (it *polarysIterator) Next() bool {
	it.index++
	return it.index < len(it.records)
}

func (it *polarysIterator) Error() error {
	return it.err
}

func (it *polarysIterator) Key() []byte {
	if it.index < 0 || it.index >= len(it.records) {
		return nil
	}
	return it.records[it.index].Key
}

func (it *polarysIterator) Value() []byte {
	if it.index < 0 || it.index >= len(it.records) {
		return nil
	}
	return it.records[it.index].Val
}

func (it *polarysIterator) Release() {
	it.records = nil
}
