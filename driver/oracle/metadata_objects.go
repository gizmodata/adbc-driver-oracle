package oracle

import (
	"context"
	"strconv"
	"strings"

	"github.com/apache/arrow-adbc/go/adbc"
	"github.com/apache/arrow-go/v18/arrow/array"

	"github.com/gizmodata/adbc-driver-oracle/internal/oratype"
)

// getObjectsImpl implements adbc.Connection.GetObjects from the Oracle
// data dictionary (ALL_USERS / ALL_OBJECTS / ALL_TAB_COLUMNS /
// ALL_CONSTRAINTS / ALL_CONS_COLUMNS). Oracle has a single catalog per
// connection — the database (PDB) name — so the catalog level always has
// exactly one entry; schemas are Oracle users.
func (c *connectionImpl) getObjectsImpl(
	ctx context.Context,
	depth adbc.ObjectDepth,
	catalog, dbSchema, tableName, columnName *string,
	tableTypes []string,
) (array.RecordReader, error) {
	dbName := c.conn.DBName()
	if dbName == "" {
		dbName = c.conn.ServiceName()
	}
	if catalog != nil && *catalog != "" && !strings.EqualFold(*catalog, dbName) {
		return buildGetObjectsRecordReader(c.alloc, nil)
	}
	entry := getObjectsInfo{CatalogName: &dbName, CatalogDbSchemas: []dbSchemaInfo{}}
	if depth != adbc.ObjectDepthCatalogs {
		schemas, err := c.listSchemas(ctx, dbSchema)
		if err != nil {
			return nil, err
		}
		includeColumns := depth == adbc.ObjectDepthAll || depth == adbc.ObjectDepthColumns
		for _, sch := range schemas {
			schName := sch
			se := dbSchemaInfo{DbSchemaName: &schName, DbSchemaTables: []tableInfo{}}
			if depth != adbc.ObjectDepthDBSchemas {
				tables, err := c.listTables(ctx, sch, tableName, columnName, tableTypes, includeColumns)
				if err != nil {
					return nil, err
				}
				se.DbSchemaTables = tables
			}
			entry.CatalogDbSchemas = append(entry.CatalogDbSchemas, se)
		}
	}
	return buildGetObjectsRecordReader(c.alloc, []getObjectsInfo{entry})
}

func (c *connectionImpl) listSchemas(ctx context.Context, filter *string) ([]string, error) {
	sb := strings.Builder{}
	sb.WriteString("SELECT USERNAME FROM ALL_USERS WHERE 1 = 1")
	appendLikeQual(&sb, "USERNAME", filter)
	sb.WriteString(" ORDER BY USERNAME")
	rows, err := c.queryAll(ctx, sb.String())
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.get(0))
	}
	return out, nil
}

// tableTypeFilter renders an ALL_OBJECTS predicate for ADBC table types.
func tableTypeFilter(types []string) string {
	if len(types) == 0 {
		return " AND O.OBJECT_TYPE IN ('TABLE', 'VIEW', 'MATERIALIZED VIEW', 'SYNONYM')"
	}
	var codes []string
	for _, t := range types {
		switch strings.ToUpper(strings.TrimSpace(t)) {
		case "TABLE", "BASE TABLE":
			codes = append(codes, "'TABLE'")
		case "VIEW":
			codes = append(codes, "'VIEW'")
		case "MATERIALIZED VIEW":
			codes = append(codes, "'MATERIALIZED VIEW'")
		case "SYNONYM", "ALIAS":
			codes = append(codes, "'SYNONYM'")
		case "GLOBAL TEMPORARY", "LOCAL TEMPORARY", "TEMPORARY":
			codes = append(codes, "'TABLE'")
		}
	}
	if len(codes) == 0 {
		return " AND 1 = 0"
	}
	return " AND O.OBJECT_TYPE IN (" + strings.Join(codes, ", ") + ")"
}

func (c *connectionImpl) listTables(ctx context.Context, schema string, tableFilter, columnFilter *string, tableTypes []string, includeColumns bool) ([]tableInfo, error) {
	sb := strings.Builder{}
	sb.WriteString("SELECT O.OBJECT_NAME, O.OBJECT_TYPE, T.TEMPORARY FROM ALL_OBJECTS O LEFT JOIN ALL_TABLES T ON T.OWNER = O.OWNER AND T.TABLE_NAME = O.OBJECT_NAME WHERE O.OWNER = ")
	sb.WriteString(sqlString(schema))
	sb.WriteString(" AND O.OBJECT_NAME NOT LIKE 'BIN$%'")
	appendLikeQual(&sb, "O.OBJECT_NAME", tableFilter)
	sb.WriteString(tableTypeFilter(tableTypes))
	sb.WriteString(" ORDER BY O.OBJECT_NAME")
	rows, err := c.queryAll(ctx, sb.String())
	if err != nil {
		return nil, err
	}
	wantTemp := false
	onlyTemp := false
	for _, t := range tableTypes {
		switch strings.ToUpper(strings.TrimSpace(t)) {
		case "GLOBAL TEMPORARY", "LOCAL TEMPORARY", "TEMPORARY":
			wantTemp = true
		case "TABLE", "BASE TABLE":
			onlyTemp = false
		}
	}
	if wantTemp {
		onlyTemp = true
		for _, t := range tableTypes {
			if u := strings.ToUpper(strings.TrimSpace(t)); u == "TABLE" || u == "BASE TABLE" {
				onlyTemp = false
			}
		}
	}
	out := make([]tableInfo, 0, len(rows))
	var columnsByTable map[string][]columnInfo
	var constraintsByTable map[string][]constraintInfo
	if includeColumns && len(rows) > 0 {
		columnsByTable, err = c.listColumns(ctx, schema, tableFilter, columnFilter)
		if err != nil {
			return nil, err
		}
		constraintsByTable, err = c.listConstraints(ctx, schema, tableFilter)
		if err != nil {
			return nil, err
		}
	}
	for _, r := range rows {
		name := r.get(0)
		typ := r.get(1)
		if typ == "TABLE" && r.get(2) == "Y" {
			typ = "GLOBAL TEMPORARY"
		} else if typ == "TABLE" && onlyTemp {
			continue
		}
		ti := tableInfo{
			TableName:        name,
			TableType:        typ,
			TableColumns:     []columnInfo{},
			TableConstraints: []constraintInfo{},
		}
		if includeColumns {
			if cols, ok := columnsByTable[name]; ok {
				ti.TableColumns = cols
			}
			if cs, ok := constraintsByTable[name]; ok {
				ti.TableConstraints = cs
			}
		}
		out = append(out, ti)
	}
	return out, nil
}

// listColumns fetches every column of every matching table in one query
// and groups them by table.
func (c *connectionImpl) listColumns(ctx context.Context, schema string, tableFilter, columnFilter *string) (map[string][]columnInfo, error) {
	sb := strings.Builder{}
	sb.WriteString(`SELECT C.TABLE_NAME, C.COLUMN_NAME, C.COLUMN_ID, C.DATA_TYPE, C.DATA_LENGTH, C.DATA_PRECISION, C.DATA_SCALE, C.NULLABLE, C.DATA_DEFAULT, C.CHAR_LENGTH, C.IDENTITY_COLUMN, C.VIRTUAL_COLUMN, M.COMMENTS
FROM ALL_TAB_COLS C LEFT JOIN ALL_COL_COMMENTS M ON M.OWNER = C.OWNER AND M.TABLE_NAME = C.TABLE_NAME AND M.COLUMN_NAME = C.COLUMN_NAME
WHERE C.OWNER = `)
	sb.WriteString(sqlString(schema))
	sb.WriteString(" AND C.HIDDEN_COLUMN = 'NO'")
	appendLikeQual(&sb, "C.TABLE_NAME", tableFilter)
	appendLikeQual(&sb, "C.COLUMN_NAME", columnFilter)
	sb.WriteString(" ORDER BY C.TABLE_NAME, C.COLUMN_ID")
	rows, err := c.queryAll(ctx, sb.String())
	if err != nil {
		return nil, err
	}
	out := map[string][]columnInfo{}
	for _, r := range rows {
		tab := r.get(0)
		ci := columnInfo{ColumnName: r.get(1)}
		if n, err := strconv.Atoi(r.get(2)); err == nil {
			ord := int32(n)
			ci.OrdinalPosition = &ord
		}
		typeName := r.get(3)
		length, _ := strconv.Atoi(r.get(4))
		precision, hasPrec := 0, !r.isNull(5)
		if hasPrec {
			precision, _ = strconv.Atoi(r.get(5))
		}
		scale, hasScale := 0, !r.isNull(6)
		if hasScale {
			scale, _ = strconv.Atoi(r.get(6))
		}
		charLen, _ := strconv.Atoi(r.get(9))
		ci.XdbcTypeName = strPtr(renderTypeName(typeName, length, charLen, precision, hasPrec, scale, hasScale))
		size := int32(length)
		if charLen > 0 {
			size = int32(charLen)
		}
		if hasPrec {
			size = int32(precision)
		}
		ci.XdbcColumnSize = &size
		if hasScale {
			dd := int16(scale)
			ci.XdbcDecimalDigits = &dd
		}
		radix := int16(10)
		ci.XdbcNumPrecRadix = &radix
		nullable := r.get(7) == "Y"
		nv := int16(0)
		yn := "NO"
		if nullable {
			nv, yn = 1, "YES"
		}
		ci.XdbcNullable = &nv
		ci.XdbcIsNullable = &yn
		if !r.isNull(8) {
			ci.XdbcColumnDef = strPtr(strings.TrimSpace(r.get(8)))
		}
		if !r.isNull(12) && r.get(12) != "" {
			ci.Remarks = strPtr(r.get(12))
		}
		auto := r.get(10) == "YES"
		ci.XdbcIsAutoincrement = &auto
		gen := r.get(11) == "YES"
		ci.XdbcIsGeneratedcolumn = &gen
		dt := xdbcDataType(typeName, hasScale && scale == 0 && hasPrec)
		ci.XdbcDataType = &dt
		ci.XdbcSqlDataType = &dt
		octet := int32(length)
		ci.XdbcCharOctetLength = &octet
		out[tab] = append(out[tab], ci)
	}
	return out, nil
}

func renderTypeName(typeName string, length, charLen, precision int, hasPrec bool, scale int, hasScale bool) string {
	switch {
	case typeName == "NUMBER":
		if hasPrec && hasScale {
			return "NUMBER(" + itoa(precision) + "," + itoa(scale) + ")"
		}
		if hasPrec {
			return "NUMBER(" + itoa(precision) + ")"
		}
		return "NUMBER"
	case typeName == "FLOAT":
		if hasPrec {
			return "FLOAT(" + itoa(precision) + ")"
		}
		return "FLOAT"
	case typeName == "VARCHAR2" || typeName == "NVARCHAR2" || typeName == "CHAR" || typeName == "NCHAR":
		if charLen > 0 {
			return typeName + "(" + itoa(charLen) + ")"
		}
		return typeName + "(" + itoa(length) + ")"
	case typeName == "RAW":
		return "RAW(" + itoa(length) + ")"
	case strings.HasPrefix(typeName, "TIMESTAMP") || strings.HasPrefix(typeName, "INTERVAL"):
		return typeName
	}
	return typeName
}

// xdbcDataType maps an Oracle dictionary type name to a JDBC
// java.sql.Types code.
func xdbcDataType(typeName string, integral bool) int16 {
	switch {
	case typeName == "NUMBER":
		if integral {
			return -5 // BIGINT
		}
		return 2 // NUMERIC
	case typeName == "FLOAT":
		return 6
	case typeName == "BINARY_FLOAT":
		return 100
	case typeName == "BINARY_DOUBLE":
		return 101
	case typeName == "CHAR":
		return 1
	case typeName == "NCHAR":
		return -15
	case typeName == "VARCHAR2", typeName == "VARCHAR":
		return 12
	case typeName == "NVARCHAR2":
		return -9
	case typeName == "LONG":
		return -1
	case typeName == "CLOB":
		return 2005
	case typeName == "NCLOB":
		return 2011
	case typeName == "BLOB":
		return 2004
	case typeName == "RAW":
		return -3
	case typeName == "LONG RAW":
		return -4
	case typeName == "DATE":
		return 93
	case strings.HasPrefix(typeName, "TIMESTAMP") && strings.Contains(typeName, "TIME ZONE"):
		return 2014
	case strings.HasPrefix(typeName, "TIMESTAMP"):
		return 93
	case typeName == "BOOLEAN":
		return 16
	case typeName == "ROWID", typeName == "UROWID":
		return -8
	case typeName == "JSON":
		return 2016
	case typeName == "XMLTYPE":
		return 2009
	}
	return 1111
}

// listConstraints loads primary/unique/foreign keys for the schema.
func (c *connectionImpl) listConstraints(ctx context.Context, schema string, tableFilter *string) (map[string][]constraintInfo, error) {
	sb := strings.Builder{}
	sb.WriteString(`SELECT K.TABLE_NAME, K.CONSTRAINT_NAME, T.CONSTRAINT_TYPE, K.COLUMN_NAME, K.POSITION, T.R_OWNER, T.R_CONSTRAINT_NAME
FROM ALL_CONS_COLUMNS K JOIN ALL_CONSTRAINTS T ON T.OWNER = K.OWNER AND T.CONSTRAINT_NAME = K.CONSTRAINT_NAME AND T.TABLE_NAME = K.TABLE_NAME
WHERE K.OWNER = `)
	sb.WriteString(sqlString(schema))
	sb.WriteString(" AND T.CONSTRAINT_TYPE IN ('P', 'U', 'R')")
	appendLikeQual(&sb, "K.TABLE_NAME", tableFilter)
	sb.WriteString(" ORDER BY K.TABLE_NAME, K.CONSTRAINT_NAME, K.POSITION")
	rows, err := c.queryAll(ctx, sb.String())
	if err != nil {
		return nil, err
	}
	type key struct{ tab, name string }
	type ref struct{ owner, name string }
	order := []key{}
	byKey := map[key]*constraintInfo{}
	refs := map[key]ref{}
	for _, r := range rows {
		k := key{r.get(0), r.get(1)}
		ci, ok := byKey[k]
		if !ok {
			name := k.name
			ct := "UNIQUE"
			switch r.get(2) {
			case "P":
				ct = "PRIMARY KEY"
			case "R":
				ct = "FOREIGN KEY"
				refs[k] = ref{r.get(5), r.get(6)}
			case "C":
				ct = "CHECK"
			}
			ci = &constraintInfo{ConstraintName: &name, ConstraintType: ct, ConstraintColumnNames: []string{}}
			byKey[k] = ci
			order = append(order, k)
		}
		ci.ConstraintColumnNames = append(ci.ConstraintColumnNames, r.get(3))
	}
	if len(refs) > 0 {
		// Resolve referenced (parent) key columns.
		sb.Reset()
		sb.WriteString(`SELECT K.OWNER, K.CONSTRAINT_NAME, K.TABLE_NAME, K.COLUMN_NAME, K.POSITION
FROM ALL_CONS_COLUMNS K WHERE (K.OWNER, K.CONSTRAINT_NAME) IN (`)
		i := 0
		for _, rf := range refs {
			if i > 0 {
				sb.WriteString(", ")
			}
			sb.WriteString("(" + sqlString(rf.owner) + ", " + sqlString(rf.name) + ")")
			i++
		}
		sb.WriteString(") ORDER BY K.OWNER, K.CONSTRAINT_NAME, K.POSITION")
		parents, err := c.queryAll(ctx, sb.String())
		if err != nil {
			return nil, err
		}
		byRef := map[ref][]stringRow{}
		for _, p := range parents {
			rk := ref{p.get(0), p.get(1)}
			byRef[rk] = append(byRef[rk], p)
		}
		dbName := c.conn.DBName()
		for k, rf := range refs {
			ci := byKey[k]
			for _, p := range byRef[rf] {
				ci.ConstraintColumnUsage = append(ci.ConstraintColumnUsage, constraintColumnUsage{
					FkCatalog:    strPtr(dbName),
					FkDbSchema:   strPtr(p.get(0)),
					FkTable:      p.get(2),
					FkColumnName: p.get(3),
				})
			}
		}
	}
	out := map[string][]constraintInfo{}
	for _, k := range order {
		out[k.tab] = append(out[k.tab], *byKey[k])
	}
	return out, nil
}

func strPtr(s string) *string { return &s }

func appendLikeQual(sb *strings.Builder, col string, pattern *string) {
	if pattern == nil {
		return
	}
	sb.WriteString(" AND ")
	sb.WriteString(col)
	if *pattern == "" {
		sb.WriteString(" IS NULL")
		return
	}
	if strings.ContainsAny(*pattern, "%_") {
		sb.WriteString(" LIKE ")
		sb.WriteString(sqlString(*pattern))
		sb.WriteString(" ESCAPE '\\'")
		return
	}
	sb.WriteString(" = ")
	sb.WriteString(sqlString(*pattern))
}

func decodeNumberText(data []byte) (string, error) {
	d, err := oratype.DecodeNumber(data, nil)
	if err != nil {
		return "", err
	}
	return string(d.Text), nil
}
