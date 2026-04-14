// Copyright 2026 Ella Networks
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package sqlite3

/*
#cgo CFLAGS: -DSQLITE_ENABLE_SESSION -DSQLITE_ENABLE_PREUPDATE_HOOK
#cgo LDFLAGS: -lm

#ifndef USE_LIBSQLITE3
#include "sqlite3-binding.h"
#else
#include <sqlite3.h>
#endif
#include <stdlib.h>
#include <string.h>

// ellaChangesetAbort is a conflict handler that rolls back the whole apply
// on any conflict. The caller-visible guarantee we need for leader-symmetric
// replication is that either the full changeset lands or nothing does.
// Cannot be static: Go takes its address via C.ellaChangesetAbort, which
// requires external linkage so the cgo-generated objects can link against it.
int ellaChangesetAbort(void *pCtx, int eConflict, sqlite3_changeset_iter *pIter) {
    (void)pIter;
    if (pCtx != NULL) {
        *(int*)pCtx = eConflict;
    }
    return 2; // SQLITE_CHANGESET_ABORT
}
*/
import "C"

import (
	"context"
	"fmt"
	"unsafe"
)

// Session wraps a sqlite3_session handle bound to a single SQLiteConn.
//
// A Session records changes made on its underlying connection while enabled.
// Call AttachTable for each table whose mutations should be captured (or
// once with the empty string to capture all tables), run the mutations,
// then call Changeset to obtain the serialized changeset.
//
// The Session must be Closed before the owning connection is closed.
type Session struct {
	conn *SQLiteConn
	s    *C.sqlite3_session
}

// CreateSession creates a new session object on schema (typically "main").
// The returned Session is enabled by default.
func (c *SQLiteConn) CreateSession(schema string) (*Session, error) {
	if schema == "" {
		schema = "main"
	}

	cSchema := C.CString(schema)
	defer C.free(unsafe.Pointer(cSchema))

	var s *C.sqlite3_session

	rv := C.sqlite3session_create(c.db, cSchema, &s)
	if rv != C.SQLITE_OK {
		return nil, c.lastError()
	}

	return &Session{conn: c, s: s}, nil
}

// AttachTable attaches a table to the session. Pass "" to capture all
// tables with PRIMARY KEYs. It is safe to attach a table that does not
// yet exist; the session will pick it up when it is created.
func (s *Session) AttachTable(table string) error {
	var cTab *C.char

	if table != "" {
		cTab = C.CString(table)
		defer C.free(unsafe.Pointer(cTab))
	}

	rv := C.sqlite3session_attach(s.s, cTab)
	if rv != C.SQLITE_OK {
		return s.conn.lastError()
	}

	return nil
}

// Enable turns recording on or off. A newly created session is enabled.
func (s *Session) Enable(enable bool) {
	v := C.int(0)
	if enable {
		v = 1
	}

	C.sqlite3session_enable(s.s, v)
}

// IsEmpty reports whether the session has recorded any changes.
func (s *Session) IsEmpty() bool {
	return C.sqlite3session_isempty(s.s) != 0
}

// Changeset serializes the recorded changes into a byte slice.
// Returns a zero-length slice (not nil) if no changes were recorded.
func (s *Session) Changeset() ([]byte, error) {
	var (
		n   C.int
		buf unsafe.Pointer
	)

	rv := C.sqlite3session_changeset(s.s, &n, &buf)
	if rv != C.SQLITE_OK {
		return nil, s.conn.lastError()
	}

	if buf == nil || n == 0 {
		if buf != nil {
			C.sqlite3_free(buf)
		}

		return []byte{}, nil
	}

	out := C.GoBytes(buf, n)
	C.sqlite3_free(buf)

	return out, nil
}

// Close releases the underlying sqlite3_session. It is safe to call more
// than once.
func (s *Session) Close() {
	if s.s == nil {
		return
	}

	C.sqlite3session_delete(s.s)
	s.s = nil
}

// CaptureChangeset creates a session on the connection, attaches the given
// tables (or all tables if tables is empty or nil), invokes fn, and returns
// the serialized changeset recorded while fn ran. If fn returns an error,
// the session is discarded and that error is returned.
//
// Transaction boundaries are the caller's responsibility. CaptureChangeset
// only records what fn mutates through this connection; it does not begin,
// commit, or roll back. The typical leader-side pattern is:
//
//	conn.CaptureChangeset(ctx, func() error {
//	    // BEGIN
//	    // mutations via the same connection
//	    // (do not COMMIT — roll back after capture)
//	    return nil
//	}, []string{"subscribers", "policies"})
//
// and then have the caller ROLLBACK once the bytes are in hand.
func (c *SQLiteConn) CaptureChangeset(
	ctx context.Context,
	fn func() error,
	tables []string,
) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	sess, err := c.CreateSession("main")
	if err != nil {
		return nil, fmt.Errorf("create session: %w", err)
	}

	defer sess.Close()

	if len(tables) == 0 {
		if err := sess.AttachTable(""); err != nil {
			return nil, fmt.Errorf("attach all tables: %w", err)
		}
	} else {
		for _, t := range tables {
			if err := sess.AttachTable(t); err != nil {
				return nil, fmt.Errorf("attach table %q: %w", t, err)
			}
		}
	}

	if err := fn(); err != nil {
		return nil, err
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	return sess.Changeset()
}

// ChangesetConflictError is returned from ApplyChangeset when the target
// database rejected the changeset. Code is one of the SQLITE_CHANGESET_*
// constants reported by the conflict handler.
type ChangesetConflictError struct {
	Code int
}

func (e *ChangesetConflictError) Error() string {
	switch e.Code {
	case 1:
		return "sqlite3changeset_apply: conflict (DATA)"
	case 2:
		return "sqlite3changeset_apply: conflict (NOTFOUND)"
	case 3:
		return "sqlite3changeset_apply: conflict (CONFLICT)"
	case 4:
		return "sqlite3changeset_apply: conflict (CONSTRAINT)"
	case 5:
		return "sqlite3changeset_apply: conflict (FOREIGN_KEY)"
	default:
		return fmt.Sprintf("sqlite3changeset_apply: conflict (code %d)", e.Code)
	}
}

// ApplyChangeset applies the given changeset to the "main" database of
// this connection. Phase 1 policy: any conflict aborts the whole apply
// (the session module wraps the call in a SAVEPOINT, so a partial apply
// cannot leak). Callers must treat an error as fatal for their higher
// level protocol — on replicated nodes, a conflict means the follower
// has diverged from the leader and should panic or restore from snapshot.
func (c *SQLiteConn) ApplyChangeset(ctx context.Context, changeset []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	if len(changeset) == 0 {
		return nil
	}

	var conflictCode C.int

	// Take address of first byte; sqlite3changeset_apply treats (0, nil)
	// as an empty changeset, but we already handled that above.
	rv := C.sqlite3changeset_apply(
		c.db,
		C.int(len(changeset)),
		unsafe.Pointer(&changeset[0]),
		nil, // xFilter: nil means accept all tables
		(*[0]byte)(unsafe.Pointer(C.ellaChangesetAbort)),
		unsafe.Pointer(&conflictCode),
	)
	if rv != C.SQLITE_OK {
		if conflictCode != 0 {
			return &ChangesetConflictError{Code: int(conflictCode)}
		}

		return c.lastError()
	}

	return nil
}

