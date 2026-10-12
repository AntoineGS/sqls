package language

import (
	"fmt"
	"strings"

	"github.com/sqls-server/sqls/catalog"
	"github.com/sqls-server/sqls/dialect"
	"github.com/sqls-server/sqls/parser/parseutil"
)

func (c *completionEngine) keywordCandidates(lower bool, keywords []string) []CompletionItem {
	candidates := []CompletionItem{}
	for _, k := range keywords {
		candidate := CompletionItem{
			Label:  k,
			Kind:   KindKeyword,
			Detail: "keyword",
		}
		if lower {
			candidate.Label = strings.ToLower(candidate.Label)
		}
		candidates = append(candidates, candidate)
	}
	return candidates
}

func (c *completionEngine) functionCandidates(lower bool, keywords []string) []CompletionItem {
	candidates := []CompletionItem{}
	for _, k := range keywords {
		candidate := CompletionItem{
			Label:  k,
			Kind:   KindFunction,
			Detail: "Function",
		}
		if lower {
			candidate.Label = strings.ToLower(candidate.Label)
		}
		candidates = append(candidates, candidate)
	}
	return candidates
}

func (c *completionEngine) columnCandidates(targetTables []*parseutil.TableInfo, parent *completionParent) []CompletionItem {
	candidates := []CompletionItem{}

	switch parent.Type {
	case completionParentTypeNone:
		for _, table := range targetTables {
			if table.DatabaseSchema != "" && table.Name != "" {
				columns, ok := c.cache.ColumnDatabase(table.DatabaseSchema, table.Name)
				if !ok {
					continue
				}
				candidates = append(candidates, generateColumnCandidates(table.Name, columns)...)
			} else if table.Name != "" {
				columns, ok := c.cache.ColumnDescs(table.Name)
				if ok {
					candidates = append(candidates, generateColumnCandidates(table.Name, columns)...)
					continue
				}
				// The relation is not a table. On InterBase it may be a
				// selectable procedure, whose output parameters are its
				// columns.
				candidates = append(candidates, c.procedureColumnCandidates(table.Name)...)
			}
		}
	case completionParentTypeSchema:
		// pass
	case completionParentTypeTable:
		for _, table := range targetTables {
			if table.Name != parent.Name && table.Alias != parent.Name {
				continue
			}
			columns, ok := c.cache.ColumnDescs(table.Name)
			if ok {
				candidates = append(candidates, generateColumnCandidates(table.Name, columns)...)
				continue
			}
			candidates = append(candidates, c.procedureColumnCandidates(table.Name)...)
		}
	case completionParentTypeSubQuery:
		// pass
	}
	return candidates
}

func generateColumnCandidates(tableName string, columns []*catalog.ColumnDesc) []CompletionItem {
	candidates := []CompletionItem{}
	for _, column := range columns {
		candidate := CompletionItem{
			Label:         column.Name,
			Kind:          KindField,
			Detail:        columnDetail(tableName),
			Documentation: catalog.ColumnDoc(tableName, column),
		}
		candidates = append(candidates, candidate)
	}
	return candidates
}

func columnDetail(tableName string) string {
	detail := strings.Join(
		[]string{
			"column from",
			"\"" + tableName + "\"",
		},
		" ",
	)
	return detail
}

func (c *completionEngine) referencedTableCandidates(targetTables []*parseutil.TableInfo) []CompletionItem {
	candidates := []CompletionItem{}

	for _, targetTable := range targetTables {
		includeTables := []*parseutil.TableInfo{}
		for _, table := range c.cache.SortedTables() {
			if table == targetTable.Name {
				includeTables = append(includeTables, targetTable)
			}
		}
		genCands := generateTableCandidatesByInfos(includeTables, c.cache)
		candidates = append(candidates, genCands...)
	}
	return candidates
}

func (c *completionEngine) tableCandidates(parent *completionParent, targetTables []*parseutil.TableInfo) []CompletionItem {
	candidates := []CompletionItem{}

	switch parent.Type {
	case completionParentTypeNone:
		excludeTables := []string{}
		for _, table := range c.cache.SortedTables() {
			isExclude := false
			for _, targetTable := range targetTables {
				if table == targetTable.Name {
					isExclude = true
				}
			}
			if isExclude {
				continue
			}
			excludeTables = append(excludeTables, table)
		}
		candidates = append(candidates, generateTableCandidates(excludeTables, c.cache)...)
	case completionParentTypeSchema:
		tables, ok := c.cache.SortedTablesByDBName(parent.Name)
		if ok {
			candidates = append(candidates, generateTableCandidates(tables, c.cache)...)
		}
	case completionParentTypeTable:
		// pass
	case completionParentTypeSubQuery:
		// pass
	}
	return candidates
}

func (c *completionEngine) joinCandidates(lastTable *parseutil.TableInfo,
	targetTables, allTables []*parseutil.TableInfo,
	joinOn, lowercaseKeywords bool) []CompletionItem {
	var candidates []CompletionItem
	if len(c.cache.ForeignKeys) == 0 {
		return candidates
	}

	caseInsensitive := c.driver == dialect.DatabaseDriverInterBase
	tMap := make(map[string]*parseutil.TableInfo)
	for _, t := range targetTables {
		tMap[t.Name] = t
	}
	fkMap := make(map[string][][]*catalog.ForeignKey)
	if lastTable == nil {
		for t := range tMap {
			for k, v := range c.foreignKeysForTable(t) {
				fkMap[k] = append(fkMap[k], v)
			}
		}
	} else {
		delete(tMap, lastTable.Name)
		rTab := []*parseutil.TableInfo{lastTable}
		if !joinOn {
			rTab = resolveTables(lastTable, c.cache)
		}
		for _, lt := range rTab {
			for k, v := range c.foreignKeysForTable(lt.Name) {
				if _, ok := tableInfoForName(tMap, k, caseInsensitive); ok {
					fkMap[lt.Name] = append(fkMap[lt.Name], v)
				}
			}
		}

		for _, t := range rTab {
			if _, ok := tableInfoForName(tMap, t.Name, caseInsensitive); !ok {
				tMap[t.Name] = t
			}
		}
	}

	aliases := make(map[string]interface{})
	for _, t := range allTables {
		if t.Alias != "" {
			aliases[t.Alias] = true
		}
	}

	for k, v := range fkMap {
		for _, fks := range v {
			for _, fk := range fks {
				candidates = append(candidates, generateForeignKeyCandidate(k, tMap, aliases,
					fk, joinOn, lowercaseKeywords, caseInsensitive))
			}
		}
	}
	return candidates
}

func (c *completionEngine) foreignKeysForTable(tableName string) map[string][]*catalog.ForeignKey {
	foreignKeys, ok := c.cache.ForeignKeys[tableName]
	if ok || c.driver != dialect.DatabaseDriverInterBase {
		return foreignKeys
	}
	// InterBase's metadata reader indexes tables with the same unqualified
	// key used by the column cache. Keep this exact empty-schema lookup scoped
	// to InterBase so foreign keys from another namespace cannot match by name.
	foreignKeys, ok = c.cache.ForeignKeys[catalog.ColumnDatabaseKey("", tableName)]
	if ok {
		return interBaseForeignKeyReferences(foreignKeys)
	}
	for cachedTableName, foreignKeys := range c.cache.ForeignKeys {
		if strings.EqualFold(cachedTableName, tableName) {
			return foreignKeys
		}
	}
	return nil
}

func interBaseForeignKeyReferences(references map[string][]*catalog.ForeignKey) map[string][]*catalog.ForeignKey {
	tables := make(map[string][]*catalog.ForeignKey, len(references))
	for key, foreignKeys := range references {
		// An empty-schema ColumnDatabaseKey starts with one tab. Do not strip
		// keys containing another schema, which would leak a different
		// namespace into this unqualified InterBase lookup.
		if !strings.HasPrefix(key, "\t") {
			continue
		}
		tableName := strings.TrimPrefix(key, "\t")
		if tableName == "" {
			continue
		}
		tables[tableName] = foreignKeys
	}
	return tables
}

func tableInfoForName(tMap map[string]*parseutil.TableInfo, name string, caseInsensitive bool) (*parseutil.TableInfo, bool) {
	if table, ok := tMap[name]; ok {
		return table, true
	}
	if !caseInsensitive {
		return nil, false
	}
	for tableName, table := range tMap {
		if strings.EqualFold(tableName, name) {
			return table, true
		}
	}
	return nil, false
}

func tableNamesEqual(left, right string, caseInsensitive bool) bool {
	return left == right || (caseInsensitive && strings.EqualFold(left, right))
}

func resolveTables(t *parseutil.TableInfo, cache *catalog.Cache) []*parseutil.TableInfo {
	if _, ok := cache.ColumnDescs(t.Name); ok {
		return []*parseutil.TableInfo{t}
	}
	var rv []*parseutil.TableInfo
	targetName := strings.ToLower(t.Name)
	for _, cond := range cache.SortedTables() {
		if strings.Contains(strings.ToLower(cond), targetName) {
			rv = append(rv, &parseutil.TableInfo{
				Name: cond,
			})
		}
	}
	return rv
}

func generateTableAlias(target string,
	aliases map[string]interface{}) string {
	ch := []rune(target)[0]
	i := 1
	var rv string
	for {
		rv = fmt.Sprintf("%c%d", ch, i)
		if _, ok := aliases[rv]; ok {
			i++
			continue
		}
		break
	}
	return rv
}

func generateForeignKeyCandidate(target string,
	tMap map[string]*parseutil.TableInfo,
	aliases map[string]interface{},
	fk *catalog.ForeignKey,
	joinOn, lowercaseKeywords, caseInsensitive bool) CompletionItem {
	var tAlias string
	if joinOn {
		if table, ok := tableInfoForName(tMap, target, caseInsensitive); ok {
			tAlias = table.Alias
			if tAlias == "" {
				tAlias = table.Name
			}
		} else {
			tAlias = target
		}
	} else {
		tAlias = generateTableAlias(target, aliases)
	}
	builder := []struct {
		sb      *strings.Builder
		alias   string
		snippet bool
	}{
		{
			sb:      &strings.Builder{},
			alias:   tAlias,
			snippet: false,
		},
		{
			sb:      &strings.Builder{},
			alias:   escapeSnippetLiteral(tAlias, caseInsensitive),
			snippet: true,
		},
	}
	if !joinOn {
		builder[1].alias = fmt.Sprintf("${1:%s}", builder[1].alias)
		onKw := "ON"
		if lowercaseKeywords {
			onKw = "on"
		}
		for _, b := range builder {
			fmt.Fprintf(b.sb, "%s %s %s ", escapeSnippetLiteral(target, b.snippet && caseInsensitive), b.alias, onKw)
		}
	}
	andKw := " AND "
	if lowercaseKeywords {
		andKw = " and "
	}
	prefix := ""
	for _, cur := range *fk {
		tIdx, rIdx := 0, 1
		if tableNamesEqual(cur[rIdx].Table, target, caseInsensitive) {
			tIdx, rIdx = rIdx, tIdx
		}
		for _, b := range builder {
			b.sb.WriteString(prefix)
		}
		prefix = andKw
		for _, b := range builder {
			b.sb.WriteString(strings.Join([]string{
				b.alias,
				escapeSnippetLiteral(cur[tIdx].Name, b.snippet && caseInsensitive),
			}, "."))
			b.sb.WriteString(" = ")
		}
		rAlias := ""
		if table, ok := tableInfoForName(tMap, cur[rIdx].Table, caseInsensitive); ok {
			rAlias = table.Alias
			if rAlias == "" {
				rAlias = table.Name
			}
		}
		if rAlias == "" {
			rAlias = cur[rIdx].Table
		}
		for _, b := range builder {
			b.sb.WriteString(strings.Join([]string{
				escapeSnippetLiteral(rAlias, b.snippet && caseInsensitive),
				escapeSnippetLiteral(cur[rIdx].Name, b.snippet && caseInsensitive),
			}, "."))
		}
	}
	builder[1].sb.WriteString("$0")
	return CompletionItem{
		Label:      builder[0].sb.String(),
		Kind:       KindSnippet,
		Detail:     "Join generator for foreign key",
		InsertText: builder[1].sb.String(),
		Snippet:    true,
	}
}

func escapeSnippetLiteral(value string, escape bool) string {
	if !escape {
		return value
	}
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, `$`, `\$`)
	return strings.ReplaceAll(value, `}`, `\}`)
}

func generateTableCandidates(tables []string, dbCache *catalog.Cache) []CompletionItem {
	candidates := []CompletionItem{}
	for _, tableName := range tables {
		candidate := CompletionItem{
			Label:  tableName,
			Kind:   KindClass,
			Detail: "table",
		}
		// Views live in SchemaTables alongside tables, so this candidate is
		// already the view's only candidate. It is relabelled here rather than
		// duplicated by a separate view generator. dbCache.View is nil-safe
		// and returns false for every driver without a catalog, so no other
		// driver's output changes.
		if view, ok := dbCache.View(tableName); ok {
			candidate.Detail = "view"
			candidate.Documentation = catalog.ViewDoc(view)
			candidates = append(candidates, candidate)
			continue
		}
		cols, ok := dbCache.ColumnDescs(tableName)
		if ok {
			candidate.Documentation = catalog.TableDoc(tableName, cols)
		}
		candidates = append(candidates, candidate)
	}
	return candidates
}

func generateTableCandidatesByInfos(tables []*parseutil.TableInfo, dbCache *catalog.Cache) []CompletionItem {
	candidates := []CompletionItem{}
	for _, table := range tables {
		name := table.Name
		detail := "referenced table"
		if table.Alias != "" {
			name = table.Alias
			detail = "aliased table"
		}
		if _, ok := dbCache.View(table.Name); ok {
			detail = strings.Replace(detail, "table", "view", 1)
		}
		candidate := CompletionItem{
			Label:  name,
			Kind:   KindClass,
			Detail: detail,
		}
		cols, ok := dbCache.ColumnDescs(table.Name)
		if ok {
			candidate.Documentation = catalog.TableDoc(table.Name, cols)
		}
		candidates = append(candidates, candidate)
	}
	return candidates
}

func (c *completionEngine) subQueryCandidates(infos []*parseutil.SubQueryInfo) []CompletionItem {
	candidates := []CompletionItem{}
	for _, info := range infos {
		candidate := CompletionItem{
			Label:         info.Name,
			Kind:          KindField,
			Detail:        "subquery",
			Documentation: catalog.SubqueryDoc(info.Name, info.Views, c.cache),
		}
		candidates = append(candidates, candidate)
	}
	return candidates
}

func (c *completionEngine) subQueryColumnCandidates(infos []*parseutil.SubQueryInfo) []CompletionItem {
	candidates := []CompletionItem{}
	for _, info := range infos {
		for _, view := range info.Views {
			for _, col := range view.SubQueryColumns {
				if col.ColumnName == "*" {
					tableCols, ok := c.cache.ColumnDescs(col.ParentTable.Name)
					if !ok {
						continue
					}
					for _, tableCol := range tableCols {
						candidate := CompletionItem{
							Label:         tableCol.Name,
							Kind:          KindField,
							Detail:        subQueryColumnDetail(info.Name),
							Documentation: catalog.SubqueryColumnDoc(tableCol.Name, info.Views, c.cache),
						}
						candidates = append(candidates, candidate)
					}
				} else {
					candidate := CompletionItem{
						Label:         col.DisplayName(),
						Kind:          KindField,
						Detail:        subQueryColumnDetail(info.Name),
						Documentation: catalog.SubqueryColumnDoc(col.DisplayName(), info.Views, c.cache),
					}
					candidates = append(candidates, candidate)
				}
			}
		}
	}
	return candidates
}

func subQueryColumnDetail(subQueryAliasName string) string {
	detail := strings.Join(
		[]string{
			"subquery column from",
			"\"" + subQueryAliasName + "\"",
		},
		" ",
	)
	return detail
}

func (c *completionEngine) schemaCandidates() []CompletionItem {
	candidates := []CompletionItem{}
	dbs := c.cache.SortedSchemas()
	for _, db := range dbs {
		candidate := CompletionItem{
			Label:  db,
			Kind:   KindModule,
			Detail: "schema",
		}
		candidates = append(candidates, candidate)
	}
	return candidates
}
