package deploy

import (
	"context"
	"fmt"

	"hmoob-dock/internal/store"
)

// MaxTaskOutputBytes is the largest output of a task the control端 keeps.
//
// The output beyond the limit is dropped and the task is marked as
// truncated, so the administrator learns that the output is incomplete
// instead of believing that the command was quiet.
const MaxTaskOutputBytes = 2 * 1024 * 1024

// OutputFragment is a fragment of the output of a task.
type OutputFragment struct {
	// Offset is the position of the fragment in the output of the task.
	Offset int64 `json:"offset"`
	// Data is the fragment itself.
	Data []byte `json:"data"`
	// Truncated reports that the output of the task was cut short.
	Truncated bool `json:"truncated"`
}

// Output is the output of a task.
//
// The output holds the fragments in the order of their offset, so a reader
// skips the fragments it has already seen.
type Output struct {
	// TaskID is the task the output belongs to.
	TaskID string `json:"task_id"`
	// Fragments are the fragments of the output.
	Fragments []OutputFragment `json:"fragments"`
	// TotalBytes is the number of bytes the output holds.
	TotalBytes int64 `json:"total_bytes"`
	// Truncated reports that the output of the task was cut short.
	Truncated bool `json:"truncated"`
}

// AppendOutput stores a fragment of the output of a task.
//
// The fragment names its offset, so a fragment that arrives twice is stored
// once: the reader of the output never sees the same bytes twice.
//
// The output of a task is limited. The bytes beyond the limit are dropped
// and the task is marked as truncated, so the administrator learns that the
// output is incomplete.
//
// The second return value reports whether the fragment was stored. A
// fragment whose offset is known already is a duplicate and is ignored.
func (s *Service) AppendOutput(ctx context.Context, taskID string, offset int64, data []byte, truncated bool) (bool, error) {
	if offset < 0 {
		return false, fmt.Errorf("%w: the offset of the output is negative", ErrInvalidRequest)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("deploy: start the output of the task %s: %w", taskID, err)
	}
	closed := false
	defer func() {
		if !closed {
			_ = tx.Rollback()
		}
	}()

	var stored int64
	err = tx.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(length(data)), 0) FROM task_output WHERE task_id = ?`, taskID).Scan(&stored)
	if err != nil {
		return false, fmt.Errorf("deploy: read the output of the task %s: %w", taskID, err)
	}

	// The bytes beyond the limit of the task are dropped. The fragment is
	// stored anyway, so the reader learns that the output ends here.
	chunk := data
	cut := truncated
	remaining := int64(MaxTaskOutputBytes) - stored
	switch {
	case remaining <= 0:
		chunk = nil
		cut = true
	case int64(len(chunk)) > remaining:
		chunk = chunk[:remaining]
		cut = true
	}

	result, err := tx.ExecContext(ctx,
		`INSERT OR IGNORE INTO task_output (task_id, offset, data, truncated, created_at)
		 VALUES (?, ?, ?, ?, ?)`,
		taskID, offset, chunk, boolToInt(cut), store.TimeToMillis(s.now()))
	if err != nil {
		return false, fmt.Errorf("deploy: store the output of the task %s: %w", taskID, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("deploy: store the output of the task %s: %w", taskID, err)
	}
	if cut {
		// The flag of the truncation belongs to the whole output, so it is
		// written even when the fragment itself is a duplicate.
		if _, err := tx.ExecContext(ctx,
			`UPDATE task_output SET truncated = 1 WHERE task_id = ?`, taskID); err != nil {
			return false, fmt.Errorf("deploy: mark the output of the task %s as truncated: %w", taskID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("deploy: commit the output of the task %s: %w", taskID, err)
	}
	closed = true
	return affected == 1, nil
}

// Output returns the output of a task.
//
// The output holds no configuration: the fragments are the output of the
// command line of Docker, never the document of an application.
func (s *Service) Output(ctx context.Context, taskID string) (Output, error) {
	if _, err := s.task(ctx, taskID); err != nil {
		return Output{}, err
	}

	rows, err := s.db.QueryContext(ctx,
		`SELECT offset, data, truncated FROM task_output WHERE task_id = ? ORDER BY offset`, taskID)
	if err != nil {
		return Output{}, fmt.Errorf("deploy: read the output of the task %s: %w", taskID, err)
	}
	defer rows.Close()

	output := Output{TaskID: taskID, Fragments: []OutputFragment{}}
	for rows.Next() {
		var fragment OutputFragment
		var truncated int64
		if err := rows.Scan(&fragment.Offset, &fragment.Data, &truncated); err != nil {
			return Output{}, fmt.Errorf("deploy: read the output of the task %s: %w", taskID, err)
		}
		fragment.Truncated = truncated != 0
		output.TotalBytes += int64(len(fragment.Data))
		if fragment.Truncated {
			output.Truncated = true
		}
		output.Fragments = append(output.Fragments, fragment)
	}
	if err := rows.Err(); err != nil {
		return Output{}, fmt.Errorf("deploy: read the output of the task %s: %w", taskID, err)
	}
	return output, nil
}
