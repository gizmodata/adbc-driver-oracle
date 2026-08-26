package oracle

import (
	"context"
	"strings"

	"github.com/apache/arrow-adbc/go/adbc"
	"github.com/apache/arrow-go/v18/arrow"

	"github.com/gizmodata/adbc-driver-oracle/internal/ttc"
)

// Bulk ingest: ADBC's standard pattern is
//
//	stmt.SetOption(ADBC_INGEST_OPTION_TARGET_TABLE, "tbl")
//	stmt.BindStream(ctx, reader)   // or stmt.Bind(ctx, batch)
//	stmt.ExecuteUpdate(ctx)         // dispatches to executeIngest
//
// Rows are loaded with array-bound INSERT statements (Oracle array DML),
// ingestBatchRows rows per round trip.

const defaultIngestBatchRows = 5000

func (s *statementImpl) executeIngest(ctx context.Context) (int64, error) {
	if s.targetTable == "" {
		return -1, errStatus(adbc.StatusInvalidState, "ingest: no target table set")
	}
	if s.bound == nil && s.boundStream == nil {
		return -1, errStatus(adbc.StatusInvalidState, "ingest: no record/stream bound")
	}
	defer s.clearBound()

	var schema *arrow.Schema
	if s.bound != nil {
		schema = s.bound.Schema()
	} else {
		schema = s.boundStream.Schema()
	}
	if err := s.prepareIngestTarget(ctx, schema); err != nil {
		return -1, err
	}

	insertSQL := s.buildInsertSQL(schema)
	ins := s.conn.conn.Prepare(insertSQL)
	defer ins.Close()
	batchRows := s.ingestBatchRows
	if batchRows <= 0 {
		batchRows = defaultIngestBatchRows
	}

	var total int64
	pump := func(rec arrow.Record) error {
		for off := int64(0); off < rec.NumRows(); off += int64(batchRows) {
			n := rec.NumRows() - off
			if n > int64(batchRows) {
				n = int64(batchRows)
			}
			slice := rec.NewSlice(off, off+n)
			binds, err := bindsFromRecord(slice, s.conn.conn)
			if err != nil {
				slice.Release()
				return err
			}
			err = ins.Execute(ctx, ttc.ExecuteOptions{Binds: binds, NumRows: int(n), AutoCommit: s.conn.autoCommit})
			slice.Release()
			if err != nil {
				return fromTTCError(err)
			}
			total += int64(ins.RowCount())
		}
		return nil
	}
	if s.bound != nil {
		if s.bound.NumRows() > 0 {
			if err := pump(s.bound); err != nil {
				return -1, err
			}
		}
	}
	if s.boundStream != nil {
		for s.boundStream.Next() {
			rec := s.boundStream.Record()
			if rec.NumRows() == 0 {
				continue
			}
			if err := pump(rec); err != nil {
				return -1, err
			}
		}
		if err := s.boundStream.Err(); err != nil {
			return -1, errStatus(adbc.StatusIO, "ingest stream: %v", err)
		}
	}
	return total, nil
}

func (s *statementImpl) qualifiedTarget() string {
	if s.targetSchema != "" {
		return identFor(s.targetSchema) + "." + identFor(s.targetTable)
	}
	return identFor(s.targetTable)
}

func (s *statementImpl) buildInsertSQL(schema *arrow.Schema) string {
	var b strings.Builder
	b.WriteString("INSERT INTO ")
	b.WriteString(s.qualifiedTarget())
	b.WriteString(" (")
	for i, f := range schema.Fields() {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(identFor(f.Name))
	}
	b.WriteString(") VALUES (")
	for i := range schema.Fields() {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(":")
		b.WriteString(itoa(i + 1))
	}
	b.WriteString(")")
	return b.String()
}

// prepareIngestTarget runs the DDL implied by the ingest mode:
//
//	create        → CREATE TABLE               (error if it exists)
//	append        → no DDL                     (insert fails if missing)
//	replace       → DROP TABLE + CREATE TABLE
//	create_append → CREATE TABLE unless it exists
func (s *statementImpl) prepareIngestTarget(ctx context.Context, schema *arrow.Schema) error {
	mode := s.ingestMode
	if mode == "" {
		mode = adbc.OptionValueIngestModeCreate
	}
	if mode == adbc.OptionValueIngestModeAppend {
		return nil
	}
	if mode == adbc.OptionValueIngestModeReplace {
		err := s.conn.execNoResult(ctx, "DROP TABLE "+s.qualifiedTarget()+" PURGE")
		if err != nil {
			var ae adbc.Error
			if !(asADBC(err, &ae) && ae.VendorCode == 942) {
				return err
			}
		}
	}
	ddl, err := s.buildCreateTableSQL(schema)
	if err != nil {
		return errStatus(adbc.StatusInvalidArgument, "ingest: %v", err)
	}
	if err := s.conn.execNoResult(ctx, ddl); err != nil {
		var ae adbc.Error
		if asADBC(err, &ae) && ae.VendorCode == 955 {
			if mode == adbc.OptionValueIngestModeCreateAppend {
				return nil
			}
			return adbc.Error{Code: adbc.StatusAlreadyExists, Msg: ae.Msg, VendorCode: ae.VendorCode}
		}
		return err
	}
	return nil
}

func (s *statementImpl) buildCreateTableSQL(schema *arrow.Schema) (string, error) {
	var b strings.Builder
	b.WriteString("CREATE ")
	if s.ingestTemporary {
		b.WriteString("GLOBAL TEMPORARY ")
	}
	b.WriteString("TABLE ")
	b.WriteString(s.qualifiedTarget())
	b.WriteString(" (")
	for i, f := range schema.Fields() {
		if i > 0 {
			b.WriteString(", ")
		}
		colType, err := oracleTypeForArrow(f.Type, s, s.conn.conn.SupportsBoolean())
		if err != nil {
			return "", err
		}
		b.WriteString(identFor(f.Name))
		b.WriteByte(' ')
		b.WriteString(colType)
		if !f.Nullable {
			b.WriteString(" NOT NULL")
		}
	}
	b.WriteString(")")
	if s.ingestTemporary {
		b.WriteString(" ON COMMIT PRESERVE ROWS")
	}
	return b.String(), nil
}

func asADBC(err error, target *adbc.Error) bool {
	if e, ok := err.(adbc.Error); ok {
		*target = e
		return true
	}
	return false
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
