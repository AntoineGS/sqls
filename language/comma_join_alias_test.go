package language_test

import (
	"context"
	"strings"
	"testing"

	"github.com/sqls-server/sqls/catalog"
	"github.com/sqls-server/sqls/dialect"
	"github.com/sqls-server/sqls/language"
)

func commaJoinAliasCache() *catalog.Cache {
	return &catalog.Cache{
		SchemaTables: map[string][]string{"": {"CITY", "MYVIEW", "CUSTOMERSHIPTO", "CUSTOMERS"}},
		ColumnsWithParent: map[string][]*catalog.ColumnDesc{
			catalog.ColumnDatabaseKey("", "CITY"): {
				{ColumnBase: catalog.ColumnBase{Table: "CITY", Name: "CITY_ID"}, Type: "INTEGER"},
			},
			catalog.ColumnDatabaseKey("", "MYVIEW"): {
				{ColumnBase: catalog.ColumnBase{Table: "MYVIEW", Name: "VIEW_ID"}, Type: "INTEGER"},
			},
			catalog.ColumnDatabaseKey("", "CUSTOMERSHIPTO"): {
				{ColumnBase: catalog.ColumnBase{Table: "CUSTOMERSHIPTO", Name: "SHIPTO_ID"}, Type: "INTEGER"},
			},
			catalog.ColumnDatabaseKey("", "CUSTOMERS"): {
				{ColumnBase: catalog.ColumnBase{Table: "CUSTOMERS", Name: "CUSTOMER_ID"}, Type: "INTEGER"},
			},
		},
		Metadata: map[catalog.MetadataKind]catalog.MetadataState{
			catalog.MetadataColumnsCurrent: catalog.MetadataReady,
			catalog.MetadataViews:          catalog.MetadataReady,
		},
	}
}

func commaJoinAliasRequest(text string) language.Request {
	return language.Request{
		Document: language.Document{URI: "memory:comma-join-alias", Text: text, Version: 1, Binding: 1},
		Context: language.Context{
			Snapshot:        &catalog.MetadataSnapshot{Cache: commaJoinAliasCache()},
			Variant:         dialect.DriverVariant{Driver: dialect.DatabaseDriverInterBase, Variant: dialect.SQLVariantInterBase3},
			DialectResolved: true,
		},
	}
}

func TestCompletionUsesAliasedTablesInCommaJoinLists(t *testing.T) {
	tests := []struct {
		name  string
		text  string
		alias string
		want  string
		omit  string
	}{
		{name: "mixed aliased and unaliased", text: "select c. from city c, myview", alias: "c", want: "CITY_ID", omit: "VIEW_ID"},
		{name: "two aliases in lookup shape, first", text: "select a. from customershipto a, customers b", alias: "a", want: "SHIPTO_ID", omit: "CUSTOMER_ID"},
		{name: "two aliases in lookup shape, second", text: "select b. from customershipto a, customers b", alias: "b", want: "CUSTOMER_ID", omit: "SHIPTO_ID"},
		{name: "same relation under two aliases", text: "select b. from city a, city b", alias: "b", want: "CITY_ID"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			items := completeAt(t, commaJoinAliasRequest(tt.text), len("select ")+len(tt.alias)+1)
			labels := completionLabels(items)
			if !labels[tt.want] {
				t.Fatalf("completion labels = %v, want %q", labels, tt.want)
			}
			if tt.omit != "" && labels[tt.omit] {
				t.Fatalf("completion labels = %v, unexpectedly include %q", labels, tt.omit)
			}
		})
	}
}

func TestHoverResolvesAliasedTablesInCommaJoinLists(t *testing.T) {
	tests := []struct {
		name  string
		text  string
		field string
		table string
	}{
		{name: "mixed aliased and unaliased", text: "select c.CITY_ID from city c, myview", field: "CITY_ID", table: "city"},
		{name: "two aliases in lookup shape", text: "select a.SHIPTO_ID from customershipto a, customers b", field: "SHIPTO_ID", table: "customershipto"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			position := strings.Index(tt.text, tt.field) + 1
			result, err := language.NewService().Hover(context.Background(), commaJoinAliasRequest(tt.text), language.Position{UTF16Column: position})
			if err != nil {
				t.Fatalf("Hover(): %v", err)
			}
			column, ok := commaJoinAliasCache().Column(tt.table, tt.field)
			if !ok {
				t.Fatalf("test fixture lacks %s.%s", tt.table, tt.field)
			}
			want := catalog.ColumnHoverDoc(tt.table, column)
			if result.Value.Markdown != want {
				t.Fatalf("hover markdown = %q, want %q", result.Value.Markdown, want)
			}
		})
	}
}
