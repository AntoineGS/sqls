//go:build !interbase || !cgo || !linux || !amd64

package ibmeta_test

import (
	"context"
	ibmeta "github.com/sqls-server/sqls/metadata/interbase"
	"testing"
)

// Stub acceptance must never dial a placeholder target in a native build.
// Native NONE attachments are qualified with explicit disposable fixtures.
func TestFactoryAcceptsNoneCharsetWithoutRejection(t *testing.T) {
	none := ibmeta.Config{Target: "server:D:\\Data\\APP.GBB", User: "SYSDBA", Charset: "NONE"}
	utf8 := none
	utf8.Charset = "UTF8"
	_, errNone := ibmeta.Factory(none)(context.Background())
	_, errUTF8 := ibmeta.Factory(utf8)(context.Background())
	if errNone == nil || errUTF8 == nil {
		t.Fatal("stub provider unexpectedly available")
	}
	if errNone.Error() != errUTF8.Error() {
		t.Fatalf("NONE specially rejected: %v vs %v", errNone, errUTF8)
	}
}
