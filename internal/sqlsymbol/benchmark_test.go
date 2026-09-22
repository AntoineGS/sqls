package sqlsymbol

import (
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/sqls-server/sqls/dialect"
)

func BenchmarkProcedureSymbols(b *testing.B) {
	text := "ALTER PROCEDURE p AS DECLARE VARIABLE v INTEGER; BEGIN\n" +
		strings.Repeat("v=v+1;\n", 1000) + "END"
	dv := dialect.DriverVariant{Driver: dialect.DatabaseDriverInterBase}
	pos := strings.Index(text, "v=v+1")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		a, err := Analyze(text, dv)
		if err != nil {
			b.Fatal(err)
		}
		r := a.Resolve(pos)
		if r.Symbol == nil {
			b.Fatal("variable not resolved")
		}
		a.References(r.Symbol, true)
	}
}

func BenchmarkProcedureSymbolsScaling(b *testing.B) {
	dv := dialect.DriverVariant{Driver: dialect.DatabaseDriverInterBase}
	for _, uses := range []int{250, 500, 1000} {
		text := "ALTER PROCEDURE p AS DECLARE VARIABLE v INTEGER; BEGIN\n" +
			strings.Repeat("v=v+1;\n", uses) + "END"
		pos := strings.Index(text, "v=v+1")
		b.Run("uses_"+strconv.Itoa(uses), func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				a, err := Analyze(text, dv)
				if err != nil {
					b.Fatal(err)
				}
				r := a.Resolve(pos)
				if r.Symbol == nil {
					b.Fatal("variable not resolved")
				}
				a.References(r.Symbol, true)
			}
		})
	}
}

func BenchmarkLocalInterBaseExample(b *testing.B) {
	path := os.Getenv("SQLS_SYMBOL_EXAMPLE")
	if path == "" {
		b.Skip("SQLS_SYMBOL_EXAMPLE not set")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		b.Fatal(err)
	}
	text := string(data)
	needle := "IMPORTEXTERNALORDER_EMPLYID(:HEADEREMPLYID_TEMP"
	call := strings.Index(text, needle)
	if call < 0 {
		b.Fatal("expected example call not found")
	}
	pos := call + len("IMPORTEXTERNALORDER_EMPLYID(:")
	dv := dialect.DriverVariant{Driver: dialect.DatabaseDriverInterBase, Variant: dialect.SQLVariantInterBase1}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		a, err := Analyze(text, dv)
		if err != nil {
			b.Fatal(err)
		}
		r := a.Resolve(pos)
		if r.Symbol == nil {
			b.Fatal("HEADEREMPLYID_TEMP not resolved")
		}
		a.References(r.Symbol, true)
	}
}
