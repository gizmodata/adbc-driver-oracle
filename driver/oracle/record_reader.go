package oracle

import (
	"context"
	"sync/atomic"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"

	"github.com/gizmodata/adbc-driver-oracle/internal/ttc"
)

// streamingRecordReader converts fetched rows into Arrow records on
// demand. Each Next() yields at most batchSize rows, fetching further
// rows from the server as needed, so peak memory is bounded by one Arrow
// batch regardless of the result-set size.
type streamingRecordReader struct {
	ctx          context.Context
	stmt         *ttc.Statement
	conn         *connectionImpl
	alloc        memory.Allocator
	sink         *arrowSink
	batchSize    int
	prefetchRows int
	numberMode   string
	schema       *arrow.Schema
	current      arrow.Record
	err          error
	refs         atomic.Int64
	done         bool
	closed       bool
}

func newStreamingRecordReader(ctx context.Context, conn *connectionImpl, stmt *ttc.Statement, batchSize, prefetchRows int, numberMode string) *streamingRecordReader {
	r := &streamingRecordReader{
		ctx:          ctx,
		stmt:         stmt,
		conn:         conn,
		alloc:        conn.alloc,
		batchSize:    batchSize,
		prefetchRows: prefetchRows,
		numberMode:   numberMode,
	}
	r.sink = newArrowSink(conn.alloc, stmt, numberMode)
	r.refs.Store(1)
	return r
}

// start is called after Execute succeeded: the describe information is
// now available and the schema can be finalized.
func (r *streamingRecordReader) start() error {
	if err := r.sink.ensure(); err != nil {
		return err
	}
	r.schema = r.sink.schema
	return nil
}

// abandon releases resources after a failed execute.
func (r *streamingRecordReader) abandon() {
	r.sink.release()
	r.stmt.Close()
	r.closed = true
}

func (r *streamingRecordReader) Retain() { r.refs.Add(1) }

func (r *streamingRecordReader) Release() {
	if r.refs.Add(-1) != 0 {
		return
	}
	if r.current != nil {
		r.current.Release()
		r.current = nil
	}
	r.close()
}

func (r *streamingRecordReader) close() {
	if r.closed {
		return
	}
	r.closed = true
	r.sink.release()
	r.stmt.Close()
}

func (r *streamingRecordReader) Schema() *arrow.Schema { return r.schema }

func (r *streamingRecordReader) RecordBatch() arrow.RecordBatch { return r.current }

// Record is the deprecated alias for RecordBatch.
func (r *streamingRecordReader) Record() arrow.RecordBatch { return r.current }

func (r *streamingRecordReader) Err() error { return r.err }

func (r *streamingRecordReader) Next() bool {
	if r.err != nil || r.done || r.closed {
		return false
	}
	if r.current != nil {
		r.current.Release()
		r.current = nil
	}
	for r.sink.rows < r.batchSize && r.stmt.MoreRows() {
		want := r.batchSize - r.sink.rows
		if want > r.prefetchRows {
			want = r.prefetchRows
		}
		if err := r.stmt.Fetch(r.ctx, r.sink, want); err != nil {
			r.err = fromTTCError(err)
			r.close()
			return false
		}
		if err := r.sink.err; err != nil {
			r.err = err
			r.close()
			return false
		}
	}
	if r.sink.rows == 0 {
		r.done = true
		r.close()
		return false
	}
	r.current = r.sink.newRecord()
	return true
}

var _ array.RecordReader = (*streamingRecordReader)(nil)
