//go:build !cgo

package simple

import "errors"

// Available reports whether this build includes the tokenizer.
func Available() bool { return false }

// Init fails: the tokenizer is C++ and needs cgo. Client-only builds
// (CGO_ENABLED=0) cannot run the server.
func Init(string) error {
	return errors.New("this memspace build has no server support (built with CGO_ENABLED=0)")
}
