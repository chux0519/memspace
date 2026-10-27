//go:build !cgo

package vec

import "errors"

// Init fails without cgo; client-only builds do not run the server.
func Init() error {
	return errors.New("this memspace build has no server support (built with CGO_ENABLED=0)")
}
