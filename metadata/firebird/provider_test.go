package fbmeta

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/sqls-server/sqls/metadata"
)

func TestKeyForUsesCanonicalTarget(t *testing.T) {
	cfg := Config{Target: "FB://server:/srv/db.fdb"}
	got, err := KeyFor(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got.Engine != "firebird" || got.URL != "fb://server:/srv/db.fdb" {
		t.Fatalf("KeyFor() = %+v, want Firebird key with canonical scheme", got)
	}
}

func TestKeyForRejectsBlankTarget(t *testing.T) {
	if _, err := KeyFor(Config{}); !errors.Is(err, metadata.ErrInvalidTarget) {
		t.Fatalf("KeyFor(blank) error = %v, want ErrInvalidTarget", err)
	}
}

func TestScopeForRequiresAuthenticatedPrincipal(t *testing.T) {
	for _, user := range []string{"", "   "} {
		if _, err := ScopeFor(Config{User: user}); !errors.Is(err, metadata.ErrScopeRequired) {
			t.Errorf("ScopeFor(user %q) error = %v, want ErrScopeRequired", user, err)
		}
	}
}

func TestScopeForUsesFixedTextPolicyAndOmitsSecrets(t *testing.T) {
	scope, err := ScopeFor(Config{User: " SYSDBA ", Password: "secret", Role: " RDB$ADMIN "})
	if err != nil {
		t.Fatal(err)
	}
	if scope.Principal != "SYSDBA" || scope.Role != "RDB$ADMIN" || scope.TextPolicy != "NONE/WIN1252" {
		t.Fatalf("ScopeFor() = %+v, want trimmed identity and NONE/WIN1252 policy", scope)
	}
	if strings.Contains(scope.Principal+scope.Role+scope.TextPolicy, "secret") {
		t.Fatalf("scope contains password: %+v", scope)
	}
}

func TestFactoryValidatesTargetBeforeOpeningAttachment(t *testing.T) {
	_, err := Factory(Config{User: "SYSDBA"})(context.Background())
	if !errors.Is(err, metadata.ErrInvalidTarget) {
		t.Fatalf("Factory(blank target) error = %v, want ErrInvalidTarget", err)
	}
}
