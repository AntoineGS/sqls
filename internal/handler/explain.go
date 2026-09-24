package handler

import (
	"bytes"
	"context"
	"fmt"
	"strings"

	"github.com/sqls-server/sqls/dialect"
	"github.com/sqls-server/sqls/internal/database"
	"github.com/sqls-server/sqls/internal/lsp"
	"github.com/sqls-server/sqls/internal/queryparams"
)

const (
	explainRefusal = "Explain supports SELECT, INSERT, UPDATE, DELETE and EXECUTE PROCEDURE\n" +
		"statements; got %s."

	// Two sentences, not one: "the plan was empty" and "nothing ran" are
	// independent facts, and the driver README is explicit that an empty plan
	// never implies execution.
	explainNoPlan = "No plan text. The statement prepared successfully and InterBase reported no plan\n" +
		"for it. Nothing was executed."

	explainPreparedOnly = "prepared only; nothing was inserted, updated or deleted"

	explainArrayColumn = "Explain is unavailable for this statement: it returns an array column, and the\n" +
		"prepare path sqls uses rejects array results. Remove the array column from the\n" +
		"select list to see its plan."
)

// explainMutatingTypes are the QueryExecType results that are admitted but are
// not queries. Everything QueryExecType reports as a query is admitted too;
// everything else is refused.
var explainMutatingTypes = map[string]bool{
	"INSERT":  true,
	"UPDATE":  true,
	"DELETE":  true,
	"EXECUTE": true,
}

// explainArrayColumnMarker is matched in the driver's error text because the
// driver exposes no typed error for it: output-type validation rejects array
// results before a plan exists (native.c:4718, reached because the pooled
// prepare path passes allow_arrays = 0). The raw message is still shown, so
// matching only adds an explanation and never hides anything.
const explainArrayColumnMarker = "array results are unsupported"

func (s *Server) explainQuery(ctx context.Context, params lsp.ExecuteCommandParams) (result interface{}, err error) {
	repo, unlock, err := s.acquireReadyConnection()
	if err != nil {
		return nil, err
	}
	defer unlock()

	if len(params.Arguments) == 0 {
		return nil, fmt.Errorf("required arguments were not provided: <File URI>")
	}
	uri, ok := params.Arguments[0].(string)
	if !ok {
		return nil, fmt.Errorf("specify the file uri as a string")
	}
	text, ok := s.fileText(uri)
	if !ok {
		return nil, fmt.Errorf("document not found, %q", uri)
	}
	snapshot, err := s.captureEditorSnapshot(uri)
	if err != nil {
		return nil, err
	}
	if snapshot.Repository != nil {
		repo = snapshot.Repository
	}
	text = snapshot.Text

	// -show-vertical is deliberately not accepted: a plan is not a table.
	if params.Range != nil {
		text = extractRangeText(
			text,
			params.Range.Start.Line,
			params.Range.Start.Character,
			params.Range.End.Line,
			params.Range.End.Character,
		)
	}
	queries, err := explainQueriesWithVariant(text, snapshot.Variant)
	if err != nil {
		return nil, err
	}

	explainer, err := explainRepositoryFor(repo)
	if err != nil {
		return nil, err
	}

	rendered, err := explainStatements(ctx, explainer, queries)
	if err != nil {
		if notice := cancellationNotice(ctx, err); notice != "" {
			return nil, &cancelledError{rendered: notice}
		}
		return nil, err
	}
	return rendered, nil
}

// explainQueries returns the statements to prepare. An InterBase selection
// carrying genuine named markers is compiled to positional SQL first: a plan
// only needs a preparable statement, so Explain translates the markers and
// never prompts for or binds a value. Everything else — including a selection
// the parameter compiler rejects — takes the ordinary parser path unchanged.
func (s *Server) explainQueries(text string) ([]string, error) {
	return explainQueriesWithVariant(text, s.parserDriverVariant())
}

func explainQueriesWithVariant(text string, variant dialect.DriverVariant) ([]string, error) {
	if variant.Driver == dialect.DatabaseDriverInterBase {
		batch, err := queryparams.Compile(text, variant.Variant.InterBaseSQLDialect())
		if err == nil && len(batch.Parameters) > 0 {
			queries := make([]string, 0, len(batch.Statements))
			for _, stmt := range batch.Statements {
				queries = append(queries, stmt.SQL)
			}
			return queries, nil
		}
	}

	stmts, err := getStatementsWithDriverVariant(text, variant)
	if err != nil {
		return nil, err
	}
	queries := make([]string, 0, len(stmts))
	for _, stmt := range stmts {
		query := strings.TrimSpace(stmt.String())
		if query == "" {
			continue
		}
		queries = append(queries, query)
	}
	return queries, nil
}

// explainRepositoryFor reports whether the active repository can explain. The
// capability, not the driver name, is the gate: a non-InterBase repository
// that later implements ExplainPlan gets the feature for free.
func explainRepositoryFor(repo database.DBRepository) (database.ExplainRepository, error) {
	explainer, ok := repo.(database.ExplainRepository)
	if !ok {
		return nil, fmt.Errorf("explain is not supported by the %s driver", repo.Driver())
	}
	return explainer, nil
}

// explainStatements renders one plan per statement. It takes the capability
// rather than a *Server so it is reachable from a test: the InterBase driver
// name is already claimed in database.driverFactories and RegisterFactory
// panics on a duplicate, so no test can install a capability-bearing
// repository under it.
func explainStatements(ctx context.Context, explainer database.ExplainRepository, queries []string) (string, error) {
	buf := new(bytes.Buffer)
	for i, query := range queries {
		if i > 0 {
			fmt.Fprintln(buf)
		}

		typ, isQuery := database.QueryExecType(query, "")
		switch {
		case isQuery:
			fmt.Fprintf(buf, "-- statement %d\n", i+1)
		case explainMutatingTypes[typ]:
			fmt.Fprintf(buf, "-- statement %d (%s)\n", i+1, explainPreparedOnly)
		default:
			// Refusing DDL costs one condition and avoids handing the user an
			// always-empty plan for CREATE TABLE, which reads like a bug.
			fmt.Fprintf(buf, "-- statement %d\n", i+1)
			fmt.Fprintf(buf, explainRefusal+"\n", typ)
			continue
		}

		plan, err := explainer.ExplainPlan(ctx, query)
		if err != nil {
			if strings.Contains(err.Error(), explainArrayColumnMarker) {
				fmt.Fprintln(buf, explainArrayColumn)
				fmt.Fprintln(buf, err.Error())
				continue
			}
			return "", err
		}
		if strings.TrimSpace(plan) == "" {
			fmt.Fprintln(buf, explainNoPlan)
			continue
		}
		fmt.Fprintln(buf, strings.TrimRight(plan, "\n"))
	}
	return buf.String(), nil
}
