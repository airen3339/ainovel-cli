package store

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

func TestEnsureSchemaIsIdempotentAndRejectsForeignVersions(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "ainovel.db")

	first, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("open fresh database: %v", err)
	}
	var version int
	if err := first.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil || version != schemaVersion {
		t.Fatalf("fresh schema version = %d, %v; want %d", version, err, schemaVersion)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("close fresh database: %v", err)
	}

	second, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("reopen at current version must succeed: %v", err)
	}
	if err := second.Close(); err != nil {
		t.Fatalf("close database: %v", err)
	}

	// 报错要让用户知道是哪个库、怎样处理：旧库改名重开，新库升级程序。
	for _, tc := range []struct {
		version int
		way     string
	}{{schemaVersion - 1, "改名备份"}, {schemaVersion + 1, "升级程序"}} {
		markSchemaVersion(t, path, tc.version)
		third, err := Open(ctx, path)
		if err == nil {
			third.Close()
			t.Fatalf("database at schema version %d must be rejected, not migrated", tc.version)
		}
		if !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), tc.way) {
			t.Fatalf("schema version %d error lacks the path or %q: %v", tc.version, tc.way, err)
		}
	}
}

func markSchemaVersion(t *testing.T, path string, version int) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(fmt.Sprintf("PRAGMA user_version = %d", version)); err != nil {
		t.Fatalf("mark schema version %d: %v", version, err)
	}
}
