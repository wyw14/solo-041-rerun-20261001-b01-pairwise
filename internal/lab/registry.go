package lab

import (
	"errors"
	"sync"

	"github.com/google/uuid"
	"github.com/wyw14/cry-97/internal/model"
)

// Registry records the process line on which each physical sample was
// collected. It is the trusted source of a sample's identity: a lab result
// may only act on the line its sample came from.
type Registry struct {
	mu      sync.RWMutex
	samples map[uuid.UUID]model.Sample
}

func NewRegistry() *Registry {
	return &Registry{samples: make(map[uuid.UUID]model.Sample)}
}

// Bind records that sample was collected on sample.LineID. Re-binding the
// same sample to the same line is idempotent; binding it to a different line
// is rejected.
func (r *Registry) Bind(sample model.Sample) error {
	if sample.ID == uuid.Nil || sample.LineID == "" {
		return errors.New("sample identity is incomplete")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if existing, ok := r.samples[sample.ID]; ok && existing.LineID != sample.LineID {
		return errors.New("sample is already bound to another process line")
	}
	r.samples[sample.ID] = sample
	return nil
}

// LineOf returns the line a registered sample was collected on.
func (r *Registry) LineOf(sampleID uuid.UUID) (model.LineID, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	sample, ok := r.samples[sampleID]
	return sample.LineID, ok
}
