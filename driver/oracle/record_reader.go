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
	batchBytes   int64
	prefetchRows int
	schema       *arrow.Schema
	pending      []arrow.Record // byte-bounded slices of an oversized batch
	current      arrow.Record
	err          error
	refs         atomic.Int64
	done         bool
	closed       bool
}

func newStreamingRecordReader(ctx context.Context, conn *connectionImpl, stmt *ttc.Statement) *streamingRecordReader {
	cfg := conn.cfg
	r := &streamingRecordReader{
		ctx:          ctx,
		stmt:         stmt,
		conn:         conn,
		alloc:        conn.alloc,
		batchSize:    cfg.batchSize,
		batchBytes:   cfg.batchBytes,
		prefetchRows: cfg.prefetchRows,
	}
	r.sink = newArrowSink(conn.alloc, stmt, cfg.types)
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
	for _, p := range r.pending {
		p.Release()
	}
	r.pending = nil
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
	if len(r.pending) > 0 {
		r.current = r.pending[0]
		r.pending = r.pending[1:]
		return true
	}
	for r.sink.rows < r.batchSize && r.stmt.MoreRows() && (r.batchBytes == 0 || r.sink.bytes < r.batchBytes) {
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
	if len(r.pending) > 0 {
		r.current = r.pending[0]
		r.pending = r.pending[1:]
		return true
	}
	if r.sink.rows == 0 {
		r.done = true
		r.close()
		return false
	}
	if err := r.sink.flushObjects(r.ctx, r.conn.conn); err != nil {
		r.err = err
		r.close()
		return false
	}
	bytes := r.sink.bytes
	rec := r.sink.newRecord()
	if r.batchBytes > 0 && bytes > r.batchBytes && rec.NumRows() > 1 {
		// The server's prefetch delivered more than batch_bytes at once:
		// hand it out as zero-copy slices of roughly batch_bytes each.
		per := int64(float64(rec.NumRows()) * float64(r.batchBytes) / float64(bytes))
		if per < 1 {
			per = 1
		}
		for off := int64(0); off < rec.NumRows(); off += per {
			end := off + per
			if end > rec.NumRows() {
				end = rec.NumRows()
			}
			r.pending = append(r.pending, rec.NewSlice(off, end))
		}
		rec.Release()
		r.current = r.pending[0]
		r.pending = r.pending[1:]
		return true
	}
	r.current = rec
	return true
}

var _ array.RecordReader = (*streamingRecordReader)(nil)
