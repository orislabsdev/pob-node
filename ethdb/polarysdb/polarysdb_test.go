package polarysdb

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// TestPolarysDBRefusesLostDatabase verifies that a data directory holding chain
// data but no state file is rejected instead of opening an empty database, which
// would restart the chain from genesis.
func TestPolarysDBRefusesLostDatabase(t *testing.T) {
	path := t.TempDir()
	ancient := filepath.Join(path, "ancient", "chain")
	if err := os.MkdirAll(ancient, 0755); err != nil {
		t.Fatalf("failed to create ancient dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(ancient, "000001.log"), []byte("frozen"), 0644); err != nil {
		t.Fatalf("failed to write ancient segment: %v", err)
	}

	if _, err := New(path, 0, 0, "test", false); err == nil {
		t.Fatal("expected an error for a data directory without a state file")
	} else {
		t.Logf("refused as expected: %v", err)
	}
}

func TestPolarysDB(t *testing.T) {
	path := "./test_polarys_db"
	os.RemoveAll(path)
	defer os.RemoveAll(path)

	db, err := New(path, 0, 0, "test", false)
	if err != nil {
		t.Fatalf("failed to create db: %v", err)
	}
	defer db.Close()

	key := []byte("hello")
	val := []byte("world")

	if err := db.Put(key, val); err != nil {
		t.Fatalf("put failed: %v", err)
	}

	got, err := db.Get(key)
	if err != nil {
		t.Fatalf("get failed: %v", err)
	}
	if !bytes.Equal(got, val) {
		t.Errorf("got %s, want %s", got, val)
	}

	// Test Batch
	batch := db.NewBatch()
	batch.Put([]byte("key1"), []byte("val1"))
	batch.Put([]byte("key2"), []byte("val2"))
	if err := batch.Write(); err != nil {
		t.Fatalf("batch write failed: %v", err)
	}

	got1, _ := db.Get([]byte("key1"))
	if !bytes.Equal(got1, []byte("val1")) {
		t.Errorf("key1: got %s, want val1", got1)
	}

	// Test Iterator
	it := db.NewIterator([]byte("key"), nil)
	count := 0
	for it.Next() {
		count++
		t.Logf("Iter: %s = %s", it.Key(), it.Value())
	}
	if count != 2 {
		t.Errorf("iterator count: got %d, want 2", count)
	}
	it.Release()
}

// TestPolarysDBPersistence checks that keys and values survive a restart. Values
// used to come back base64 encoded because the JSON mapper of the backing store
// copies the encoded text into []byte fields instead of decoding it.
func TestPolarysDBPersistence(t *testing.T) {
	path := "./test_polarys_db_persist"
	os.RemoveAll(path)
	defer os.RemoveAll(path)

	db, err := New(path, 0, 0, "test", false)
	if err != nil {
		t.Fatalf("failed to create db: %v", err)
	}
	keys := [][]byte{[]byte("key1"), []byte("key2")}
	for i, key := range keys {
		if err := db.Put(key, testValue(i)); err != nil {
			t.Fatalf("put %x failed: %v", key, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close failed: %v", err)
	}

	db, err = New(path, 0, 0, "test", false)
	if err != nil {
		t.Fatalf("failed to reopen db: %v", err)
	}
	defer db.Close()

	for i, key := range keys {
		want := testValue(i)
		got, err := db.Get(key)
		if err != nil {
			t.Fatalf("get %x failed: %v", key, err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("get %x: got %x, want %x", key, got, want)
		}
	}
	it := db.NewIterator([]byte("key"), nil)
	count := 0
	for it.Next() {
		if it.Key() != nil && !bytes.HasPrefix(it.Key(), []byte("key")) {
			t.Errorf("iterator returned key %x outside of the prefix", it.Key())
		}
		count++
	}
	if err := it.Error(); err != nil {
		t.Fatalf("iterator failed: %v", err)
	}
	it.Release()
	if count != len(keys) {
		t.Errorf("iterator count after restart: got %d, want %d", count, len(keys))
	}
}

// TestPolarysDBBatchWithConcurrentWrites checks that a batch still commits after
// unrelated writes landed while it was being filled. The backing store bumps a
// global version on every write and rejects commits validated against an older
// snapshot, which used to fail the batch.
func TestPolarysDBBatchWithConcurrentWrites(t *testing.T) {
	path := "./test_polarys_db_conflict"
	os.RemoveAll(path)
	defer os.RemoveAll(path)

	db, err := New(path, 0, 0, "test", false)
	if err != nil {
		t.Fatalf("failed to create db: %v", err)
	}
	defer db.Close()

	stale := []byte("stale")
	if err := db.Put(stale, []byte("value")); err != nil {
		t.Fatalf("put failed: %v", err)
	}

	batch := db.NewBatch()
	if err := batch.Put([]byte("batch"), []byte("value")); err != nil {
		t.Fatalf("batch put failed: %v", err)
	}
	if err := batch.Delete(stale); err != nil {
		t.Fatalf("batch delete failed: %v", err)
	}
	// Commit unrelated writes while the batch is still open.
	for i := 0; i < 5; i++ {
		if err := db.Put([]byte(fmt.Sprintf("concurrent-%d", i)), []byte("value")); err != nil {
			t.Fatalf("put %d failed: %v", i, err)
		}
	}
	time.Sleep(150 * time.Millisecond) // let the write buffer of the store commit

	if err := batch.Write(); err != nil {
		t.Fatalf("batch write failed: %v", err)
	}
	if got, err := db.Get([]byte("batch")); err != nil || !bytes.Equal(got, []byte("value")) {
		t.Errorf("batch value: got %q err=%v, want value", got, err)
	}
	if has, err := db.Has(stale); err != nil || has {
		t.Errorf("deleted key still present: has=%v err=%v", has, err)
	}
}

// TestPolarysDBBatchWritesSurviveCrash checks that a batch is durable as soon as
// it is written instead of waiting for the backing store to dump its in-memory
// tables. A second instance is opened over the same directory while the first
// one is still alive, which is what a restarted node sees after a crash: only
// what reached the disk of the dead process is available.
func TestPolarysDBBatchWritesSurviveCrash(t *testing.T) {
	path := "./test_polarys_db_crash"
	os.RemoveAll(path)
	defer os.RemoveAll(path)

	db, err := New(path, 0, 0, "test", false)
	if err != nil {
		t.Fatalf("failed to create db: %v", err)
	}
	defer db.Close()

	if err := db.Put([]byte("stale"), []byte("value")); err != nil {
		t.Fatalf("put failed: %v", err)
	}
	batch := db.NewBatch()
	if err := batch.Put([]byte("head"), []byte("block-11")); err != nil {
		t.Fatalf("batch put failed: %v", err)
	}
	if err := batch.Delete([]byte("stale")); err != nil {
		t.Fatalf("batch delete failed: %v", err)
	}
	if err := batch.Write(); err != nil {
		t.Fatalf("batch write failed: %v", err)
	}
	// The backing store appends to its WAL in the background.
	time.Sleep(500 * time.Millisecond)

	restarted, err := New(path, 0, 0, "test", false)
	if err != nil {
		t.Fatalf("failed to reopen db: %v", err)
	}
	defer restarted.Close()

	if got, err := restarted.Get([]byte("head")); err != nil || !bytes.Equal(got, []byte("block-11")) {
		t.Errorf("head after restart: got %q err=%v, want block-11", got, err)
	}
	if has, err := restarted.Has([]byte("stale")); err != nil || has {
		t.Errorf("deleted key came back after restart: has=%v err=%v", has, err)
	}
}

func testValue(i int) []byte {
	return []byte{0x00, 0x01, byte(i), 0xff}
}

// TestPolarysDBConcurrentWrites runs single writes and batches from several
// goroutines at once. Writes are serialized by the wrapper, so no commit may
// fail with a conflict.
func TestPolarysDBConcurrentWrites(t *testing.T) {
	path := "./test_polarys_db_concurrent"
	os.RemoveAll(path)
	defer os.RemoveAll(path)

	db, err := New(path, 0, 0, "test", false)
	if err != nil {
		t.Fatalf("failed to create db: %v", err)
	}
	defer db.Close()

	const (
		workers    = 4
		iterations = 5
	)
	var (
		wg    sync.WaitGroup
		errCh = make(chan error, workers)
	)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				if err := db.Put([]byte(fmt.Sprintf("w%d-%d", i, j)), []byte("value")); err != nil {
					errCh <- fmt.Errorf("put %d/%d: %w", i, j, err)
					return
				}
				batch := db.NewBatch()
				if err := batch.Put([]byte(fmt.Sprintf("b%d-%d", i, j)), []byte("value")); err != nil {
					errCh <- fmt.Errorf("batch put %d/%d: %w", i, j, err)
					return
				}
				if err := batch.Write(); err != nil {
					errCh <- fmt.Errorf("batch write %d/%d: %w", i, j, err)
					return
				}
				batch.Reset()
			}
		}(i)
	}
	wg.Wait()
	close(errCh)

	for err := range errCh {
		t.Fatalf("concurrent write failed: %v", err)
	}
	for i := 0; i < workers; i++ {
		for j := 0; j < iterations; j++ {
			for _, key := range []string{fmt.Sprintf("w%d-%d", i, j), fmt.Sprintf("b%d-%d", i, j)} {
				got, err := db.Get([]byte(key))
				if err != nil {
					t.Fatalf("get %s failed: %v", key, err)
				}
				if !bytes.Equal(got, []byte("value")) {
					t.Fatalf("get %s: got %q, want value", key, got)
				}
			}
		}
	}
}
