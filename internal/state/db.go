package state

import (
	"database/sql"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

// DB wraps the SQLite connection with the driver name used.
type DB struct {
	*sql.DB
}

// Open opens the SQLite database at path, enabling WAL mode and foreign keys.
// It runs schema migrations transactionally before returning.
func Open(path string) (*DB, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err := migrate(db); err != nil {
		db.Close()
		return nil, err
	}
	return &DB{DB: db}, nil
}

// Close closes the underlying database.
func (d *DB) Close() error { return d.DB.Close() }

// Now returns the current time in UTC, used for all timestamp columns.
func Now() time.Time { return time.Now().UTC() }
