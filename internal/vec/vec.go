//go:build cgo

// Package vec statically links sqlite-vec (vector search for SQLite) into
// the SQLite provided by mattn/go-sqlite3.
package vec

/*
#cgo CFLAGS: -DSQLITE_CORE -DSQLITE_VEC_STATIC -I${SRCDIR}/../fts/simple/include -O2 -Wno-unused-variable -Wno-unused-function
#cgo arm64 CFLAGS: -DSQLITE_VEC_ENABLE_NEON
#include "sqlite3.h"
#include "sqlite-vec.h"

static int memspace_register_vec(void) {
	return sqlite3_auto_extension((void (*)(void))sqlite3_vec_init);
}
*/
import "C"

import (
	"fmt"
	"sync"

	// Provides the SQLite symbols sqlite-vec links against.
	_ "github.com/mattn/go-sqlite3"
)

var (
	once    sync.Once
	initErr error
)

// Init registers sqlite-vec for every SQLite connection opened afterwards.
func Init() error {
	once.Do(func() {
		if rc := C.memspace_register_vec(); rc != 0 {
			initErr = fmt.Errorf("vec: sqlite3_auto_extension failed (rc=%d)", int(rc))
		}
	})
	return initErr
}
