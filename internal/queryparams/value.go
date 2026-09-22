package queryparams

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Value is one named parameter's wire-format value, as submitted by the
// editor client. Type selects which InterBase wire type Convert produces;
// Value is always the string spelling of that type, never the client's
// native JSON type.
type Value struct {
	Name  string `json:"name"`
	Type  string `json:"type"`
	Value string `json:"value"`
}

const (
	typeText      = "text"
	typeInteger   = "integer"
	typeNumber    = "number"
	typeDate      = "date"
	typeTimestamp = "timestamp"
	typeBoolean   = "boolean"
	typeNull      = "null"
)

var (
	integerPattern   = regexp.MustCompile(`^[+-]?[0-9]+$`)
	numberPattern    = regexp.MustCompile(`^[+-]?(\d+(\.\d+)?|\.\d+)([eE][+-]?\d+)?$`)
	timestampPattern = regexp.MustCompile(`^\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}(\.\d{1,9})?$`)
)

// Convert parses value.Value under value.Type into the Go native type that
// carries it as a driver argument: string for text, int64 for integer,
// float64 for number, time.Time (UTC) for date/timestamp, bool for boolean,
// and nil for null. It returns an error identifying the parameter's name and
// type, never its input value, so a rejected submission cannot leak into a
// diagnostic message.
func Convert(value Value) (any, error) {
	switch value.Type {
	case typeText:
		return value.Value, nil
	case typeInteger:
		return convertInteger(value)
	case typeNumber:
		return convertNumber(value)
	case typeDate:
		return convertDate(value)
	case typeTimestamp:
		return convertTimestamp(value)
	case typeBoolean:
		return convertBoolean(value)
	case typeNull:
		return convertNull(value)
	default:
		return nil, fmt.Errorf("queryparams: parameter %q has unsupported type %q", value.Name, value.Type)
	}
}

func invalidValueErr(value Value) error {
	return fmt.Errorf("queryparams: parameter %q has an invalid %s value", value.Name, value.Type)
}

func convertInteger(value Value) (any, error) {
	trimmed := strings.TrimSpace(value.Value)
	if !integerPattern.MatchString(trimmed) {
		return nil, invalidValueErr(value)
	}
	n, err := strconv.ParseInt(trimmed, 10, 64)
	if err != nil {
		return nil, invalidValueErr(value)
	}
	return n, nil
}

func convertNumber(value Value) (any, error) {
	trimmed := strings.TrimSpace(value.Value)
	if !numberPattern.MatchString(trimmed) {
		return nil, invalidValueErr(value)
	}
	f, err := strconv.ParseFloat(trimmed, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return nil, invalidValueErr(value)
	}
	return f, nil
}

func convertDate(value Value) (any, error) {
	trimmed := strings.TrimSpace(value.Value)
	t, err := time.ParseInLocation("2006-01-02", trimmed, time.UTC)
	if err != nil {
		return nil, invalidValueErr(value)
	}
	return t, nil
}

func convertTimestamp(value Value) (any, error) {
	trimmed := strings.TrimSpace(value.Value)
	if !timestampPattern.MatchString(trimmed) {
		return nil, invalidValueErr(value)
	}
	t, err := time.ParseInLocation("2006-01-02 15:04:05.999999999", trimmed, time.UTC)
	if err != nil {
		return nil, invalidValueErr(value)
	}
	return t, nil
}

func convertBoolean(value Value) (any, error) {
	switch strings.TrimSpace(value.Value) {
	case "true":
		return true, nil
	case "false":
		return false, nil
	default:
		return nil, invalidValueErr(value)
	}
}

func convertNull(value Value) (any, error) {
	if value.Value != "" {
		return nil, invalidValueErr(value)
	}
	return nil, nil
}

// Bind resolves batch's parameters against values and produces one positional
// argument slice per statement, ready to hand to a SQL driver in place of
// batch.Statements[i].Keys order. It canonicalizes and validates every
// value's name first — no missing, extra, or duplicate case-folded name — then
// converts every value, and only after both stages succeed does it build the
// per-statement argument slices. An invalid value anywhere in values, even
// one used only by a later statement, fails the whole batch: Bind never
// returns a partial set of usable arguments.
func Bind(batch Batch, values []Value) ([][]any, error) {
	required := make(map[string]string, len(batch.Parameters))
	for _, p := range batch.Parameters {
		required[p.Key] = p.Name
	}

	keys := make([]string, len(values))
	seen := make(map[string]bool, len(values))
	for i, v := range values {
		key := strings.ToUpper(v.Name)
		if _, ok := required[key]; !ok {
			return nil, fmt.Errorf("queryparams: parameter %q is not used by this query", v.Name)
		}
		if seen[key] {
			return nil, fmt.Errorf("queryparams: parameter %q is bound more than once", v.Name)
		}
		seen[key] = true
		keys[i] = key
	}
	for key, name := range required {
		if !seen[key] {
			return nil, fmt.Errorf("queryparams: parameter %q has no value", name)
		}
	}

	converted := make(map[string]any, len(values))
	for i, v := range values {
		value, err := Convert(v)
		if err != nil {
			return nil, err
		}
		converted[keys[i]] = value
	}

	args := make([][]any, len(batch.Statements))
	for i, stmt := range batch.Statements {
		stmtArgs := make([]any, len(stmt.Keys))
		for j, key := range stmt.Keys {
			stmtArgs[j] = converted[key]
		}
		args[i] = stmtArgs
	}
	return args, nil
}
