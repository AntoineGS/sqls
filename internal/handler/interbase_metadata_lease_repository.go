package handler

import (
	"context"
	"errors"

	"github.com/sqls-server/sqls/catalog"
	"github.com/sqls-server/sqls/dialect"
	"github.com/sqls-server/sqls/internal/database"
	"github.com/sqls-server/sqls/metadata"
)

// leaseDDLObjectKinds is the set of object kinds InterBase's shared "ddl"
// detail facet covers (see internal/database/interbase_metadata_detail.go's
// detailPlanDDL). Table and function DDL are not implemented there — table
// needs constraint/index/trigger facts no bulk category retains, and function
// DDL is permanently refused by the driver — so both kinds fall straight
// through to the wrapped repository's own catalog round trip, unchanged.
var leaseDDLObjectKinds = map[database.ObjectKind]bool{
	database.ObjectKindProcedure: true,
	database.ObjectKindDomain:    true,
	database.ObjectKindTrigger:   true,
	database.ObjectKindIndex:     true,
	database.ObjectKindGenerator: true,
	database.ObjectKindView:      true,
}

// leaseDDLRepository decorates an InterBase database.DBRepository so
// hover/definition's ObjectDDL calls share the metadata lease's Detail load
// (see metadata.Lease.Detail's shared-Load-per-Coverage contract) with any
// other consumer of the same store, instead of each issuing its own catalog
// round trip. Kinds the lease's "ddl" facet does not cover, and any
// ErrUnsupported answer from the lease, fall back to the wrapped
// repository's own ObjectDDL.
type leaseDDLRepository struct {
	database.DBRepository
	lease   *metadata.Lease
	dialect int
}

var (
	_ database.DDLRepository              = (*leaseDDLRepository)(nil)
	_ database.TableDescriptionRepository = (*leaseDDLRepository)(nil)
)

// wrapRepositoryWithLease returns repo decorated to route InterBase object
// DDL lookups through lease, or repo unchanged when the driver is not
// InterBase, repo is nil, or no lease is bound for this snapshot. Non-InterBase
// drivers' Provider.DetailPlan is always metadata.ErrUnsupported by design
// (Ruling R8 in docs/superpowers/plans/2026-09-24-sqls-shared-metadata.md), so
// they always keep the raw-repository path.
func wrapRepositoryWithLease(repo database.DBRepository, lease *metadata.Lease, dv dialect.DriverVariant) database.DBRepository {
	if repo == nil || lease == nil || dv.Driver != dialect.DatabaseDriverInterBase {
		return repo
	}
	return &leaseDDLRepository{DBRepository: repo, lease: lease, dialect: dv.Variant.InterBaseSQLDialect()}
}

func (r *leaseDDLRepository) ObjectDDL(ctx context.Context, kind database.ObjectKind, name string) (string, error) {
	if !leaseDDLObjectKinds[kind] {
		return r.rawObjectDDL(ctx, kind, name)
	}
	detail, err := r.lease.Detail(ctx, metadata.Request{
		Object:      catalog.ObjectID{Kind: kind, Name: name},
		Facet:       "ddl",
		Dialect:     r.dialect,
		Consistency: metadata.Committed,
	})
	if err != nil {
		if errors.Is(err, metadata.ErrUnsupported) {
			return r.rawObjectDDL(ctx, kind, name)
		}
		return "", err
	}
	return detail.Text, nil
}

func (r *leaseDDLRepository) rawObjectDDL(ctx context.Context, kind database.ObjectKind, name string) (string, error) {
	ddlRepo, ok := r.DBRepository.(database.DDLRepository)
	if !ok {
		return "", database.ErrUnsupportedDDL
	}
	return ddlRepo.ObjectDDL(ctx, kind, name)
}

// TableDescription forwards to the wrapped repository's own capability.
// TableDescriptionRepository is not part of database.DBRepository, so it is
// not otherwise promoted through the embedded interface; without this method
// interBaseRelationDefinitionWithSnapshot's unsupportedTableDescription
// fallback (see interbase_relation_definition.go) would silently stop working
// once a repo passed through this decorator.
func (r *leaseDDLRepository) TableDescription(ctx context.Context, name string) (database.TableDescription, error) {
	source, ok := r.DBRepository.(database.TableDescriptionRepository)
	if !ok {
		return database.TableDescription{}, database.ErrUnsupportedDDL
	}
	return source.TableDescription(ctx, name)
}
