//go:build !interbase || !cgo || !linux || !amd64

package database

import (
	"strings"
	"testing"

	"github.com/sqls-server/sqls/dialect"
)

func TestInterBaseStubReportsNativeBuildRequirements(t *testing.T) {
	_, err := Open(&DBConfig{Driver: dialect.DatabaseDriverInterBase})
	if err == nil {
		t.Fatal("Open() returned nil error without the native InterBase adapter")
	}
	message := strings.ToLower(err.Error())
	if !strings.Contains(message, "interbase") || !strings.Contains(message, "build") {
		t.Fatalf("Open() error = %q, want a clear native-build explanation", err)
	}
}
