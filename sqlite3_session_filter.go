// Copyright 2026 Ella Networks
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package sqlite3

/*
#include <stdlib.h>
*/
import "C"

import (
	"sync"
	"unsafe"
)

// This file holds the exported xFilter trampoline. cgo forbids //export in a
// file whose preamble defines C functions, so it cannot live alongside
// ellaChangesetAbort in sqlite3_session.go.

// applyFilters maps the pCtx of an in-flight sqlite3changeset_apply call to
// that call's table filter. sqlite3changeset_apply is synchronous, so an
// entry lives only for the duration of one ApplyChangesetFiltered call.
var (
	applyFilterMu sync.RWMutex
	applyFilters  = make(map[unsafe.Pointer]func(string) bool)
)

func registerApplyFilter(key unsafe.Pointer, filter func(string) bool) {
	applyFilterMu.Lock()
	defer applyFilterMu.Unlock()

	applyFilters[key] = filter
}

func unregisterApplyFilter(key unsafe.Pointer) {
	applyFilterMu.Lock()
	defer applyFilterMu.Unlock()

	delete(applyFilters, key)
}

func lookupApplyFilter(key unsafe.Pointer) func(string) bool {
	applyFilterMu.RLock()
	defer applyFilterMu.RUnlock()

	return applyFilters[key]
}

// ellaChangesetFilterGo is the xFilter callback. SQLite invokes it once per
// table header in the changeset, before it inspects the local schema, and
// skips every change for that table when the return value is zero.
//
// An unregistered pCtx returns 1, matching the accept-all behavior of a nil
// xFilter: a filter that went missing must surface as a conflict rather than
// silently discard changes.
//
//export ellaChangesetFilterGo
func ellaChangesetFilterGo(pCtx unsafe.Pointer, zTab *C.char) C.int {
	filter := lookupApplyFilter(pCtx)
	if filter == nil {
		return 1
	}

	if filter(C.GoString(zTab)) {
		return 1
	}

	return 0
}
