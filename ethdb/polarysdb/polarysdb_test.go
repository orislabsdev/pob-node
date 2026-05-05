package polarysdb

import (
	"bytes"
	"os"
	"testing"
)

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
