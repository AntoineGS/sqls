package dialect

type postgreSQLDialect struct {
	GenericSQLDialect
}

func (*postgreSQLDialect) PreservesQuotedStringEscapes() bool  { return true }
func (*postgreSQLDialect) ScansWholeDelimitedIdentifier() bool { return true }
func (*postgreSQLDialect) SupportsEscapeStringLiterals() bool  { return true }
func (*postgreSQLDialect) SupportsDollarQuotedStrings() bool   { return true }

type duckDBDialect struct {
	postgreSQLDialect
}

func (*duckDBDialect) MatchKeyword(word string) KeywordKind {
	if kind, ok := match[word]; ok {
		return kind
	}
	for _, keyword := range duckdbKeywords {
		if word == keyword {
			return Matched
		}
	}
	return Unmatched
}
