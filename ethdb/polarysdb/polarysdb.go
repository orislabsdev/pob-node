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
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/polarysfoundation/polarysdb"
	"github.com/polarysfoundation/polarysdb/modules/tx"
)

const (
	defaultTable = "pob_chain"
)

var (
	errPolarysNotFound = errors.New("not found")
)

// Database is a persistent key-value store based on PolarysDB.
type Database struct {
	db   *polarysdb.Database
	path string
	mu   sync.RWMutex
}

// New returns a wrapped PolarysDB object.
func New(path string, cache int, handles int, namespace string, readonly bool) (*Database, error) {
	// Use absolute path for stable key derivation regardless of working directory
	absPath, _ := filepath.Abs(path)
	h := sha256.Sum256([]byte(absPath))
	encryptionKey := polarysdb.GenerateKeyFromBytes(h[:])

	cfg := polarysdb.DefaultConfig()
	home, _ := os.UserHomeDir()
	relPath, err := filepath.Rel(home, absPath)
	if err != nil {
		cfg.DirPath = absPath
	} else {
		cfg.DirPath = relPath
	}
	cfg.EncryptionKey = encryptionKey

	// PolarysDB manages WAL and synchronization automatically.
	cfg.EnableTransactions = true
	cfg.EnableIndexes = false
	cfg.EnableCompression = true
	cfg.Debug = true

	db, err := polarysdb.InitWithConfig(cfg)
	if err != nil {
		return nil, err
	}

	// Ensure the default table exists
	if !db.Exist(defaultTable) {
		if err := db.Create(defaultTable); err != nil {
			db.Close()
			return nil, err
		}
	}

	return &Database{
		db:   db,
		path: path,
	}, nil
}

// Close closes the database.
func (d *Database) Close() error {
	return d.db.Close()
}

// polarysRecord is used to store both key and value in PolarysDB,
// since ReadBatch only returns values and we need keys for iteration.
type polarysRecord struct {
	Key []byte `json:"k"`
	Val []byte `json:"v"`
}

// Has retrieves if a key is present in the key-value store.
func (d *Database) Has(key []byte) (bool, error) {
	_, exists := d.db.Read(defaultTable, hex.EncodeToString(key))
	return exists, nil
}

// Get retrieves the given key if it's present in the key-value store.
func (d *Database) Get(key []byte) ([]byte, error) {
	val, exists := d.db.Read(defaultTable, hex.EncodeToString(key))
	if !exists {
		return nil, errPolarysNotFound
	}

	record, err := d.toRecord(val)
	if err != nil {
		return nil, err
	}
	return record.Val, nil
}

// toRecord converts a value from PolarysDB (which might be a map if loaded from disk)
// into a polarysRecord struct.
func (d *Database) toRecord(val any) (polarysRecord, error) {
	if r, ok := val.(polarysRecord); ok {
		return r, nil
	}
	// When loaded from disk, PolarysDB returns map[string]any due to JSON unmarshaling
	var record polarysRecord
	data, err := json.Marshal(val)
	if err != nil {
		return record, err
	}
	if err := json.Unmarshal(data, &record); err != nil {
		return record, err
	}
	return record, nil
}

// Put inserts the given value into the key-value store.
func (d *Database) Put(key []byte, value []byte) error {
	return d.db.Write(defaultTable, hex.EncodeToString(key), polarysRecord{Key: key, Val: value})
}

// Delete removes the key from the key-value store.
func (d *Database) Delete(key []byte) error {
	return d.db.Delete(defaultTable, hex.EncodeToString(key))
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
func (d *Database) NewBatch() ethdb.Batch {
	txn, _ := d.db.BeginTransaction()
	return &batch{
		db:  d,
		txn: txn,
	}
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
	// PolarysDB handles periodic flushing based on SaveInterval.
	// We can't force a manual snapshot flush easily via public API,
	// but WAL is already synced periodically.
	return nil
}

// batch is a write-only batch that commits changes to its host database.
type batch struct {
	db   *Database
	txn  *tx.Transaction
	size int
}

func (b *batch) Put(key, value []byte) error {
	if err := b.txn.Write(defaultTable, hex.EncodeToString(key), polarysRecord{Key: key, Val: value}); err != nil {
		return err
	}
	b.size += len(key) + len(value)
	return nil
}

func (b *batch) Delete(key []byte) error {
	if err := b.txn.Delete(defaultTable, hex.EncodeToString(key)); err != nil {
		return err
	}
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

func (b *batch) Write() error {
	return b.db.db.CommitTransaction(b.txn)
}

func (b *batch) Reset() {
	b.txn, _ = b.db.db.BeginTransaction()
	b.size = 0
}

func (b *batch) Replay(w ethdb.KeyValueWriter) error {
	for table, tableData := range b.txn.Changes {
		if table != defaultTable {
			continue
		}
		for k, v := range tableData {
			if v == nil {
				decodedKey, _ := hex.DecodeString(k)
				if err := w.Delete(decodedKey); err != nil {
					return err
				}
				continue
			}

			// We use d.toRecord even if it's a batch because the transaction might have
			// loaded existing data into Changes during Snapshot creation.
			// Actually, Snapshot is a map[string]map[string]any from db.data.
			// If db.data was loaded from disk, it contains maps.

			// For simplicity, we'll use a temporary Database instance or just the helper logic
			var record polarysRecord
			if r, ok := v.(polarysRecord); ok {
				record = r
			} else {
				data, _ := json.Marshal(v)
				json.Unmarshal(data, &record)
			}

			if err := w.Put(record.Key, record.Val); err != nil {
				return err
			}
		}
	}
	return nil
}

// Iterator implementation
type polarysIterator struct {
	records []polarysRecord
	index   int
}

func (d *Database) NewIterator(prefix []byte, start []byte) ethdb.Iterator {
	all, _ := d.db.ReadBatch(defaultTable)
	var records []polarysRecord
	for _, a := range all {
		record, err := d.toRecord(a)
		if err != nil {
			continue
		}
		// Filter by prefix and start
		if bytes.HasPrefix(record.Key, prefix) && bytes.Compare(record.Key, append(prefix, start...)) >= 0 {
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
	return nil
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
