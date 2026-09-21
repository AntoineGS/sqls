//go:build !interbase || !cgo || !linux || !amd64

package database

// ClassifyFailure never recognises a driver cancellation outcome without the
// native build: the driver's error types are not linked in.
func ClassifyFailure(error) (FailureKind, string) {
	return FailureNone, ""
}
