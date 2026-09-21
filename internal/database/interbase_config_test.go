package database

import (
	"reflect"
	"strings"
	"testing"

	"github.com/sqls-server/sqls/dialect"
)

func TestInterBaseConfigValidatesCharset(t *testing.T) {
	accepted := []struct {
		param string
		want  string
	}{
		{param: "", want: "UTF8"},
		{param: "UTF8", want: "UTF8"},
		{param: "utf8", want: "UTF8"},
		{param: "WIN1250", want: "WIN1250"},
		{param: "win1250", want: "WIN1250"},
		{param: "WIN1252", want: "WIN1252"},
		{param: "win1252", want: "WIN1252"},
		{param: "ISO8859_1", want: "ISO8859_1"},
		{param: "iso8859_1", want: "ISO8859_1"},
		{param: "ASCII", want: "ASCII"},
		{param: "ascii", want: "ASCII"},
		{param: "  utf8  ", want: "UTF8"},
	}
	for _, test := range accepted {
		t.Run("accepts "+test.param, func(t *testing.T) {
			cfg := &DBConfig{
				Driver: dialect.DatabaseDriverInterBase,
				Path:   "/tmp/example.ib",
				User:   "alice",
			}
			if test.param != "" {
				cfg.Params = map[string]string{"charset": test.param}
			}
			if err := cfg.Validate(); err != nil {
				t.Fatalf("DBConfig.Validate() error = %v", err)
			}
			got, err := interBaseCharset(cfg)
			if err != nil || got != test.want {
				t.Fatalf("interBaseCharset(%q) = (%q, %v), want (%q, nil)", test.param, got, err, test.want)
			}
		})
	}

	rejected := []struct {
		name   string
		params map[string]string
		want   string
	}{
		{name: "unsupported value", params: map[string]string{"charset": "latin1"}, want: "charset"},
		{name: "hyphenated spelling is not the catalog name", params: map[string]string{"charset": "UTF-8"}, want: "charset"},
		{name: "conflicting duplicates", params: map[string]string{"charset": "UTF8", "CHARSET": "ASCII"}, want: "conflicting"},
	}
	for _, test := range rejected {
		t.Run("rejects "+test.name, func(t *testing.T) {
			cfg := &DBConfig{
				Driver: dialect.DatabaseDriverInterBase,
				Path:   "/tmp/example.ib",
				User:   "alice",
				Passwd: "do-not-log",
				Params: test.params,
			}
			err := cfg.Validate()
			if err == nil {
				t.Fatal("DBConfig.Validate() returned nil error")
			}
			if !strings.Contains(strings.ToLower(err.Error()), test.want) {
				t.Fatalf("DBConfig.Validate() error = %q, want mention %q", err, test.want)
			}
			if strings.Contains(err.Error(), cfg.Passwd) {
				t.Fatalf("DBConfig.Validate() leaked password: %q", err)
			}
		})
	}
}

func TestInterBaseCharsetMatchesDriverAllowlist(t *testing.T) {
	// interbase-go normalizeCharset (interbase.go:383-392) accepts exactly these
	// five names, and the empty value defaults to UTF8. That normalizer is
	// unexported and only builds with cgo, so sqls keeps this copy for untagged
	// validation. The tagged TestInterBaseCharsetAllowlistMatchesDriverNormalizer
	// proves the two have not drifted.
	want := []string{"UTF8", "WIN1250", "WIN1252", "ISO8859_1", "ASCII"}
	if !reflect.DeepEqual(interBaseCharsets, want) {
		t.Fatalf("interBaseCharsets = %#v, want %#v", interBaseCharsets, want)
	}
	if got, err := interBaseCharset(&DBConfig{}); err != nil || got != "UTF8" {
		t.Fatalf("interBaseCharset(empty) = (%q, %v), want (\"UTF8\", nil)", got, err)
	}
}
