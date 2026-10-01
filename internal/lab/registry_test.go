package lab

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/wyw14/cry-97/internal/model"
)

func TestRegistryBindsSampleToLine(t *testing.T) {
	registry := NewRegistry()
	sample := model.Sample{ID: uuid.New(), LineID: "A", Kind: model.SampleCOD, ObservedAt: time.Now().UTC()}

	if err := registry.Bind(sample); err != nil {
		t.Fatalf("bind: %v", err)
	}
	line, ok := registry.LineOf(sample.ID)
	if !ok || line != "A" {
		t.Fatalf("sample bound to %q (found=%v)", line, ok)
	}

	// The same sample may be re-bound to its own line (idempotent replay).
	if err := registry.Bind(sample); err != nil {
		t.Fatalf("rebind same line: %v", err)
	}

	// Claiming the same sample came from another line must fail.
	forged := sample
	forged.LineID = "B"
	if err := registry.Bind(forged); err == nil {
		t.Fatal("sample rebound to a different line")
	}
	if line, _ := registry.LineOf(sample.ID); line != "A" {
		t.Fatalf("binding changed after rejected rebind: %q", line)
	}

	if _, ok := registry.LineOf(uuid.New()); ok {
		t.Fatal("unknown sample reported as bound")
	}
}

func TestResultsRollback(t *testing.T) {
	results := NewResults()
	result := model.LabResult{SampleID: uuid.New(), LineID: "A", Revision: 1, Valid: true}

	notice, err := results.Apply(result)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if notice.Revised {
		t.Fatal("first apply should not be a revision")
	}
	results.Rollback(notice)
	if _, ok := results.Get(result.SampleID); ok {
		t.Fatal("rollback did not remove newly inserted result")
	}

	if _, err := results.Apply(result); err != nil {
		t.Fatalf("re-apply: %v", err)
	}
	revised := result
	revised.Revision = 2
	notice, err = results.Apply(revised)
	if err != nil || !notice.Revised {
		t.Fatalf("revision apply: %v notice=%+v", err, notice)
	}
	results.Rollback(notice)
	stored, ok := results.Get(result.SampleID)
	if !ok || stored.Revision != 1 {
		t.Fatalf("rollback did not restore previous revision: %+v", stored)
	}
}
