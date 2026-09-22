package database

import (
	"context"
	"errors"
	"testing"
)

func TestMockCapabilityRepositoryRecordsCalls(t *testing.T) {
	repo := NewMockCapabilityRepository()
	repo.MockObjectDDL = func(_ context.Context, kind ObjectKind, name string) (string, error) {
		return "CREATE " + string(kind) + " " + name, nil
	}
	repo.MockExplainPlan = func(context.Context, string) (string, error) {
		return "PLAN (CITY NATURAL)", nil
	}

	if _, err := repo.ObjectDDL(context.Background(), ObjectKindProcedure, "MYPROC"); err != nil {
		t.Fatal("ObjectDDL:", err)
	}
	if _, err := repo.ExplainPlan(context.Background(), "SELECT 1"); err != nil {
		t.Fatal("ExplainPlan:", err)
	}

	ddlCalls := repo.ObjectDDLCalls()
	if len(ddlCalls) != 1 || ddlCalls[0].Kind != ObjectKindProcedure || ddlCalls[0].Name != "MYPROC" {
		t.Errorf("ObjectDDLCalls() = %+v, want one {procedure MYPROC} call", ddlCalls)
	}
	if got := repo.ExplainPlanCalls(); len(got) != 1 || got[0] != "SELECT 1" {
		t.Errorf("ExplainPlanCalls() = %v, want [\"SELECT 1\"]", got)
	}
}

func TestMockCapabilityRepositoryDefaultsToObjectNotFound(t *testing.T) {
	// The zero-configuration mock must not answer with a fabricated CREATE
	// statement: a test that forgets to set MockObjectDDL should see the
	// not-found branch, which renders nothing, rather than invented DDL.
	repo := NewMockCapabilityRepository()
	if _, err := repo.ObjectDDL(context.Background(), ObjectKindTable, "CITY"); !errors.Is(err, ErrObjectNotFound) {
		t.Errorf("default ObjectDDL error = %v, want ErrObjectNotFound", err)
	}
}
