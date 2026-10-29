package pipeline

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/git2-project/git2-runner/app/internal/cisvc/apierr"
	"github.com/git2-project/git2-runner/app/internal/cisvc/db"
	"github.com/git2-project/git2-runner/app/internal/cisvc/notify"
)

// MaxChunk is the most log bytes one AppendLog call may carry.
const MaxChunk = 256 << 10

// truncatedNotice ends a log that reached Options.MaxLogSize.
const truncatedNotice = "\n[git2: the log reached its size limit; later output is not stored]\n"

// AppendLog appends data at offset to the log of a job runnerID runs. Bytes
// before the current end of the log are ignored, so a retried call is
// harmless; data past the end is not appended. It returns the log's size
// afterwards and whether the log is truncated.
//
// Every call also extends the runner's lease, since output proves the
// runner is alive.
func (s *Service) AppendLog(ctx context.Context, runnerID, jobID uuid.UUID, offset int64, data []byte) (int64, bool, error) {
	switch {
	case offset < 0:
		return 0, false, apierr.InvalidField("offset", "must not be negative")
	case len(data) == 0 || len(data) > MaxChunk:
		return 0, false, apierr.InvalidField("data", "must be 1 byte to 256 KiB")
	}
	var size int64
	var truncated, grew bool
	err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		grew = false
		j, err := lockRunnersJob(ctx, tx, runnerID, jobID)
		if err != nil {
			return err
		}
		size = j.logSize
		truncated = size >= s.opts.MaxLogSize
		skip := size - offset
		if truncated || skip < 0 || skip >= int64(len(data)) {
			return nil
		}
		chunk := data[skip:]
		if size+int64(len(chunk)) > s.opts.MaxLogSize {
			kept := chunk[:s.opts.MaxLogSize-size]
			chunk = append(append(make([]byte, 0, len(kept)+len(truncatedNotice)), kept...), truncatedNotice...)
			truncated = true
		}
		if _, err := tx.Exec(ctx, `INSERT INTO ci_svc.job_log_chunks (job_id, "offset", data) VALUES ($1, $2, $3)`,
			jobID, size, chunk); err != nil {
			return fmt.Errorf("insert log chunk: %w", err)
		}
		size += int64(len(chunk))
		_, err = tx.Exec(ctx, `
			UPDATE ci_svc.jobs SET log_size = $2,
				lease_expires_at = now() + make_interval(secs => $3::float8), updated_at = now()
			WHERE id = $1`, jobID, size, s.opts.Lease.Seconds())
		if err != nil {
			return fmt.Errorf("update log size: %w", err)
		}
		grew = true
		return nil
	})
	if err != nil {
		return 0, false, err
	}
	if grew {
		s.hub.Publish(ctx, notify.LogTopic(jobID))
	}
	return size, truncated, nil
}

// StreamLog calls send with the job's log from offset on. Without follow
// it sends what is stored now and returns. With follow it keeps sending
// new output until the job has finished and every byte was sent, or ctx
// ends.
func (s *Service) StreamLog(ctx context.Context, jobID uuid.UUID, offset int64, follow bool, send func(offset int64, data []byte) error) error {
	if offset < 0 {
		return apierr.InvalidField("offset", "must not be negative")
	}
	// Subscribe before the first read, so no signal falls between them.
	wake, unsubscribe := s.hub.Subscribe(notify.LogTopic(jobID))
	defer unsubscribe()
	var poll <-chan time.Time
	if follow {
		t := time.NewTicker(s.opts.LogPoll)
		defer t.Stop()
		poll = t.C
	}

	for first := true; ; first = false {
		// Status first, then chunks: a job's final status is written after
		// its last chunk, so a final status read here means every chunk is
		// already visible.
		var status JobStatus
		var size int64
		err := s.pool.QueryRow(ctx, `SELECT status, log_size FROM ci_svc.jobs WHERE id = $1`, jobID).Scan(&status, &size)
		if errors.Is(err, pgx.ErrNoRows) {
			if first {
				return errJobNotFound
			}
			return nil // deleted with its pipeline
		}
		if err != nil {
			return fmt.Errorf("read job: %w", err)
		}
		if offset < size {
			if offset, err = s.sendChunks(ctx, jobID, offset, size, send); err != nil {
				return err
			}
		}
		if !follow || (status.Final() && offset >= size) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-wake:
		case <-poll:
		}
	}
}

type logChunk struct {
	offset int64
	data   []byte
}

// sendChunks sends the log bytes from offset up to size and returns the
// new offset. The chunks are read into memory first, so a slow client
// does not hold a database connection.
func (s *Service) sendChunks(ctx context.Context, jobID uuid.UUID, offset, size int64, send func(int64, []byte) error) (int64, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT "offset", data FROM ci_svc.job_log_chunks
		WHERE job_id = $1 AND "offset" + length(data) > $2 AND "offset" < $3
		ORDER BY "offset"`, jobID, offset, size)
	if err != nil {
		return offset, fmt.Errorf("read log: %w", err)
	}
	chunks, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (logChunk, error) {
		var c logChunk
		err := row.Scan(&c.offset, &c.data)
		return c, err
	})
	if err != nil {
		return offset, fmt.Errorf("read log: %w", err)
	}
	for _, c := range chunks {
		start := max(offset-c.offset, 0)
		if start >= int64(len(c.data)) {
			continue
		}
		if err := send(c.offset+start, c.data[start:]); err != nil {
			return offset, err
		}
		offset = c.offset + int64(len(c.data))
	}
	return offset, nil
}
