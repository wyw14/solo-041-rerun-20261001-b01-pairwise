package process

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/wyw14/cry-97/internal/discharge"
	"github.com/wyw14/cry-97/internal/lab"
	"github.com/wyw14/cry-97/internal/model"
)

type QualificationOutcome struct {
	Batch  model.Batch       `json:"batch"`
	Permit *discharge.Permit `json:"permit,omitempty"`
}

func (p *Plant) SubmitLabPayload(ctx context.Context, payload []byte) (QualificationOutcome, error) {
	now := p.now()
	result, decodeErr := lab.Decode(payload, now)
	outcome, applyErr := p.ApplyLabResult(ctx, result, now)
	if decodeErr != nil {
		return QualificationOutcome{}, decodeErr
	}
	if applyErr != nil {
		return QualificationOutcome{}, applyErr
	}
	return outcome, nil
}

func (p *Plant) ApplyLabResult(ctx context.Context, result model.LabResult, now time.Time) (QualificationOutcome, error) {
	if err := ctx.Err(); err != nil {
		return QualificationOutcome{}, err
	}
	if !result.Valid || result.BatchID == uuid.Nil {
		return QualificationOutcome{}, errors.New("invalid lab result cannot qualify a batch")
	}

	// The whole qualification is one transaction: all identity relationships
	// between sample, line, batch and permit are verified before any state is
	// touched, and the valve/permit are actuated before the batch is committed.
	// A rejected result therefore leaves every line's batches, permits and
	// valves exactly as they were.
	p.mu.Lock()
	defer p.mu.Unlock()

	batch, exists := p.batches[result.BatchID]
	if !exists {
		return QualificationOutcome{}, errors.New("lab result batch is not found")
	}
	line, lineExists := p.lines[batch.LineID]
	if !lineExists {
		return QualificationOutcome{}, errors.New("batch process line is not found")
	}
	if result.LineID != batch.LineID || result.LineID != line.ID {
		return QualificationOutcome{}, errors.New("lab result does not belong to batch line")
	}
	sample, sampleRegistered := p.sampler.Sample(result.SampleID)
	if !sampleRegistered {
		return QualificationOutcome{}, errors.New("lab result sample is not registered")
	}
	if sample.LineID != result.LineID {
		return QualificationOutcome{}, errors.New("lab result does not belong to sample line")
	}

	// Pure computation: ApplyQualification returns either the next batch
	// state or an error without mutating the stored batch.
	updated, err := batch.ApplyQualification(result, now)
	if err != nil {
		return QualificationOutcome{}, err
	}

	// Record the result first so a stale revision is rejected before the
	// batch or any valve moves. It is rolled back if a later step fails.
	notice, err := p.labResults.Apply(result)
	if err != nil {
		return QualificationOutcome{}, err
	}

	// Actuate the permit/valve while nothing is committed yet. An unqualified
	// result that merely leaves the valve closed is the single benign outcome
	// recorded without a permit; every other error aborts the transaction.
	permit, permitErr := p.permits.ApplyResult(result, now)
	if permitErr != nil && (result.Qualified() || !errors.Is(permitErr, discharge.ErrNotQualified)) {
		p.labResults.Revert(notice)
		return QualificationOutcome{}, permitErr
	}

	p.batches[result.BatchID] = updated
	line.Stage = updated.Stage
	line.UpdatedAt = now.UTC()
	p.lines[updated.LineID] = line

	outcome := QualificationOutcome{Batch: updated}
	if permitErr == nil {
		outcome.Permit = &permit
	}
	return outcome, nil
}

func (p *Plant) Permits() []discharge.Permit {
	return p.permits.All()
}
