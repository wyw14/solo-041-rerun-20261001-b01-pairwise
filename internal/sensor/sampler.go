package sensor

import (
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/wyw14/cry-97/internal/model"
)

type Sampler struct {
	mu        sync.RWMutex
	latest    map[model.LineID]map[model.SampleKind]model.Sample
	byID      map[uuid.UUID]model.Sample
	listeners []func(model.Sample)
}

func NewSampler() *Sampler {
	return &Sampler{
		latest: make(map[model.LineID]map[model.SampleKind]model.Sample),
		byID:   make(map[uuid.UUID]model.Sample),
	}
}

func (s *Sampler) Subscribe(listener func(model.Sample)) error {
	if listener == nil {
		return errors.New("sample listener is required")
	}
	s.mu.Lock()
	s.listeners = append(s.listeners, listener)
	s.mu.Unlock()
	return nil
}

func (s *Sampler) Publish(sample model.Sample) error {
	if sample.LineID == "" || sample.Kind == "" || sample.ObservedAt.Equal(time.Time{}) {
		return errors.New("sample identity is incomplete")
	}
	s.mu.Lock()
	s.byID[sample.ID] = sample
	byKind := s.latest[sample.LineID]
	if byKind == nil {
		byKind = make(map[model.SampleKind]model.Sample)
		s.latest[sample.LineID] = byKind
	}
	previous, exists := byKind[sample.Kind]
	if exists && sample.Sequence <= previous.Sequence {
		s.mu.Unlock()
		return nil
	}
	byKind[sample.Kind] = sample
	listeners := append([]func(model.Sample){}, s.listeners...)
	s.mu.Unlock()
	for _, listener := range listeners {
		listener(sample)
	}
	return nil
}

// Sample returns the sample previously accepted under id, together with the
// processing line it was submitted to. The line identity comes from the
// accepted sample record and can therefore be used to verify that a later lab
// result still refers to the same line.
func (s *Sampler) Sample(id uuid.UUID) (model.Sample, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sample, ok := s.byID[id]
	return sample, ok
}

func (s *Sampler) LineSnapshot(lineID model.LineID) []model.Sample {
	s.mu.RLock()
	defer s.mu.RUnlock()
	byKind := s.latest[lineID]
	result := make([]model.Sample, 0, len(byKind))
	for _, sample := range byKind {
		result = append(result, sample)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Kind < result[j].Kind })
	return result
}
