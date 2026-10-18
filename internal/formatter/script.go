package formatter

import (
	"sort"
	"strings"

	"github.com/sqls-server/sqls/ast"
	"github.com/sqls-server/sqls/dialect"
	"github.com/sqls-server/sqls/internal/config"
	"github.com/sqls-server/sqls/internal/lsp"
	"github.com/sqls-server/sqls/parser"
)

// formatSource formats src. A script that changes its terminator with
// SET TERM is formatted one statement at a time, each followed by the
// terminator in effect, so that "END^" and "SET TERM ; ^" keep their shape.
func formatSource(src string, params lsp.DocumentFormattingParams, cfg *config.Config, d dialect.Dialect) (string, error) {
	parts, script := splitScript(src)
	if !script {
		return formatStatements(src, params, cfg, d)
	}
	keyword := func(word string) string {
		if cfg.LowercaseKeywords {
			return strings.ToLower(word)
		}
		return word
	}
	lines := []string{}
	for _, part := range parts {
		if part.setTerm {
			if comment := strings.TrimSpace(part.text); comment != "" {
				lines = append(lines, comment)
			}
			// TERM is an identifier to the lexer; it keeps its spelling.
			lines = append(lines, keyword("SET")+" "+part.termWord+" "+part.term+" "+part.previous)
			continue
		}
		body, err := formatStatements(part.text, params, cfg, d)
		if err != nil {
			return "", err
		}
		if part.term != "" && endsInLineComment(body) {
			body += "\n"
		}
		if body += part.term; strings.TrimSpace(body) != "" {
			lines = append(lines, body)
		}
	}
	return strings.Join(lines, "\n"), nil
}

func formatStatements(src string, params lsp.DocumentFormattingParams, cfg *config.Config, d dialect.Dialect) (string, error) {
	parsed, err := parser.ParseWithDialect(src, d)
	if err != nil {
		return "", err
	}
	env := &formatEnvironment{options: params.Options, commentBreaks: commentLineBreaks(parsed)}
	rendered := Eval(parsed, env).Render(&ast.RenderOptions{LowerCase: cfg.LowercaseKeywords})
	return tidyLines(rendered, indentUnit(params.Options)), nil
}

func indentUnit(options lsp.FormattingOptions) string {
	if !options.InsertSpaces {
		return "\t"
	}
	return strings.Repeat(" ", int(options.TabSize))
}

type scriptPart struct {
	text     string
	term     string // terminator ending text, or the new one of SET TERM
	previous string // terminator in effect before SET TERM
	termWord string // TERM as written
	setTerm  bool
}

// splitScript splits src at its statement terminators, following SET TERM.
// It reports false when src contains no SET TERM.
func splitScript(src string) ([]scriptPart, bool) {
	spans := scanSpans(src)
	term, start, script := ";", 0, false
	// fresh: only whitespace and comments since start
	fresh := true
	parts := []scriptPart{}
	for _, s := range spans {
		if s.kind != spanCode {
			continue
		}
		for i := s.from; i < s.to; {
			if fresh && !isSpace(src[i]) {
				if next, word, end, ok := matchSetTerm(src, i, min(s.to, i+64), term); ok {
					parts = append(parts, scriptPart{text: src[start:i], term: next, previous: term, termWord: word, setTerm: true})
					term, start, i, script = next, end, end, true
					continue
				}
				fresh = false
			}
			if strings.HasPrefix(src[i:s.to], term) {
				parts = append(parts, scriptPart{text: src[start:i], term: term})
				start = i + len(term)
				i, fresh = start, true
				continue
			}
			i++
		}
	}
	if strings.TrimSpace(src[start:]) != "" {
		parts = append(parts, scriptPart{text: src[start:]})
	}
	return parts, script
}

// matchSetTerm matches "SET TERM <new> <term>" at src[i:limit], returning
// the new terminator, TERM as written and the end of the match.
func matchSetTerm(src string, i, limit int, term string) (string, string, int, bool) {
	words := strings.Fields(src[i:limit])
	if len(words) < 3 || !strings.EqualFold(words[0], "SET") || !strings.EqualFold(words[1], "TERM") {
		return "", "", 0, false
	}
	at := i + strings.Index(src[i:limit], words[2])
	if strings.HasSuffix(words[2], term) && len(words[2]) > len(term) {
		return strings.TrimSuffix(words[2], term), words[1], at + len(words[2]), true
	}
	if len(words) < 4 || !strings.HasPrefix(words[3], term) {
		return "", "", 0, false
	}
	end := at + len(words[2])
	end += strings.Index(src[end:limit], term) + len(term)
	return words[2], words[1], end, true
}

func isSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }

func endsInLineComment(text string) bool {
	spans := scanSpans(strings.TrimRight(text, " \t"))
	return len(spans) > 0 && spans[len(spans)-1].kind == spanLineComment
}

// tidyLines indents each line by its BEGIN ... END depth, trims trailing
// whitespace and drops blank lines. It changes whitespace outside string
// literals and comments only.
func tidyLines(text, unit string) string {
	spans := scanSpans(text)
	kindAt := func(pos int) spanKind {
		if i := sort.Search(len(spans), func(i int) bool { return spans[i].to > pos }); i < len(spans) && spans[i].from <= pos {
			return spans[i].kind
		}
		return spanCode
	}
	words := blockWords(text, spans)
	var blocks []bool // true for BEGIN, false for CASE
	depth, next := 0, 0
	lines := []string{}
	for start := 0; start <= len(text); {
		end := strings.IndexByte(text[start:], '\n')
		if end < 0 {
			end = len(text)
		} else {
			end += start
		}
		line := text[start:end]
		inside := start > 0 && kindAt(start-1) != spanCode && kindAt(start-1) != spanLineComment
		first := start + len(line) - len(strings.TrimLeft(line, " \t"))
		lineDepth := depth
		for ; next < len(words) && words[next].pos < end; next++ {
			w := words[next]
			switch w.word {
			case "BEGIN":
				blocks = append(blocks, true)
				depth++
			case "CASE":
				blocks = append(blocks, false)
			case "END":
				if len(blocks) > 0 {
					if blocks[len(blocks)-1] {
						depth--
						if w.pos == first {
							lineDepth = depth
						}
					}
					blocks = blocks[:len(blocks)-1]
				}
			}
		}
		trimmed := strings.TrimRight(line, " \t")
		if len(trimmed) < len(line) && kindAt(start+len(trimmed)) != spanCode {
			trimmed = line
		}
		switch {
		case inside:
			lines = append(lines, trimmed)
		case strings.TrimSpace(trimmed) != "":
			lines = append(lines, strings.Repeat(unit, max(lineDepth, 0))+trimmed)
		}
		start = end + 1
	}
	return strings.Join(lines, "\n")
}

type blockWord struct {
	pos  int
	word string
}

// blockWords lists BEGIN, CASE and END words in code, not after a period.
func blockWords(text string, spans []span) []blockWord {
	words := []blockWord{}
	for _, s := range spans {
		if s.kind != spanCode {
			continue
		}
		for i := s.from; i < s.to; {
			if !isWordByte(text[i]) {
				i++
				continue
			}
			j := i
			for j < s.to && isWordByte(text[j]) {
				j++
			}
			word := strings.ToUpper(text[i:j])
			if (word == "BEGIN" || word == "CASE" || word == "END") && (i == 0 || text[i-1] != '.') {
				words = append(words, blockWord{pos: i, word: word})
			}
			i = j
		}
	}
	return words
}

func isWordByte(c byte) bool {
	return c == '_' || c == '$' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= 0x80
}

type spanKind int

const (
	spanCode spanKind = iota
	spanQuoted
	spanLineComment
	spanBlockComment
)

type span struct {
	kind     spanKind
	from, to int
}

// scanSpans splits text into code, quoted strings or identifiers, and
// comments.
func scanSpans(text string) []span {
	spans := []span{}
	add := func(kind spanKind, from, to int) {
		if from >= to {
			return
		}
		if n := len(spans); n > 0 && kind == spanCode && spans[n-1].kind == spanCode {
			spans[n-1].to = to
			return
		}
		spans = append(spans, span{kind, from, to})
	}
	for i := 0; i < len(text); {
		switch {
		case text[i] == '\'' || text[i] == '"':
			quote, j := text[i], i+1
			for j < len(text) {
				if text[j] == quote {
					if j+1 < len(text) && text[j+1] == quote {
						j += 2
						continue
					}
					j++
					break
				}
				j++
			}
			add(spanQuoted, i, j)
			i = j
		case strings.HasPrefix(text[i:], "--"):
			j := strings.IndexAny(text[i:], "\r\n")
			if j < 0 {
				j = len(text) - i
			}
			add(spanLineComment, i, i+j)
			i += j
		case strings.HasPrefix(text[i:], "/*"):
			j := strings.Index(text[i+2:], "*/")
			if j < 0 {
				j = len(text)
			} else {
				j += i + 4
			}
			add(spanBlockComment, i, j)
			i = j
		default:
			add(spanCode, i, i+1)
			i++
		}
	}
	return spans
}
