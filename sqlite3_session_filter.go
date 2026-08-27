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
