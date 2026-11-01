package sqlsymbol

import "github.com/sqls-server/sqls/token"

// NameOccurrence is one identifier word and its decoded SQL name.
type NameOccurrence struct {
	Name Name
	Span Span
}

// CatalogNameSpans returns, in source order, the identifier words whose SQL
// identity is the catalog spelling name. Comments and literals are never
// words, and a :variable reference never names a catalog object, so both are
// excluded.
func (a *Analysis) CatalogNameSpans(name string) []NameOccurrence {
	if a == nil {
		return nil
	}
	var occurrences []NameOccurrence
	items := significantLexemes(a.lexemes)
	for i, item := range items {
		if !isNameToken(item) || (i > 0 && items[i-1].Token.Kind == token.Colon) {
			continue
		}
		if word, ok := nameFromLexeme(a.Text, item); ok && word.MatchesCatalogName(name) {
			occurrences = append(occurrences, NameOccurrence{Name: word, Span: item.Span})
		}
	}
	return occurrences
}

// Trigger is a recognized CREATE or ALTER TRIGGER statement.
type Trigger struct {
	// Span runs from CREATE/ALTER through the body's END.
	Span Span
	// Relation is the CREATE TRIGGER's FOR relation, or for ALTER TRIGGER the
	// one Triggers' TriggerCatalog proves; zero when unproven.
	Relation Name
	// RowColumns are the column words of NEW.<column> and OLD.<column> row
	// references in the body, which belong to Relation. Empty when Relation is
	// unproven.
	RowColumns []Span
	// Locals holds the keys of the declared variables.
	Locals map[string]bool
}

// Triggers returns the recognized CREATE and ALTER TRIGGER statements in
// source order. An ALTER TRIGGER's relation comes from relations, with
// diagnostics' rules: unproven when relations is nil, does not know the
// trigger, or an earlier statement in the document creates, alters or drops
// it.
func (a *Analysis) Triggers(relations TriggerCatalog) []Trigger {
	if a == nil {
		return nil
	}
	var triggers []Trigger
	items := significantLexemes(a.lexemes)
	var ddl []modelDDL
	ddlDetected := false
	invalidated := func(i int, name Name) bool {
		if !ddlDetected {
			ddlDetected = true
			ddl = detectDDL(a.Text, items, splitStatements(items, boundaryAfterItems(items, scriptDelimiterOffsets(a.Text))))
		}
		return ddlInvalidated(ddl, i, name)
	}
	for i := 0; i+1 < len(items); i++ {
		if !(isWord(items[i], "CREATE") || isWord(items[i], "ALTER")) || !isWord(items[i+1], "TRIGGER") {
			continue
		}
		model, ok := parseTrigger(a.Text, items, i)
		if !ok {
			continue
		}
		trg := Trigger{Span: Span{Start: items[i].Span.Start, End: items[model.bodyEnd].Span.End}, Locals: make(map[string]bool, len(model.locals))}
		for key := range model.locals {
			trg.Locals[key] = true
		}
		relation, ok := triggerRelation(model, relations, invalidated)
		if ok {
			trg.Relation = relation
			for idx := model.bodyStart + 1; idx+2 < model.bodyEnd; idx++ {
				alias, ok := nameFromLexeme(a.Text, items[idx])
				if ok && isTriggerRowAlias(alias) && items[idx+1].Token.Kind == token.Period && isNameToken(items[idx+2]) {
					trg.RowColumns = append(trg.RowColumns, items[idx+2].Span)
				}
			}
		}
		triggers = append(triggers, trg)
	}
	return triggers
}
