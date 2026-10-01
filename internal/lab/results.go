package lab

import (
	"errors"
	"sync"

	"github.com/google/uuid"
	"github.com/wyw14/cry-97/internal/model"
)

type RevisionNotice struct {
	Previous model.LabResult
	Current  model.LabResult
	Revised  bool
}

type Results struct {
	mu       sync.RWMutex
	bySample map[uuid.UUID]model.LabResult
}

func NewResults() *Results {
	return &Results{bySample: make(map[uuid.UUID]model.LabResult)}
}

func (r *Results) Apply(result model.LabResult) (RevisionNotice, error) {
	if !result.Valid || result.SampleID == uuid.Nil || result.Revision == 0 {
		return RevisionNotice{}, errors.New("lab result is invalid")
	}
	r.mu.Lock()
	previous, exists := r.bySample[result.SampleID]
	if exists && !result.NewerThan(previous) {
		r.mu.Unlock()
		return RevisionNotice{}, errors.New("lab result revision is stale")
	}
	r.bySample[result.SampleID] = result
	r.mu.Unlock()
	notice := RevisionNotice{Previous: previous, Current: result, Revised: exists}
	return notice, nil
}

func (r *Results) Get(sampleID uuid.UUID) (model.LabResult, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	result, ok := r.bySample[sampleID]
	return result, ok
}

// Rollback reverses an Apply described by notice, restoring the previous
// entry (or removing a newly inserted one).
func (r *Results) Rollback(notice RevisionNotice) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if notice.Revised {
		r.bySample[notice.Current.SampleID] = notice.Previous
	} else {
		delete(r.bySample, notice.Current.SampleID)
	}
}
