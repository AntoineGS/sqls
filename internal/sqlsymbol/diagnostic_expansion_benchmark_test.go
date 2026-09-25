package sqlsymbol

import (
	"strconv"
	"strings"
	"testing"

	"github.com/sqls-server/sqls/dialect"
)

var diagnosticExpansionBenchSink []Finding

func diagnosticExpansionFixture(related int) string {
	var source strings.Builder
	for i := 0; i < related; i++ {
		name := strconv.Itoa(i)
		source.WriteString("SELECT ID FROM T WHERE ID = NULL;\n")
		source.WriteString("CREATE PROCEDURE P" + name + " AS DECLARE VARIABLE V INTEGER; BEGIN V = V + 1; END\n")
	}
	return source.String()
}

func BenchmarkDiagnosticExpansion(b *testing.B) {
	allOptional := DiagnosticOptions{Rules: map[string]string{
		codeLossyAssignment: "warning", codeReadBeforeAssignment: "warning",
		codeOutputNotAssigned: "warning", codeDeadStore: "hint", codeUnreachable: "hint",
		codeNullableAssignment: "warning", codeNullableNotIn: "warning", codeOuterJoinFilter: "warning",
	}}
	policies := []struct {
		name    string
		options DiagnosticOptions
	}{{"default", DiagnosticOptions{}}, {"all_optional", allOptional}}
	variant := dialect.DriverVariant{Driver: dialect.DatabaseDriverInterBase}
	for _, size := range []int{1, 10, 100} {
		text := diagnosticExpansionFixture(size)
		for _, policy := range policies {
			b.Run(policy.name+"/related_"+strconv.Itoa(size), func(b *testing.B) {
				b.ReportAllocs()
				b.SetBytes(int64(len(text)))
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					a, err := Analyze(text, variant)
					if err != nil {
						b.Fatal(err)
					}
					diagnosticExpansionBenchSink = a.DiagnosticsWithOptions(nil, policy.options)
				}
			})
		}
	}
	for _, fixture := range []struct {
		name string
		text string
	}{
		{"deep_expression", "SELECT " + strings.Repeat("(", 24) + "1" + strings.Repeat("+1)", 24) + ";"},
		{"large_cte_list", func() string {
			var ctes strings.Builder
			ctes.WriteString("WITH ")
			for i := 0; i < 100; i++ {
				if i > 0 {
					ctes.WriteString(", ")
				}
				ctes.WriteString("Q" + strconv.Itoa(i) + " AS (SELECT " + strconv.Itoa(i) + " AS ID)")
			}
			ctes.WriteString(" SELECT ID FROM Q99;")
			return ctes.String()
		}()},
	} {
		for _, policy := range policies {
			b.Run(policy.name+"/"+fixture.name, func(b *testing.B) {
				b.ReportAllocs()
				b.SetBytes(int64(len(fixture.text)))
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					a, err := Analyze(fixture.text, variant)
					if err != nil {
						b.Fatal(err)
					}
					diagnosticExpansionBenchSink = a.DiagnosticsWithOptions(nil, policy.options)
				}
			})
		}
	}
}
