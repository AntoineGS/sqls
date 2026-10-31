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

// Trigger is a recognized CREATE TRIGGER statement.
type Trigger struct {
	// Span runs from CREATE through the body's END.
	Span     Span
	Relation Name
	// RowColumns are the column words of NEW.<column> and OLD.<column> row
	// references in the body, which belong to Relation.
	RowColumns []Span
	// Locals holds the keys of the declared variables.
	Locals map[string]bool
}

// Triggers returns the recognized CREATE TRIGGER statements in source order.
func (a *Analysis) Triggers() []Trigger {
	if a == nil {
		return nil
	}
	var triggers []Trigger
	items := significantLexemes(a.lexemes)
	for i := 0; i+1 < len(items); i++ {
		if !isWord(items[i], "CREATE") || !isWord(items[i+1], "TRIGGER") {
			continue
		}
		model, ok := parseTrigger(a.Text, items, i)
		if !ok {
			continue
		}
		trg := Trigger{Span: Span{Start: items[i].Span.Start, End: items[model.bodyEnd].Span.End}, Relation: model.relation, Locals: make(map[string]bool, len(model.locals))}
		for key := range model.locals {
			trg.Locals[key] = true
		}
		for idx := model.bodyStart + 1; idx+2 < model.bodyEnd; idx++ {
			alias, ok := nameFromLexeme(a.Text, items[idx])
			if ok && isTriggerRowAlias(alias) && items[idx+1].Token.Kind == token.Period && isNameToken(items[idx+2]) {
				trg.RowColumns = append(trg.RowColumns, items[idx+2].Span)
			}
		}
		triggers = append(triggers, trg)
	}
	return triggers
}
