package discharge

import (
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/wyw14/cry-97/internal/model"
)

// ErrNotQualified reports that a valid result does not qualify for discharge
// and that no previously active permit needed to be revoked. It is not a
// reason to reject the result for the batch.
var ErrNotQualified = errors.New("result is not qualified for discharge")

type Permit struct {
	ID             uuid.UUID    `json:"id"`
	LineID         model.LineID `json:"line_id"`
	BatchID        uuid.UUID    `json:"batch_id"`
	SampleID       uuid.UUID    `json:"sample_id"`
	SampleRevision uint64       `json:"sample_revision"`
	Active         bool         `json:"active"`
	IssuedAt       time.Time    `json:"issued_at"`
	RevokedAt      *time.Time   `json:"revoked_at,omitempty"`
}

type PermitBook struct {
	mu      sync.RWMutex
	byBatch map[uuid.UUID]Permit
	valves  *ValveController
}

func NewPermitBook(valves *ValveController) (*PermitBook, error) {
	if valves == nil {
		return nil, errors.New("permit book requires valve controller")
	}
	return &PermitBook{byBatch: make(map[uuid.UUID]Permit), valves: valves}, nil
}

// Check validates a result without changing any permit or valve state.
func (b *PermitBook) Check(result model.LabResult) error {
	if !result.Valid || result.SampleID == uuid.Nil || result.BatchID == uuid.Nil || result.Revision == 0 {
		return errors.New("permit result is invalid")
	}
	if result.LineID == "" {
		return errors.New("permit result line is missing")
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	current, exists := b.byBatch[result.BatchID]
	if exists && current.LineID != result.LineID {
		return errors.New("permit result line does not match permit line")
	}
	if exists && current.SampleID == result.SampleID && result.Revision <= current.SampleRevision {
		return errors.New("permit result revision is stale")
	}
	return nil
}

// ApplyResult updates the permit for the result's batch and drives that
// batch's line valve. A batch's permit is always bound to the batch's own
// line; callers must have verified result.LineID against the batch. Permit
// book state is restored if the valve cannot be driven, so a failure never
// leaves permit and valve disagreeing.
func (b *PermitBook) ApplyResult(result model.LabResult, now time.Time) (Permit, error) {
	if err := b.Check(result); err != nil {
		return Permit{}, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	current, exists := b.byBatch[result.BatchID]
	if !result.Qualified() {
		if exists && current.Active {
			previous := current
			stamp := now.UTC()
			current.Active = false
			current.SampleID = result.SampleID
			current.SampleRevision = result.Revision
			current.RevokedAt = &stamp
			b.byBatch[result.BatchID] = current
			if err := b.valves.Hold(result.LineID, now); err != nil {
				b.byBatch[result.BatchID] = previous
				return Permit{}, err
			}
			return current, nil
		}
		return Permit{}, ErrNotQualified
	}
	permit := Permit{
		ID: uuid.New(), LineID: result.LineID, BatchID: result.BatchID,
		SampleID: result.SampleID, SampleRevision: result.Revision, Active: true, IssuedAt: now.UTC(),
	}
	if err := b.valves.Open(result.LineID, permit.ID, now); err != nil {
		return Permit{}, err
	}
	b.byBatch[result.BatchID] = permit
	return permit, nil
}

func (b *PermitBook) Current(batchID uuid.UUID) (Permit, bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	permit, ok := b.byBatch[batchID]
	return permit, ok
}

func (b *PermitBook) All() []Permit {
	b.mu.RLock()
	defer b.mu.RUnlock()
	result := make([]Permit, 0, len(b.byBatch))
	for _, permit := range b.byBatch {
		result = append(result, permit)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].IssuedAt.After(result[j].IssuedAt) })
	return result
}
