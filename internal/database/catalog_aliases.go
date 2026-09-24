package database

import (
	"github.com/sqls-server/sqls/catalog"
	"github.com/sqls-server/sqls/metadata"
)

// Compatibility aliases: these types now live in the public catalog and
// metadata packages. Aliasing (rather than copying) preserves pointer/value
// identity for every existing internal/database caller and test.
type (
	ColumnBase             = catalog.ColumnBase
	ColumnDesc             = catalog.ColumnDesc
	ForeignKey             = catalog.ForeignKey
	DBCache                = catalog.Cache
	CatalogCache           = catalog.CatalogCache
	ObjectKind             = catalog.ObjectKind
	ViewDesc               = catalog.ViewDesc
	GeneratorDesc          = catalog.GeneratorDesc
	ProcedureDesc          = catalog.ProcedureDesc
	ParameterDirection     = catalog.ParameterDirection
	ProcedureParameterDesc = catalog.ProcedureParameterDesc
	TriggerDesc            = catalog.TriggerDesc
	DomainDesc             = catalog.DomainDesc
	IndexDesc              = catalog.IndexDesc
	FunctionDesc           = catalog.FunctionDesc
	FunctionArgumentDesc   = catalog.FunctionArgumentDesc
	TableDescription       = catalog.TableDescription
	DescriptionSpan        = catalog.DescriptionSpan
	DescriptionColumn      = catalog.DescriptionColumn

	MetadataKind           = catalog.MetadataKind
	MetadataState          = catalog.MetadataState
	MetadataStatus         = catalog.MetadataStatus
	MetadataSnapshot       = catalog.MetadataSnapshot
	MetadataPatch          = catalog.MetadataPatch
	MetadataJob            = catalog.MetadataJob
	MetadataPlan           = catalog.MetadataPlan
	MetadataPlanRepository = catalog.MetadataPlanRepository

	MetadataLoad = metadata.Load
)

const (
	ObjectKindTable     = catalog.ObjectKindTable
	ObjectKindView      = catalog.ObjectKindView
	ObjectKindProcedure = catalog.ObjectKindProcedure
	ObjectKindTrigger   = catalog.ObjectKindTrigger
	ObjectKindDomain    = catalog.ObjectKindDomain
	ObjectKindIndex     = catalog.ObjectKindIndex
	ObjectKindGenerator = catalog.ObjectKindGenerator
	ObjectKindFunction  = catalog.ObjectKindFunction

	ParameterInput  = catalog.ParameterInput
	ParameterOutput = catalog.ParameterOutput

	MetadataSchemas        = catalog.MetadataSchemas
	MetadataRelations      = catalog.MetadataRelations
	MetadataColumnsCurrent = catalog.MetadataColumnsCurrent
	MetadataColumnsAll     = catalog.MetadataColumnsAll
	MetadataPrimaryKeys    = catalog.MetadataPrimaryKeys
	MetadataForeignKeys    = catalog.MetadataForeignKeys
	MetadataViews          = catalog.MetadataViews
	MetadataProcedures     = catalog.MetadataProcedures
	MetadataGenerators     = catalog.MetadataGenerators
	MetadataDomains        = catalog.MetadataDomains
	MetadataFunctions      = catalog.MetadataFunctions
	MetadataIndexes        = catalog.MetadataIndexes
	MetadataTriggers       = catalog.MetadataTriggers

	MetadataPending     = catalog.MetadataPending
	MetadataLoading     = catalog.MetadataLoading
	MetadataReady       = catalog.MetadataReady
	MetadataFailed      = catalog.MetadataFailed
	MetadataBlocked     = catalog.MetadataBlocked
	MetadataCancelled   = catalog.MetadataCancelled
	MetadataUnsupported = catalog.MetadataUnsupported
)
