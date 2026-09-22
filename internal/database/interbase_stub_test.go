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

func TestExplainRepositoryAbsentWithoutNativeBuild(t *testing.T) {
	// A query plan needs the native client, so on an untagged build the
	// capability is absent rather than stubbed: a capability interface must
	// not be implemented by a method that always fails. Sub-project 3's code
	// action simply does not offer.
	repository := DBRepository(&InterBaseDBRepository{})
	if _, ok := repository.(ExplainRepository); ok {
		t.Error("*InterBaseDBRepository must not implement ExplainRepository without the interbase build tag")
	}
	// The untagged capabilities are still present.
	if _, ok := repository.(CatalogRepository); !ok {
		t.Error("*InterBaseDBRepository must implement CatalogRepository on every build")
	}
	if _, ok := repository.(DDLRepository); !ok {
		t.Error("*InterBaseDBRepository must implement DDLRepository on every build")
	}
}
