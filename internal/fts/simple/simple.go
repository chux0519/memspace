//go:build cgo

// Package simple statically links the simple FTS5 tokenizer (with cppjieba)
// into SQLite as provided by mattn/go-sqlite3.
//
// Index-side tokenization is per character (plus pinyin), so any Chinese
// substring is findable; jieba_query() segments queries into words, so
// matches are word-precise when the dictionary knows the word and fall back
// to character sequences when it does not.
package simple

/*
#cgo CXXFLAGS: -std=c++17 -O2 -DUSE_JIEBA=1 -DSQLITE_CORE -I${SRCDIR}/include -Wno-deprecated-declarations
#cgo CFLAGS: -I${SRCDIR}/include
#cgo linux LDFLAGS: -lstdc++ -lm
#cgo darwin LDFLAGS: -lc++
#cgo windows LDFLAGS: -lstdc++
#include <stdlib.h>
#include "sqlite3.h"

extern int sqlite3_simple_init(sqlite3 *db, char **pzErrMsg, const void *pApi);
extern void memspace_set_pinyin(const char *data, size_t len);
extern void memspace_set_jieba_dict(const char *dir);

static int memspace_register_simple(void) {
	return sqlite3_auto_extension((void (*)(void))sqlite3_simple_init);
}
*/
import "C"

import (
	"compress/gzip"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"unsafe"

	// Provides the SQLite symbols the tokenizer links against.
	_ "github.com/mattn/go-sqlite3"
)

//go:embed data/*.gz data/user.dict.utf8
var data embed.FS

var (
	once    sync.Once
	initErr error
)

// Available reports whether this build includes the tokenizer.
func Available() bool { return true }

// Init registers the tokenizer for every SQLite connection opened afterwards
// and points jieba at its dictionaries, extracted under cacheDir. It must run
// before the first connection is opened; later calls are no-ops.
func Init(cacheDir string) error {
	once.Do(func() { initErr = initOnce(cacheDir) })
	return initErr
}

func initOnce(cacheDir string) error {
	pinyin, err := gunzip("data/pinyin.txt.gz")
	if err != nil {
		return err
	}
	// Deliberately leaked: C++ keeps the pointer for the process lifetime.
	cp := C.CBytes(pinyin)
	C.memspace_set_pinyin((*C.char)(cp), C.size_t(len(pinyin)))

	dir, err := extractDicts(cacheDir)
	if err != nil {
		return err
	}
	cdir := C.CString(dir + string(os.PathSeparator))
	C.memspace_set_jieba_dict(cdir) // copied into a std::string
	C.free(unsafe.Pointer(cdir))

	if rc := C.memspace_register_simple(); rc != 0 {
		return fmt.Errorf("simple: sqlite3_auto_extension failed (rc=%d)", int(rc))
	}
	return nil
}

func gunzip(name string) ([]byte, error) {
	f, err := data.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		return nil, fmt.Errorf("simple: %s: %w", name, err)
	}
	return io.ReadAll(zr)
}

// extractDicts writes the dictionaries to <cacheDir>/jieba-<hash>, a
// content-addressed directory, so upgrades never mix dictionary versions and
// an existing extraction is reused.
func extractDicts(cacheDir string) (string, error) {
	files := map[string][]byte{}
	for _, name := range []string{"jieba.dict.utf8", "hmm_model.utf8", "idf.utf8", "stop_words.utf8"} {
		b, err := gunzip("data/" + name + ".gz")
		if err != nil {
			return "", err
		}
		files[name] = b
	}
	user, err := data.ReadFile("data/user.dict.utf8")
	if err != nil {
		return "", err
	}
	files["user.dict.utf8"] = user

	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	h := sha256.New()
	for _, n := range names {
		h.Write([]byte(n))
		h.Write(files[n])
	}
	dir := filepath.Join(cacheDir, "jieba-"+hex.EncodeToString(h.Sum(nil))[:16])
	if _, err := os.Stat(filepath.Join(dir, ".complete")); err == nil {
		return dir, nil
	}
	tmp, err := os.MkdirTemp(cacheDir, ".jieba-tmp-")
	if err != nil {
		if err := os.MkdirAll(cacheDir, 0o755); err != nil {
			return "", err
		}
		if tmp, err = os.MkdirTemp(cacheDir, ".jieba-tmp-"); err != nil {
			return "", err
		}
	}
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(tmp, n), files[n], 0o644); err != nil {
			os.RemoveAll(tmp)
			return "", err
		}
	}
	os.WriteFile(filepath.Join(tmp, ".complete"), nil, 0o644)
	if err := os.Rename(tmp, dir); err != nil {
		os.RemoveAll(tmp)
		// Another process extracted the same content first.
		if _, statErr := os.Stat(filepath.Join(dir, ".complete")); statErr == nil {
			return dir, nil
		}
		return "", err
	}
	return dir, nil
}
