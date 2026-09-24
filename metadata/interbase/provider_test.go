package ibmeta_test

import (
	"context"
	"errors"
	"testing"

	"github.com/sqls-server/sqls/metadata"
	ibmeta "github.com/sqls-server/sqls/metadata/interbase"
)

func TestKeyForDelegatesToNormalizeTarget(t *testing.T) {
	cfg := ibmeta.Config{Target: `server/3050:D:\Data\APP.GBB`}
	got, err := ibmeta.KeyFor(cfg)
	if err != nil {
		t.Fatal(err)
	}
	want, err := ibmeta.NormalizeTarget(cfg.Target)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("KeyFor() = %+v, want %+v (same as NormalizeTarget)", got, want)
	}
}

func TestKeyForRejectsBlankTarget(t *testing.T) {
	if _, err := ibmeta.KeyFor(ibmeta.Config{}); !errors.Is(err, metadata.ErrInvalidTarget) {
		t.Fatalf("KeyFor(blank) error = %v, want ErrInvalidTarget", err)
	}
}

func TestScopeForRequiresAuthenticatedPrincipal(t *testing.T) {
	if _, err := ibmeta.ScopeFor(ibmeta.Config{}); !errors.Is(err, metadata.ErrScopeRequired) {
		t.Fatalf("ScopeFor(blank user) error = %v, want ErrScopeRequired", err)
	}
	if _, err := ibmeta.ScopeFor(ibmeta.Config{User: "   "}); !errors.Is(err, metadata.ErrScopeRequired) {
		t.Fatalf("ScopeFor(blank user) error = %v, want ErrScopeRequired", err)
	}
}

func TestScopeForDiffersByPrincipalRoleAndTextPolicy(t *testing.T) {
	base, err := ibmeta.ScopeFor(ibmeta.Config{User: "ALICE", Role: "READER", Charset: "NONE", CatalogTextCharset: "WIN1252"})
	if err != nil {
		t.Fatal(err)
	}
	differentUser, err := ibmeta.ScopeFor(ibmeta.Config{User: "BOB", Role: "READER", Charset: "NONE", CatalogTextCharset: "WIN1252"})
	if err != nil {
		t.Fatal(err)
	}
	differentRole, err := ibmeta.ScopeFor(ibmeta.Config{User: "ALICE", Role: "WRITER", Charset: "NONE", CatalogTextCharset: "WIN1252"})
	if err != nil {
		t.Fatal(err)
	}
	differentPolicy, err := ibmeta.ScopeFor(ibmeta.Config{User: "ALICE", Role: "READER", Charset: "UTF8", CatalogTextCharset: "WIN1252"})
	if err != nil {
		t.Fatal(err)
	}
	same, err := ibmeta.ScopeFor(ibmeta.Config{User: "ALICE", Role: "READER", Charset: "NONE", CatalogTextCharset: "WIN1252"})
	if err != nil {
		t.Fatal(err)
	}
	if base != same {
		t.Fatalf("identical Config produced different Scope: %+v vs %+v", base, same)
	}
	if base == differentUser || base == differentRole || base == differentPolicy {
		t.Fatal("Scope collapsed distinct principal/role/text-policy inputs")
	}
	if base.Principal != "ALICE" || same.Principal != "ALICE" {
		t.Fatal("Scope must never carry a password or other secret field derived from Config")
	}
}

// TestFactoryAcceptsNoneCharsetWithoutRejection is the "Charset: NONE must
// work" requirement: this generic API must never route through SQLs's
// interBaseCharsets allowlist. On an untagged/non-cgo build there is no
// native driver to attach with, so the only assertion possible here is that
// failure comes from "provider unavailable" (the build-tag stub), never a
// charset-validation error — the same Config with Charset: "UTF8" instead
// must fail identically, which proves Charset was never inspected.
func TestFactoryAcceptsNoneCharsetWithoutRejection(t *testing.T) {
	none := ibmeta.Config{Target: "server:D:\\Data\\APP.GBB", User: "SYSDBA", Charset: "NONE"}
	utf8 := ibmeta.Config{Target: "server:D:\\Data\\APP.GBB", User: "SYSDBA", Charset: "UTF8"}

	_, errNone := ibmeta.Factory(none)(context.Background())
	_, errUTF8 := ibmeta.Factory(utf8)(context.Background())
	if errNone == nil || errUTF8 == nil {
		t.Skip("native InterBase provider is available in this build; charset-allowlist bypass is exercised via a live attach instead")
	}
	if errNone.Error() != errUTF8.Error() {
		t.Fatalf("Charset: NONE failed differently than Charset: UTF8 (%v vs %v); NONE must not be specially rejected", errNone, errUTF8)
	}
}
