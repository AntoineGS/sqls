package sqlsymbol

import (
	"strings"
	"testing"

	"github.com/sqls-server/sqls/dialect"
)

func TestSQLUpdateColumnContext(t *testing.T) {
	text := "UPDATE customerinvoice SET amountpaid=0, balance=:ordertotal WHERE invoice=:invoice;"
	a, err := Analyze(text, dialect.DriverVariant{Driver: dialect.DatabaseDriverInterBase})
	if err != nil {
		t.Fatal(err)
	}
	r := a.Resolve(strings.Index(text, "amountpaid"))
	if r.Role != Column || r.SQL == nil {
		t.Fatalf("resolution: %+v", r)
	}
	if len(r.SQL.Scopes) != 1 || len(r.SQL.Scopes[0]) != 1 {
		t.Fatalf("scopes: %+v", r.SQL.Scopes)
	}
	if r.SQL.Scopes[0][0].Name.Key() != "CUSTOMERINVOICE" {
		t.Fatalf("wrong relation: %+v", r.SQL.Scopes)
	}
}

func TestSQLRelationScopesAndAliases(t *testing.T) {
	text := `SELECT c.id FROM customer c JOIN address a ON a.customer_id = c.id
INSERT INTO customer (id, name) VALUES (1, 'one');
DELETE FROM customer c WHERE c.id = 1;`
	a, err := Analyze(text, interBaseVariant())
	if err != nil {
		t.Fatal(err)
	}

	qualified := a.Resolve(strings.Index(text, "c.id") + len("c."))
	if qualified.Role != Column || qualified.SQL == nil {
		t.Fatalf("qualified SELECT column: %+v", qualified)
	}
	if len(qualified.SQL.Scopes) != 1 || len(qualified.SQL.Scopes[0]) != 2 {
		t.Fatalf("qualified SELECT scopes: %+v", qualified.SQL.Scopes)
	}
	if qualified.SQL.Scopes[0][0].Name.Key() != "CUSTOMER" || qualified.SQL.Scopes[0][0].Alias == nil || qualified.SQL.Scopes[0][0].Alias.Key() != "C" {
		t.Fatalf("customer binding: %+v", qualified.SQL.Scopes[0][0])
	}
	if qualified.SQL.Scopes[0][1].Name.Key() != "ADDRESS" || qualified.SQL.Scopes[0][1].Alias == nil || qualified.SQL.Scopes[0][1].Alias.Key() != "A" {
		t.Fatalf("address binding: %+v", qualified.SQL.Scopes[0][1])
	}

	aliasOffset := strings.Index(text, "c.id")
	if got := a.Resolve(aliasOffset); got.Role != Alias {
		t.Fatalf("qualifier role: %+v", got)
	}
	insertColumn := strings.Index(text, "INSERT INTO customer (") + len("INSERT INTO customer (")
	if got := a.Resolve(insertColumn); got.Role != Column || len(got.SQL.Scopes) != 1 || len(got.SQL.Scopes[0]) != 1 || got.SQL.Scopes[0][0].Name.Key() != "CUSTOMER" {
		t.Fatalf("INSERT column: %+v", got)
	}
	deleteColumn := strings.LastIndex(text, "c.id") + len("c.")
	if got := a.Resolve(deleteColumn); got.Role != Column || len(got.SQL.Scopes) != 1 || got.SQL.Scopes[0][0].Name.Key() != "CUSTOMER" {
		t.Fatalf("DELETE predicate: %+v", got)
	}
}

func TestSQLNestedScopesShadowAliasesAndCorrelate(t *testing.T) {
	text := `SELECT c.id FROM customer c
WHERE EXISTS (SELECT o.id FROM orders o WHERE c.id = o.id);`
	a, err := Analyze(text, interBaseVariant())
	if err != nil {
		t.Fatal(err)
	}
	correlatedOffset := strings.Index(text, "WHERE c.id") + len("WHERE ") + len("c.")
	r := a.Resolve(correlatedOffset)
	if r.Role != Column || r.SQL == nil || r.SQL.Qualifier == nil || r.SQL.Qualifier.Key() != "C" {
		t.Fatalf("correlated member: %+v", r)
	}
	if len(r.SQL.Scopes) != 2 || len(r.SQL.Scopes[0]) != 1 || len(r.SQL.Scopes[1]) != 1 {
		t.Fatalf("correlated scopes: %+v", r.SQL.Scopes)
	}
	if r.SQL.Scopes[0][0].Name.Key() != "ORDERS" || r.SQL.Scopes[1][0].Name.Key() != "CUSTOMER" {
		t.Fatalf("scope order: %+v", r.SQL.Scopes)
	}
	qualifier := a.Resolve(strings.Index(text, "WHERE c.id") + len("WHERE "))
	if qualifier.Role != Alias {
		t.Fatalf("outer qualifier role: %+v", qualifier)
	}
}

func TestSQLNestedScopesKeepReusedAliasesSeparate(t *testing.T) {
	text := `SELECT c.id FROM customer c
WHERE c.id IN (SELECT c.id FROM orders c WHERE c.id = c.id);`
	a, err := Analyze(text, interBaseVariant())
	if err != nil {
		t.Fatal(err)
	}
	offset := strings.Index(text, "SELECT c.id FROM orders") + len("SELECT c.")
	r := a.Resolve(offset)
	if r.Role != Column || r.SQL == nil || len(r.SQL.Scopes) != 2 {
		t.Fatalf("inner reused alias: %+v", r)
	}
	if got := r.SQL.Scopes[0][0]; got.Name.Key() != "ORDERS" || got.Alias == nil || got.Alias.Key() != "C" {
		t.Fatalf("inner alias binding: %+v", got)
	}
	if got := r.SQL.Scopes[1][0]; got.Name.Key() != "CUSTOMER" || got.Alias == nil || got.Alias.Key() != "C" {
		t.Fatalf("outer alias binding: %+v", got)
	}
}

func TestSQLDerivedRelationRetainsUnknownOwnership(t *testing.T) {
	text := `SELECT d.amount, t.id FROM (SELECT amount FROM customer) d JOIN totals t ON t.id = d.id;`
	a, err := Analyze(text, interBaseVariant())
	if err != nil {
		t.Fatal(err)
	}
	offset := strings.Index(text, "d.amount") + len("d.")
	r := a.Resolve(offset)
	if r.Role != Column || r.SQL == nil || len(r.SQL.Scopes) != 1 || len(r.SQL.Scopes[0]) != 2 {
		t.Fatalf("derived member: %+v", r)
	}
	if r.SQL.Scopes[0][0].Name != (Name{}) || r.SQL.Scopes[0][0].Alias == nil || r.SQL.Scopes[0][0].Alias.Key() != "D" {
		t.Fatalf("derived binding: %+v", r.SQL.Scopes[0][0])
	}
	if r.SQL.Scopes[0][1].Name.Key() != "TOTALS" {
		t.Fatalf("concrete binding: %+v", r.SQL.Scopes[0][1])
	}
}
