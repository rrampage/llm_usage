package main

import (
	"database/sql"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"
)

// openSQLiteReadOnly opens a harness-owned SQLite database without allowing
// llm_usage to mutate it. modernc.org/sqlite is a pure-Go SQLite port, so this
// remains CGO-free while delegating WAL, rollback-journal, encoding, schema and
// locking semantics to SQLite itself rather than the experimental mini parser.
func openSQLiteReadOnly(path string) (*sql.DB, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	u := &url.URL{Scheme: "file", Path: filepath.ToSlash(abs)}
	q := u.Query()
	q.Set("mode", "ro")
	q.Set("_query_only", "true")
	q.Set("_defensive", "true")
	q.Set("_busy_timeout", "5000")
	u.RawQuery = q.Encode()

	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

func sqliteTableExists(db *sql.DB, table string) (bool, error) {
	var one int
	err := db.QueryRow(`SELECT 1 FROM sqlite_schema WHERE type='table' AND name=? LIMIT 1`, table).Scan(&one)
	if err == sql.ErrNoRows {
		return false, nil
	}
	return err == nil, err
}

func sqliteTableColumns(db *sql.DB, table string) (map[string]bool, error) {
	rows, err := db.Query("PRAGMA table_info(" + quoteSQLiteIdent(table) + ")")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var cid int
		var name, typ string
		var notnull, pk int
		var dflt any
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			return nil, err
		}
		out[strings.ToLower(name)] = true
	}
	return out, rows.Err()
}

func sqliteHasColumns(columns map[string]bool, required ...string) bool {
	for _, name := range required {
		if !columns[strings.ToLower(name)] {
			return false
		}
	}
	return true
}

func quoteSQLiteIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

func sqliteQueryError(path, table string, err error) error {
	return fmt.Errorf("sqlite %s table %s: %w", path, table, err)
}
