package main

import (
	"fmt"
	"log"
	"os"

	"github.com/polarysfoundation/polarysdb"
)

func main() {
	key := polarysdb.GenerateKeyFromBytes([]byte("01234567890123456789012345678901"))
	dir := "./test_db"
	os.RemoveAll(dir)
	defer os.RemoveAll(dir)

	db, err := polarysdb.Init(key, dir, true)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	table := "test_table"
	if err := db.Create(table); err != nil {
		log.Fatal(err)
	}

	db.Write(table, "key1", []byte("value1"))
	db.Write(table, "key2", []byte("value2"))

	val, ok := db.Read(table, "key1")
	fmt.Printf("Read key1: %v (ok: %v)\n", val, ok)

	batch, err := db.ReadBatch(table)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("Batch: %+v\n", batch)
}
