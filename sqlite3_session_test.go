// Copyright 2026 Ella Networks
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

//go:build cgo && sqlite_session
// +build cgo,sqlite_session

package sqlite3

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"
)

// newSessionDriver registers a uniquely named driver that captures every
// *SQLiteConn produced by ConnectHook. Callers use the returned slice to
// reach into the raw connection for CreateSession / ApplyChangeset.
func newSessionDriver(t *testing.T) (string, *[]*SQLiteConn) {
	t.Helper()

	conns := &[]*SQLiteConn{}
	name := fmt.Sprintf("sqlite3_session_%d_%d", time.Now().UnixNano(), len(*conns))

	sql.Register(name, &SQLiteDriver{
		ConnectHook: func(c *SQLiteConn) error {
			*conns = append(*conns, c)
			return nil
		},
	})

	return name, conns
}

func openDB(t *testing.T, driver string) (*sql.DB, string) {
	t.Helper()

	path := TempFilename(t)
	db, err := sql.Open(driver, path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}

	if err := db.Ping(); err != nil {
		t.Fatalf("ping %s: %v", path, err)
	}

	return db, path
}

func mustExec(t *testing.T, db *sql.DB, q string, args ...any) {
	t.Helper()

	if _, err := db.Exec(q, args...); err != nil {
		t.Fatalf("exec %q: %v", q, err)
	}
}

// TestSessionRoundTrip captures INSERT/UPDATE/DELETE on a source DB, applies
// the changeset to a fresh destination DB with the same schema, and verifies
// both databases converge to the same content.
func TestSessionRoundTrip(t *testing.T) {
	driver, conns := newSessionDriver(t)

	src, srcPath := openDB(t, driver)
	defer os.Remove(srcPath)
	defer src.Close()

	dst, dstPath := openDB(t, driver)
	defer os.Remove(dstPath)
	defer dst.Close()

	const schema = `
		CREATE TABLE items (
			id   INTEGER PRIMARY KEY,
			name TEXT    NOT NULL,
			qty  INTEGER NOT NULL
		);
	`
	mustExec(t, src, schema)
	mustExec(t, dst, schema)

	// Seed some baseline rows on both sides so UPDATE/DELETE changes have
	// a matching target row on the destination.
	for _, q := range []string{
		"INSERT INTO items (id, name, qty) VALUES (1, 'apple', 3)",
		"INSERT INTO items (id, name, qty) VALUES (2, 'banana', 7)",
	} {
		mustExec(t, src, q)
		mustExec(t, dst, q)
	}

	if len(*conns) < 2 {
		t.Fatalf("expected at least 2 SQLiteConns from ConnectHook, got %d", len(*conns))
	}

	srcConn := (*conns)[0]
	dstConn := (*conns)[1]

	ctx := context.Background()

	// Capture INSERT + UPDATE + DELETE as one changeset.
	changeset, err := srcConn.CaptureChangeset(ctx, func() error {
		if _, err := src.Exec("INSERT INTO items (id, name, qty) VALUES (3, 'cherry', 12)"); err != nil {
			return err
		}

		if _, err := src.Exec("UPDATE items SET qty = 99 WHERE id = 1"); err != nil {
			return err
		}

		if _, err := src.Exec("DELETE FROM items WHERE id = 2"); err != nil {
			return err
		}

		return nil
	}, []string{"items"})
	if err != nil {
		t.Fatalf("CaptureChangeset: %v", err)
	}

	if len(changeset) == 0 {
		t.Fatal("expected non-empty changeset after mutations")
	}

	if err := dstConn.ApplyChangeset(ctx, changeset); err != nil {
		t.Fatalf("ApplyChangeset: %v", err)
	}

	assertTablesEqual(t, src, dst, "items", "id, name, qty")
}

// TestSessionEmptyChangeset exercises the no-mutation path: CaptureChangeset
// returns a zero-length slice and ApplyChangeset accepts it as a no-op.
func TestSessionEmptyChangeset(t *testing.T) {
	driver, conns := newSessionDriver(t)

	src, srcPath := openDB(t, driver)
	defer os.Remove(srcPath)
	defer src.Close()

	mustExec(t, src, "CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT)")

	srcConn := (*conns)[0]

	changeset, err := srcConn.CaptureChangeset(context.Background(), func() error { return nil }, nil)
	if err != nil {
		t.Fatalf("CaptureChangeset: %v", err)
	}

	if len(changeset) != 0 {
		t.Fatalf("expected empty changeset, got %d bytes", len(changeset))
	}

	if err := srcConn.ApplyChangeset(context.Background(), changeset); err != nil {
		t.Fatalf("ApplyChangeset on empty: %v", err)
	}
}

// TestSessionFnErrorPropagates ensures that when fn returns an error,
// CaptureChangeset surfaces it and does not swallow it.
func TestSessionFnErrorPropagates(t *testing.T) {
	driver, conns := newSessionDriver(t)

	src, srcPath := openDB(t, driver)
	defer os.Remove(srcPath)
	defer src.Close()

	mustExec(t, src, "CREATE TABLE t (id INTEGER PRIMARY KEY)")

	srcConn := (*conns)[0]

	sentinel := errors.New("boom")

	_, err := srcConn.CaptureChangeset(context.Background(), func() error { return sentinel }, []string{"t"})
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected sentinel error, got %v", err)
	}
}

// TestSessionMultipleTables attaches two tables and confirms changes on a
// third (unattached) table are not replicated.
func TestSessionMultipleTables(t *testing.T) {
	driver, conns := newSessionDriver(t)

	src, srcPath := openDB(t, driver)
	defer os.Remove(srcPath)
	defer src.Close()

	dst, dstPath := openDB(t, driver)
	defer os.Remove(dstPath)
	defer dst.Close()

	const schema = `
		CREATE TABLE tracked_a (id INTEGER PRIMARY KEY, v TEXT);
		CREATE TABLE tracked_b (id INTEGER PRIMARY KEY, v TEXT);
		CREATE TABLE ignored  (id INTEGER PRIMARY KEY, v TEXT);
	`
	mustExec(t, src, schema)
	mustExec(t, dst, schema)

	srcConn := (*conns)[0]
	dstConn := (*conns)[1]

	changeset, err := srcConn.CaptureChangeset(context.Background(), func() error {
		if _, err := src.Exec("INSERT INTO tracked_a (id, v) VALUES (1, 'a')"); err != nil {
			return err
		}
		if _, err := src.Exec("INSERT INTO tracked_b (id, v) VALUES (1, 'b')"); err != nil {
			return err
		}
		if _, err := src.Exec("INSERT INTO ignored (id, v) VALUES (1, 'x')"); err != nil {
			return err
		}
		return nil
	}, []string{"tracked_a", "tracked_b"})
	if err != nil {
		t.Fatalf("CaptureChangeset: %v", err)
	}

	if err := dstConn.ApplyChangeset(context.Background(), changeset); err != nil {
		t.Fatalf("ApplyChangeset: %v", err)
	}

	assertTablesEqual(t, src, dst, "tracked_a", "id, v")
	assertTablesEqual(t, src, dst, "tracked_b", "id, v")

	// The "ignored" table must be empty on the destination.
	var n int
	if err := dst.QueryRow("SELECT COUNT(*) FROM ignored").Scan(&n); err != nil {
		t.Fatalf("count ignored: %v", err)
	}
	if n != 0 {
		t.Fatalf("unattached table should not replicate, got %d rows on dst", n)
	}
}

// TestApplyChangesetConflict verifies a surface constraint conflict is
// reported as ChangesetConflictError and the destination remains
// unchanged (thanks to the SAVEPOINT wrapped around the apply).
func TestApplyChangesetConflict(t *testing.T) {
	driver, conns := newSessionDriver(t)

	src, srcPath := openDB(t, driver)
	defer os.Remove(srcPath)
	defer src.Close()

	dst, dstPath := openDB(t, driver)
	defer os.Remove(dstPath)
	defer dst.Close()

	const schema = `CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT NOT NULL);`
	mustExec(t, src, schema)
	mustExec(t, dst, schema)

	// Pre-populate destination with a row that will collide on PK.
	mustExec(t, dst, "INSERT INTO t (id, v) VALUES (1, 'existing')")

	srcConn := (*conns)[0]
	dstConn := (*conns)[1]

	changeset, err := srcConn.CaptureChangeset(context.Background(), func() error {
		_, err := src.Exec("INSERT INTO t (id, v) VALUES (1, 'from-src')")
		return err
	}, []string{"t"})
	if err != nil {
		t.Fatalf("CaptureChangeset: %v", err)
	}

	err = dstConn.ApplyChangeset(context.Background(), changeset)
	if err == nil {
		t.Fatal("expected conflict error, got nil")
	}

	var cerr *ChangesetConflictError
	if !errors.As(err, &cerr) {
		t.Fatalf("expected *ChangesetConflictError, got %T: %v", err, err)
	}

	// Destination row must still hold the pre-existing value.
	var v string
	if err := dst.QueryRow("SELECT v FROM t WHERE id = 1").Scan(&v); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if v != "existing" {
		t.Fatalf("destination row changed despite aborted apply: got %q", v)
	}
}

// assertTablesEqual fetches all rows from both DBs (ordered by the given
// column list) and compares the materialized string representations.
func assertTablesEqual(t *testing.T, a, b *sql.DB, table, cols string) {
	t.Helper()

	aRows := dumpTable(t, a, table, cols)
	bRows := dumpTable(t, b, table, cols)

	if len(aRows) != len(bRows) {
		t.Fatalf("row count mismatch for %s: src=%d dst=%d", table, len(aRows), len(bRows))
	}

	for i := range aRows {
		if aRows[i] != bRows[i] {
			t.Fatalf("row %d mismatch for %s: src=%q dst=%q", i, table, aRows[i], bRows[i])
		}
	}
}

func dumpTable(t *testing.T, db *sql.DB, table, cols string) []string {
	t.Helper()

	rows, err := db.Query(fmt.Sprintf("SELECT %s FROM %s ORDER BY %s", cols, table, cols))
	if err != nil {
		t.Fatalf("query %s: %v", table, err)
	}
	defer rows.Close()

	columns, err := rows.Columns()
	if err != nil {
		t.Fatalf("columns %s: %v", table, err)
	}

	var out []string

	for rows.Next() {
		dest := make([]any, len(columns))
		ptrs := make([]any, len(columns))
		for i := range dest {
			ptrs[i] = &dest[i]
		}

		if err := rows.Scan(ptrs...); err != nil {
			t.Fatalf("scan %s: %v", table, err)
		}

		out = append(out, fmt.Sprintf("%v", dest))
	}

	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err %s: %v", table, err)
	}

	return out
}
