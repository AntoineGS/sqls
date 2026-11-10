package language

import (
	"regexp"
	"strconv"
	"strings"
)

// StatementError is one rejection returned by a database server while
// preparing a statement. Line and Column are 1-based within the statement;
// zero means that the server did not report a position.
type StatementError struct {
	SQLCode      int
	Message      string
	Line, Column int
	Token        string
}

var (
	sqlErrorCodeLine = regexp.MustCompile(`(?i)^SQL error code = -?\d+$`)
	lineCharPosition = regexp.MustCompile(`(?i)line\s+(\d+),\s*char\s+(\d+)`)
	atLinePosition   = regexp.MustCompile(`(?i)at\s+line\s+(\d+),\s*column\s+(\d+)`)
)

// ParseServerMessage extracts a statement-relative position and a trailing
// token/name line from raw multi-line server message text. The original
// message is retained in StatementError.Message for host logging and
// fallback display.
func ParseServerMessage(sqlCode int, message string) StatementError {
	parsed := StatementError{SQLCode: sqlCode, Message: message}
	lines := cleanServerMessageLines(message, &parsed)
	if len(lines) >= 2 && !strings.EqualFold(lines[len(lines)-1], "Unexpected end of command") {
		parsed.Token = lines[len(lines)-1]
	}
	return parsed
}

func cleanServerMessageLines(message string, parsed *StatementError) []string {
	lines := make([]string, 0)
	for _, rawLine := range strings.Split(message, "\n") {
		line := strings.TrimSpace(rawLine)
		line = strings.TrimSpace(strings.TrimPrefix(line, "-"))
		if line == "" || strings.EqualFold(line, "Dynamic SQL Error") || sqlErrorCodeLine.MatchString(line) {
			continue
		}

		if match := lineCharPosition.FindStringSubmatchIndex(line); match != nil {
			parsed.Line = parsePositionNumber(line[match[2]:match[3]])
			parsed.Column = parsePositionNumber(line[match[4]:match[5]])
			line = stripPosition(line, match[0], match[1])
		} else if match := atLinePosition.FindStringSubmatchIndex(line); match != nil {
			parsed.Line = parsePositionNumber(line[match[2]:match[3]])
			parsed.Column = parsePositionNumber(line[match[4]:match[5]])
			line = stripPosition(line, match[0], match[1])
		}
		if line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

func parsePositionNumber(value string) int {
	n, err := strconv.Atoi(value)
	if err != nil {
		return 0
	}
	return n
}

func stripPosition(line string, start, end int) string {
	prefix := line[:start]
	suffix := line[end:]
	if strings.HasSuffix(prefix, " - ") {
		prefix = strings.TrimSuffix(prefix, " - ")
	} else if strings.HasSuffix(strings.TrimRight(prefix, " \t"), ",") {
		prefix = strings.TrimRight(strings.TrimSuffix(strings.TrimRight(prefix, " \t"), ","), " \t")
	}
	line = strings.TrimSpace(prefix + suffix)
	if strings.HasSuffix(line, ",") {
		line = strings.TrimSuffix(line, ",")
	}
	return strings.TrimSpace(line)
}

func serverMessageText(serverErr *StatementError) string {
	if serverErr == nil {
		return ""
	}
	parsed := StatementError{}
	lines := cleanServerMessageLines(serverErr.Message, &parsed)
	if len(lines) > 0 {
		message := lines[0]
		if len(lines) > 1 {
			message += ": " + strings.Join(lines[1:], " ")
		}
		return message
	}
	if message := strings.TrimSpace(serverErr.Message); message != "" {
		return message
	}
	return "SQL error " + strconv.Itoa(serverErr.SQLCode)
}
