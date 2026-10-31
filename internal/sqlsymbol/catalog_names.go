package sqlsymbol

import "github.com/sqls-server/sqls/token"

// CatalogNameSpans returns, in source order, the spans of identifier words
// whose SQL identity is the catalog spelling name. Comments and literals are
// never words, so they are excluded.
func (a *Analysis) CatalogNameSpans(name string) []Span {
	if a == nil {
		return nil
	}
	var spans []Span
	for _, item := range a.lexemes {
		if !isNameToken(item) {
			continue
		}
		if word, ok := nameFromLexeme(a.Text, item); ok && word.MatchesCatalogName(name) {
			spans = append(spans, item.Span)
		}
	}
	return spans
}

// TriggerRowRelation returns the FOR relation of the trigger whose body holds
// the column word at offset in a NEW.<column> or OLD.<column> row reference.
func (a *Analysis) TriggerRowRelation(offset int) (Name, bool) {
	if a == nil {
		return Name{}, false
	}
	items := significantLexemes(a.lexemes)
	index := -1
	for i, item := range items {
		if item.Span.Start <= offset && offset < item.Span.End {
			index = i
			break
		}
	}
	if index < 2 || !isNameToken(items[index]) || items[index-1].Token.Kind != token.Period ||
		!isTriggerRowAlias(nameOrEmpty(a.Text, items[index-2])) {
		return Name{}, false
	}
	for _, trg := range discoverTriggers(a.Text, items) {
		if trg.bodyStart < index && index < trg.bodyEnd {
			return trg.relation, true
		}
	}
	return Name{}, false
}

func nameOrEmpty(text string, item lexeme) Name {
	name, _ := nameFromLexeme(text, item)
	return name
}
