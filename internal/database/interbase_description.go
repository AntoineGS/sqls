package database

import (
	"context"
	"errors"

	"interbase-go/schema"
)

func (db *InterBaseDBRepository) TableDescription(ctx context.Context, name string) (TableDescription, error) {
	if db == nil || db.Conn == nil {
		return TableDescription{}, errors.New("interbase: database connection is nil")
	}

	relation, err := schema.New(db.Conn).Table(ctx, name)
	if err != nil {
		return TableDescription{}, err
	}
	if relation == nil || relation.Name != name {
		return TableDescription{}, ErrObjectNotFound
	}

	rendered, err := relation.DescribeCatalogWithOptions(schema.DDLOptions{Dialect: interBaseSourceDialect(db)})
	if err != nil {
		return TableDescription{}, err
	}
	return copyCatalogDescription(rendered), nil
}

func copyCatalogDescription(description schema.CatalogDescription) TableDescription {
	result := TableDescription{
		Body:    description.Body,
		Table:   DescriptionSpan{Start: description.Table.Start, End: description.Table.End},
		Columns: make([]DescriptionColumn, len(description.Columns)),
	}
	for index, column := range description.Columns {
		result.Columns[index] = DescriptionColumn{
			Name: column.Name,
			Span: DescriptionSpan{Start: column.Span.Start, End: column.Span.End},
		}
	}
	return result
}

var _ TableDescriptionRepository = (*InterBaseDBRepository)(nil)
