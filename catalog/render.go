package catalog

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/sqls-server/sqls/parser/parseutil"
)

func ColumnDoc(tableName string, colDesc *ColumnDesc) string {
	buf := new(bytes.Buffer)
	fmt.Fprintf(buf, "`%s`.`%s` column", tableName, colDesc.Name)
	fmt.Fprintln(buf)
	fmt.Fprintln(buf)
	fmt.Fprintln(buf, colDesc.OnelineDesc())
	return buf.String()
}

// ColumnHoverDoc renders a column's definition facts for hover popups. Unlike
// ColumnDoc, this also includes known nullability and default metadata.
func ColumnHoverDoc(tableName string, colDesc *ColumnDesc) string {
	buf := new(bytes.Buffer)
	fmt.Fprintf(buf, "`%s`.`%s` column", tableName, colDesc.Name)
	fmt.Fprintln(buf)
	fmt.Fprintln(buf)
	description := colDesc.OnelineDesc()
	switch strings.ToUpper(colDesc.Null) {
	case "NO":
		description = appendColumnFact(description, "NOT NULL")
	case "YES":
		description = appendColumnFact(description, "NULL")
	}
	if colDesc.Default.Valid && colDesc.Default.String != "" {
		description = appendColumnFact(description, "DEFAULT "+codeSpan(colDesc.Default.String))
	}
	fmt.Fprintln(buf, description)
	return buf.String()
}

func appendColumnFact(description, fact string) string {
	if description == "" {
		return fact
	}
	return description + " " + fact
}

func Coalesce(str ...string) string {
	for _, s := range str {
		if s != "" {
			return s
		}
	}
	return ""
}

func TableDoc(tableName string, cols []*ColumnDesc) string {
	buf := new(bytes.Buffer)
	fmt.Fprintf(buf, "# %s table", codeSpan(tableName))
	fmt.Fprintln(buf)
	fmt.Fprintln(buf)
	fmt.Fprintln(buf)
	writeColumnTable(buf, cols)
	return buf.String()
}

func writeColumnTable(buf *bytes.Buffer, cols []*ColumnDesc) {
	fmt.Fprintln(buf, "| Name&nbsp;&nbsp; | Type&nbsp;&nbsp; | Primary&nbsp;key&nbsp;&nbsp; | Default&nbsp;&nbsp; | Extra&nbsp;&nbsp; |")
	fmt.Fprintln(buf, "| :--------------- | :--------------- | :---------------------- | :------------------ | :---------------- |")
	for _, col := range cols {
		fmt.Fprintf(buf, "| %s | %s | %s | %s | %s |",
			tableCellCode(col.Name),
			tableCellCode(col.Type),
			tableCellCode(col.Key),
			tableCellCode(Coalesce(col.Default.String, "-")),
			tableCellText(col.Extra),
		)
		fmt.Fprintln(buf)
	}
}

func SubqueryDoc(name string, views []*parseutil.SubQueryView, dbCache *Cache) string {
	buf := new(bytes.Buffer)
	fmt.Fprintf(buf, "%s subquery", name)
	fmt.Fprintln(buf)
	fmt.Fprintln(buf)
	for _, view := range views {
		for _, colmun := range view.SubQueryColumns {
			if colmun.ColumnName == "*" {
				tableCols, ok := dbCache.ColumnDescs(colmun.ParentTable.Name)
				if !ok {
					continue
				}
				for _, tableCol := range tableCols {
					fmt.Fprintf(buf, "- %s(%s.%s): %s", tableCol.Name, colmun.ParentTable.Name, tableCol.Name, tableCol.OnelineDesc())
					fmt.Fprintln(buf)
				}
			} else {
				columnDesc, ok := dbCache.Column(colmun.ParentTable.Name, colmun.ColumnName)
				if !ok {
					continue
				}
				fmt.Fprintf(buf, "- %s(%s.%s): %s", colmun.DisplayName(), colmun.ParentTable.Name, colmun.ColumnName, columnDesc.OnelineDesc())
				fmt.Fprintln(buf)

			}
		}
	}
	return buf.String()
}

func SubqueryColumnDoc(identName string, views []*parseutil.SubQueryView, dbCache *Cache) string {
	buf := new(bytes.Buffer)
	fmt.Fprintf(buf, "%s subquery column", identName)
	fmt.Fprintln(buf)
	fmt.Fprintln(buf)
	for _, view := range views {
		for _, colmun := range view.SubQueryColumns {
			if colmun.ColumnName == "*" {
				tableCols, ok := dbCache.ColumnDescs(colmun.ParentTable.Name)
				if !ok {
					continue
				}
				for _, tableCol := range tableCols {
					if identName == tableCol.Name {
						fmt.Fprintf(buf, "- %s(%s.%s): %s", identName, colmun.ParentTable.Name, tableCol.Name, tableCol.OnelineDesc())
						fmt.Fprintln(buf)
						continue
					}
				}
			} else {
				if identName != colmun.ColumnName && identName != colmun.AliasName {
					continue
				}
				columnDesc, ok := dbCache.Column(colmun.ParentTable.Name, colmun.ColumnName)
				if !ok {
					continue
				}
				fmt.Fprintf(buf, "- %s(%s.%s): %s", identName, colmun.ParentTable.Name, colmun.ColumnName, columnDesc.OnelineDesc())
				fmt.Fprintln(buf)
			}
		}
	}
	return buf.String()
}
