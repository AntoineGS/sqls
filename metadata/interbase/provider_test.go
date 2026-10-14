package ibmeta_test

import (
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
