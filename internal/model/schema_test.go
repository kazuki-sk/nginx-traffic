package model

import (
	"bytes"
	"flag"
	"os"
	"testing"
)

var update = flag.Bool("update", false, "rewrite schema/map.schema.json")

// The published schema must equal the one derived from the Go types.
func TestSchemaUpToDate(t *testing.T) {
	got := Schema()
	path := "../../schema/map.schema.json"
	if *update {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(want, got) {
		t.Fatal("schema/map.schema.json is stale; run go test ./internal/model -update")
	}
}
