// Bolt piece completion support was removed upstream (dead-code cleanup); this
// is now the sole fallback whenever sqlite's cgo-backed completion isn't
// usable — regardless of the now-vestigial noboltdb/wasm build tags.
//go:build !cgo || nosqlite
// +build !cgo nosqlite

package storage

import (
	"errors"
)

func NewDefaultPieceCompletionForDir(dir string) (PieceCompletion, error) {
	return nil, errors.New("y ur OS no have features")
}
