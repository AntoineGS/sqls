package ibmeta_test

import (
	"errors"
	"testing"

	"github.com/sqls-server/sqls/metadata"
	ibmeta "github.com/sqls-server/sqls/metadata/interbase"
)

func TestNormalizeTargetBarePrefixEquivalence(t *testing.T) {
	bare, err := ibmeta.NormalizeTarget(`server/3050:D:\Data\APP.GBB`)
	if err != nil {
		t.Fatal(err)
	}
	prefixed, err := ibmeta.NormalizeTarget(`ib://server/3050:D:\Data\APP.GBB`)
	if err != nil {
		t.Fatal(err)
	}
	if bare != prefixed {
		t.Fatalf("bare = %+v, prefixed = %+v, want equal", bare, prefixed)
	}
	upperScheme, err := ibmeta.NormalizeTarget(`IB://server/3050:D:\Data\APP.GBB`)
	if err != nil {
		t.Fatal(err)
	}
	if upperScheme != prefixed {
		t.Fatalf("case-insensitive prefix produced a different key: %+v vs %+v", upperScheme, prefixed)
	}
}

func TestNormalizeTargetCasePreserved(t *testing.T) {
	upper, err := ibmeta.NormalizeTarget(`server:D:\Data\A.GBB`)
	if err != nil {
		t.Fatal(err)
	}
	lower, err := ibmeta.NormalizeTarget(`server:D:\data\A.GBB`)
	if err != nil {
		t.Fatal(err)
	}
	if upper == lower {
		t.Fatal("case differences were folded away")
	}
}

func TestNormalizeTargetAliasesNotMerged(t *testing.T) {
	one, err := ibmeta.NormalizeTarget(`server1:D:\Data\APP.GBB`)
	if err != nil {
		t.Fatal(err)
	}
	two, err := ibmeta.NormalizeTarget(`server2:D:\Data\APP.GBB`)
	if err != nil {
		t.Fatal(err)
	}
	if one == two {
		t.Fatal("distinct host aliases were merged into one key")
	}
}

func TestNormalizeTargetAtSignPassthrough(t *testing.T) {
	key, err := ibmeta.NormalizeTarget(`user@server:D:\Data\APP.GBB`)
	if err != nil {
		t.Fatal(err)
	}
	if key.URL != `ib://user@server:D:\Data\APP.GBB` {
		t.Fatalf("URL = %q, want the '@' preserved verbatim", key.URL)
	}
}

func TestNormalizeTargetBlankRejected(t *testing.T) {
	if _, err := ibmeta.NormalizeTarget(""); !errors.Is(err, metadata.ErrInvalidTarget) {
		t.Fatalf("err = %v, want ErrInvalidTarget", err)
	}
	if _, err := ibmeta.NormalizeTarget("   "); !errors.Is(err, metadata.ErrInvalidTarget) {
		t.Fatalf("err = %v, want ErrInvalidTarget", err)
	}
	if _, err := ibmeta.NormalizeTarget("ib://"); !errors.Is(err, metadata.ErrInvalidTarget) {
		t.Fatalf("err = %v, want ErrInvalidTarget", err)
	}
}

func TestNormalizeTargetEngineIsInterbase(t *testing.T) {
	key, err := ibmeta.NormalizeTarget("server:db")
	if err != nil {
		t.Fatal(err)
	}
	if key.Engine != "interbase" {
		t.Fatalf("Engine = %q, want interbase", key.Engine)
	}
}
