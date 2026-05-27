package zulipbot_test

import (
	"context"
	"database/sql"
	"testing"

	storagedb "github.com/tum-zulip/go-campusbot/internal/zulipbot/storage/db"
)

func openZulipbotTestStorage(t *testing.T, path string) (*sql.DB, *storagedb.Queries) {
	t.Helper()
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	db.SetMaxOpenConns(1)
	if err := storagedb.ConfigureSQLite(context.Background(), db); err != nil {
		_ = db.Close()
		t.Fatalf("ConfigureSQLite: %v", err)
	}
	if err := storagedb.InitSchema(context.Background(), db); err != nil {
		_ = db.Close()
		t.Fatalf("InitSchema: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, storagedb.New(db)
}
