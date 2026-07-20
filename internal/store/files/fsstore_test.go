package files

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A managed upload and an agent-written plain file of the same display name must
// list once (the managed record wins), not twice.
func TestListDedupesManagedAndPlainSameName(t *testing.T) {
	dir := t.TempDir()
	s, err := NewFSStore(dir)
	if err != nil {
		t.Fatalf("NewFSStore: %v", err)
	}
	ctx := context.Background()

	// API-managed upload: key uuid-1, display name "data.csv".
	if err := s.WriteMeta(ctx, FileMeta{Key: "uuid-1", Name: "data.csv", ContentType: "text/csv"}); err != nil {
		t.Fatalf("WriteMeta: %v", err)
	}
	if _, err := s.WriteBlob(ctx, "uuid-1", strings.NewReader("a,b\n1,2\n")); err != nil {
		t.Fatalf("WriteBlob: %v", err)
	}
	// An agent wrote a plain file of the SAME display name alongside the upload.
	if err := os.WriteFile(filepath.Join(dir, "data.csv"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write plain dup: %v", err)
	}
	// And a genuinely distinct agent output.
	if err := os.WriteFile(filepath.Join(dir, "processed-data.csv.txt"), []byte("y"), 0o644); err != nil {
		t.Fatalf("write plain output: %v", err)
	}

	list, err := s.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	byName := map[string]int{}
	var dataKey string
	for _, m := range list {
		byName[m.Name]++
		if m.Name == "data.csv" {
			dataKey = m.Key
		}
	}
	if byName["data.csv"] != 1 {
		t.Fatalf("expected data.csv listed once, got %d (list=%+v)", byName["data.csv"], list)
	}
	if dataKey != "uuid-1" {
		t.Fatalf("expected managed record to win (key uuid-1), got key %q", dataKey)
	}
	if byName["processed-data.csv.txt"] != 1 {
		t.Fatalf("expected distinct plain file listed once, got %d", byName["processed-data.csv.txt"])
	}
}
