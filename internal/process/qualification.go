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
	if decodeErr != nil {
		return QualificationOutcome{}, decodeErr
	}
	return p.ApplyLabResult(ctx, result, now)
}

func (p *Plant) ApplyLabResult(ctx context.Context, result model.LabResult, now time.Time) (QualificationOutcome, error) {
	if err := ctx.Err(); err != nil {
		return QualificationOutcome{}, err
	}
	if !result.Valid || result.SampleID == uuid.Nil || result.BatchID == uuid.Nil ||
		result.LineID == "" || result.Revision == 0 {
		return QualificationOutcome{}, errors.New("invalid lab result cannot qualify a batch")
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	// Phase 1: verify the full identity chain sample -> line -> batch using
	// read-only checks only. A result whose line_id disagrees with the
	// sample's collection line, the batch's line, or the permit's line is
	// rejected before any state changes, so neither line's batches, permits
	// nor valves move.
	batch, exists := p.batches[result.BatchID]
	if !exists {
		return QualificationOutcome{}, errors.New("lab result batch is not found")
	}
	if batch.LineID != result.LineID {
		return QualificationOutcome{}, errors.New("lab result line does not match batch line")
	}
	line, exists := p.lines[result.LineID]
	if !exists {
		return QualificationOutcome{}, errors.New("lab result line is not configured")
	}
	if activeID, ok := p.lineBatches[result.LineID]; !ok || activeID != result.BatchID || !batch.IsActive() {
		return QualificationOutcome{}, errors.New("lab result batch is not the active batch of its line")
	}
	sampleLine, bound := p.sampleRegistry.LineOf(result.SampleID)
	if !bound {
		return QualificationOutcome{}, errors.New("lab result sample is not registered")
	}
	if sampleLine != result.LineID {
		return QualificationOutcome{}, errors.New("lab result sample was collected on another line")
	}
	if err := p.permits.Check(result); err != nil {
		return QualificationOutcome{}, err
	}

	// Phase 2: compute and commit. Batch.ApplyQualification rejects stale
	// revisions against batch bookkeeping before anything physical happens.
	updated, err := batch.ApplyQualification(result, now)
	if err != nil {
		return QualificationOutcome{}, err
	}
	notice, err := p.labResults.Apply(result)
	if err != nil {
		return QualificationOutcome{}, err
	}
	permit, permitErr := p.permits.ApplyResult(result, now)
	if permitErr != nil && !errors.Is(permitErr, discharge.ErrNotQualified) {
		// Keep the result ledger consistent with batch state: no batch,
		// line or valve changed, so neither may the ledger.
		p.labResults.Rollback(notice)
		return QualificationOutcome{}, permitErr
	}

	p.batches[result.BatchID] = updated
	line.Stage = updated.Stage
	line.UpdatedAt = now.UTC()
	p.lines[result.LineID] = line

	outcome := QualificationOutcome{Batch: updated}
	if permitErr == nil {
		outcome.Permit = &permit
	}
	return outcome, nil
}

func (p *Plant) Permits() []discharge.Permit {
	return p.permits.All()
}
