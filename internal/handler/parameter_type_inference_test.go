package handler

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sqls-server/sqls/internal/database"
	"github.com/sqls-server/sqls/internal/queryparams"
)

func compileInferenceBatch(t *testing.T, sql string, dialect int) queryparams.Batch {
	t.Helper()
	batch, err := queryparams.Compile(sql, dialect)
	if err != nil {
		t.Fatal(err)
	}
	return batch
}

func TestInferParameterTypesMatchesCompatibleRepeatedNames(t *testing.T) {
	batch := compileInferenceBatch(t, "SELECT :id FROM T; SELECT :ID FROM U", 3)
	result, err := inferParameterTypes(context.Background(), batch, func(_ context.Context, sql string) ([]database.InputDescriptor, error) {
		if strings.Contains(sql, "FROM T") {
			return []database.InputDescriptor{{Kind: "INTEGER"}}, nil
		}
		return []database.InputDescriptor{{Kind: "BIGINT"}}, nil
	}, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(result) != 1 || result[0].InferredType != "integer" {
		t.Fatalf("compatible repeated parameter = %+v, want one integer inference", result)
	}
}

func TestInferParameterTypesLeavesConflictingNameUninferred(t *testing.T) {
	batch := compileInferenceBatch(t, "SELECT :id FROM T; SELECT :ID FROM U", 3)
	result, err := inferParameterTypes(context.Background(), batch, func(_ context.Context, sql string) ([]database.InputDescriptor, error) {
		if strings.Contains(sql, "FROM T") {
			return []database.InputDescriptor{{Kind: "VARCHAR"}}, nil
		}
		return []database.InputDescriptor{{Kind: "INTEGER"}}, nil
	}, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(result) != 1 || result[0].InferredType != "" || result[0].DatabaseType != "" {
		t.Fatalf("conflicting descriptor should use picker: %+v", result)
	}
}

func TestInferParameterTypesRequiresExactDescriptorCount(t *testing.T) {
	batch := compileInferenceBatch(t, "SELECT :a, :b FROM T", 3)
	result, err := inferParameterTypes(context.Background(), batch, func(context.Context, string) ([]database.InputDescriptor, error) {
		return []database.InputDescriptor{{Kind: "INTEGER"}}, nil
	}, 3)
	if err != nil {
		t.Fatal(err)
	}
	for _, parameter := range result {
		if parameter.InferredType != "" {
			t.Errorf("parameter %+v inferred despite descriptor count mismatch", parameter)
		}
	}
}

func TestInferParameterTypesFailedPrepareOnlyAffectsItsStatement(t *testing.T) {
	batch := compileInferenceBatch(t, "SELECT :safe FROM T; SELECT :unknown FROM U", 3)
	prepareErr := errors.New("prepare failed")
	result, err := inferParameterTypes(context.Background(), batch, func(_ context.Context, sql string) ([]database.InputDescriptor, error) {
		if strings.Contains(sql, "FROM U") {
			return nil, prepareErr
		}
		return []database.InputDescriptor{{Kind: "INTEGER"}}, nil
	}, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(result) != 2 || result[0].Key != "SAFE" || result[0].InferredType != "integer" || result[1].Key != "UNKNOWN" || result[1].InferredType != "" {
		t.Fatalf("prepare failure fallback = %+v, want safe inferred and unknown untyped", result)
	}
}

func TestInferParameterTypesDoesNotDescribeBatchWithoutParameters(t *testing.T) {
	batch := compileInferenceBatch(t, "SELECT 1 FROM T", 3)
	calls := 0
	result, err := inferParameterTypes(context.Background(), batch, func(context.Context, string) ([]database.InputDescriptor, error) {
		calls++
		return nil, nil
	}, 3)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 0 || len(result) != 0 {
		t.Fatalf("no-parameter inference made %d calls and returned %+v", calls, result)
	}
}

func TestInferParameterTypesPreservesExactScaledDecimalString(t *testing.T) {
	batch := compileInferenceBatch(t, "SELECT :amount FROM T", 3)
	result, err := inferParameterTypes(context.Background(), batch, func(context.Context, string) ([]database.InputDescriptor, error) {
		return []database.InputDescriptor{{Kind: "BIGINT", Subtype: 2, Scale: -2, Precision: 18}}, nil
	}, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(result) != 1 || result[0].InferredType != "text" || result[0].DatabaseType != "DECIMAL(18,2)" {
		t.Fatalf("scaled decimal inference = %+v, want text / DECIMAL(18,2)", result)
	}
	args, err := queryparams.Bind(batch, []queryparams.Value{{Name: "amount", Type: result[0].InferredType, Value: "9007199254740993.25"}})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(args, [][]any{{"9007199254740993.25"}}) {
		t.Fatalf("bound exact decimal = %#v, want original string", args)
	}
}

func TestInferParameterTypesLeavesOctetsAndTimeToPicker(t *testing.T) {
	batch := compileInferenceBatch(t, "SELECT :binary, :time, :blob FROM T", 3)
	result, err := inferParameterTypes(context.Background(), batch, func(context.Context, string) ([]database.InputDescriptor, error) {
		return []database.InputDescriptor{{Kind: "VARCHAR OCTETS"}, {Kind: "TIME"}, {Kind: "BLOB", Subtype: 0}}, nil
	}, 3)
	if err != nil {
		t.Fatal(err)
	}
	for _, parameter := range result {
		if parameter.InferredType != "" || parameter.DatabaseType != "" {
			t.Errorf("unsupported descriptor inferred parameter: %+v", parameter)
		}
	}
}

func TestInferParameterTypesUsesCharacterCharsetIDToRejectOctets(t *testing.T) {
	batch := compileInferenceBatch(t, "SELECT :varchar_octets, :char_octets, :none_charset, :collated_utf8 FROM T", 3)
	descriptors := []database.InputDescriptor{
		{Kind: "VARCHAR", Subtype: 1},
		{Kind: "CHAR", Subtype: 0x101},
		{Kind: "VARCHAR", Subtype: 0},
		{Kind: "CHAR", Subtype: 0x104},
	}
	result, err := inferParameterTypes(context.Background(), batch, func(context.Context, string) ([]database.InputDescriptor, error) {
		return descriptors, nil
	}, 3)
	if err != nil {
		t.Fatal(err)
	}
	for _, index := range []int{0, 1} {
		if result[index].InferredType != "" || result[index].DatabaseType != "" {
			t.Errorf("OCTETS descriptor %+v inferred a type, want picker fallback", result[index])
		}
	}
	for _, index := range []int{2, 3} {
		if result[index].InferredType != "text" {
			t.Errorf("ordinary character descriptor %+v inferredType = %q, want text", result[index], result[index].InferredType)
		}
	}
}

func TestInferParameterTypesDoesNotGuessDialectOneDecimal(t *testing.T) {
	batch := compileInferenceBatch(t, "SELECT :amount FROM T", 1)
	result, err := inferParameterTypes(context.Background(), batch, func(context.Context, string) ([]database.InputDescriptor, error) {
		return []database.InputDescriptor{{Kind: "BIGINT", Subtype: 2, Scale: -2, Precision: 18}}, nil
	}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(result) != 1 || result[0].InferredType != "" || result[0].DatabaseType != "" {
		t.Fatalf("dialect 1 decimal inference = %+v, want picker fallback", result)
	}
}

func TestInferParameterTypesStatementDeadlineFallsBackLocally(t *testing.T) {
	batch := compileInferenceBatch(t, "SELECT :late FROM T; SELECT :safe FROM U", 3)
	result, err := inferParameterTypes(context.Background(), batch, func(ctx context.Context, sql string) ([]database.InputDescriptor, error) {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > parameterDescriptionTimeout || time.Until(deadline) <= 0 {
			t.Errorf("DescribeInputs context deadline = %v, present = %t", deadline, ok)
		}
		if strings.Contains(sql, "FROM T") {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return []database.InputDescriptor{{Kind: "INTEGER"}}, nil
	}, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(result) != 2 || result[0].InferredType != "" || result[1].InferredType != "integer" {
		t.Fatalf("statement deadline fallback = %+v, want late untyped and safe inferred", result)
	}
}

func TestInferParameterTypesMapsNativeWireTypeFamilies(t *testing.T) {
	batch := compileInferenceBatch(t, "SELECT :ratio, :day, :moment, :enabled FROM T", 3)
	descriptors := []database.InputDescriptor{
		{Kind: "DOUBLE PRECISION"},
		{Kind: "DATE"},
		{Kind: "TIMESTAMP"},
		{Kind: "BOOLEAN"},
	}
	result, err := inferParameterTypes(context.Background(), batch, func(context.Context, string) ([]database.InputDescriptor, error) {
		return descriptors, nil
	}, 3)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"number", "date", "timestamp", "boolean"}
	for i, parameter := range result {
		if parameter.InferredType != want[i] {
			t.Errorf("parameter %q inferredType = %q, want %q", parameter.Name, parameter.InferredType, want[i])
		}
	}
}

func TestInferParameterTypesUsesExecutableSelectsForOutputTargets(t *testing.T) {
	selection := "FOR SELECT value FROM T WHERE id = :id INTO :output DO;"
	batch := compileInferenceBatch(t, queryparams.ExecutableSelects(selection), 3)
	if len(batch.Parameters) != 1 || batch.Parameters[0].Key != "ID" {
		t.Fatalf("compiled normalized parameters = %+v, want only ID", batch.Parameters)
	}
	var described string
	result, err := inferParameterTypes(context.Background(), batch, func(_ context.Context, sql string) ([]database.InputDescriptor, error) {
		described = sql
		return []database.InputDescriptor{{Kind: "INTEGER"}}, nil
	}, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(result) != 1 || result[0].Key != "ID" || result[0].InferredType != "integer" {
		t.Fatalf("normalized inference = %+v, want only ID inferred", result)
	}
	if strings.Contains(described, "INTO") || strings.Contains(described, "OUTPUT") {
		t.Errorf("describer received output target SQL %q", described)
	}
}
