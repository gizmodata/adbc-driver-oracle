package oracle

import (
	"encoding/json"

	"github.com/apache/arrow-adbc/go/adbc"
	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
)

// The structs below mirror those in arrow-adbc's
// `go/adbc/driver/internal/driverbase` package. Because that package is
// declared `internal`, we can't import it across module boundaries — so
// we vendor the small set of types needed to feed the standard
// ADBC GetObjectsSchema via JSON round-trip into a RecordBuilder.
//
// Keep field names + JSON tags exactly aligned with the upstream
// definitions; the arrow Record builder uses the JSON keys to drive
// the union/struct/list dispatch.

type getObjectsInfo struct {
	CatalogName      *string        `json:"catalog_name,omitempty"`
	CatalogDbSchemas []dbSchemaInfo `json:"catalog_db_schemas"`
}

type dbSchemaInfo struct {
	DbSchemaName   *string     `json:"db_schema_name,omitempty"`
	DbSchemaTables []tableInfo `json:"db_schema_tables"`
}

type tableInfo struct {
	TableName        string           `json:"table_name"`
	TableType        string           `json:"table_type"`
	TableColumns     []columnInfo     `json:"table_columns"`
	TableConstraints []constraintInfo `json:"table_constraints"`
}

type columnInfo struct {
	ColumnName            string  `json:"column_name"`
	OrdinalPosition       *int32  `json:"ordinal_position,omitempty"`
	Remarks               *string `json:"remarks,omitempty"`
	XdbcDataType          *int16  `json:"xdbc_data_type,omitempty"`
	XdbcTypeName          *string `json:"xdbc_type_name,omitempty"`
	XdbcColumnSize        *int32  `json:"xdbc_column_size,omitempty"`
	XdbcDecimalDigits     *int16  `json:"xdbc_decimal_digits,omitempty"`
	XdbcNumPrecRadix      *int16  `json:"xdbc_num_prec_radix,omitempty"`
	XdbcNullable          *int16  `json:"xdbc_nullable,omitempty"`
	XdbcColumnDef         *string `json:"xdbc_column_def,omitempty"`
	XdbcSqlDataType       *int16  `json:"xdbc_sql_data_type,omitempty"`
	XdbcDatetimeSub       *int16  `json:"xdbc_datetime_sub,omitempty"`
	XdbcCharOctetLength   *int32  `json:"xdbc_char_octet_length,omitempty"`
	XdbcIsNullable        *string `json:"xdbc_is_nullable,omitempty"`
	XdbcScopeCatalog      *string `json:"xdbc_scope_catalog,omitempty"`
	XdbcScopeSchema       *string `json:"xdbc_scope_schema,omitempty"`
	XdbcScopeTable        *string `json:"xdbc_scope_table,omitempty"`
	XdbcIsAutoincrement   *bool   `json:"xdbc_is_autoincrement,omitempty"`
	XdbcIsGeneratedcolumn *bool   `json:"xdbc_is_generatedcolumn,omitempty"`
}

type constraintInfo struct {
	ConstraintName        *string                 `json:"constraint_name,omitempty"`
	ConstraintType        string                  `json:"constraint_type"`
	ConstraintColumnNames []string                `json:"constraint_column_names"`
	ConstraintColumnUsage []constraintColumnUsage `json:"constraint_column_usage,omitempty"`
}

type constraintColumnUsage struct {
	FkCatalog    *string `json:"fk_catalog,omitempty"`
	FkDbSchema   *string `json:"fk_db_schema,omitempty"`
	FkTable      string  `json:"fk_table"`
	FkColumnName string  `json:"fk_column_name"`
}

// buildGetObjectsRecordReader is a local reimplementation of
// driverbase.BuildGetObjectsRecordReader. The standard ADBC
// GetObjectsSchema is nested (list<struct<list<struct<list<struct>>>>)
// and Arrow Go's RecordBuilder accepts JSON input that matches its
// schema, so the simplest correct path is JSON marshal -> Unmarshal
// into the builder.
func buildGetObjectsRecordReader(mem memory.Allocator, infos []getObjectsInfo) (array.RecordReader, error) {
	bldr := array.NewRecordBuilder(mem, adbc.GetObjectsSchema)
	defer bldr.Release()
	for _, info := range infos {
		b, err := json.Marshal(info)
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(b, bldr); err != nil {
			return nil, err
		}
	}
	rec := bldr.NewRecord()
	defer rec.Release()
	return array.NewRecordReader(adbc.GetObjectsSchema, []arrow.Record{rec})
}
