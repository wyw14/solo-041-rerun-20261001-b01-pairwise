package model

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestBatchApplyQualificationRejectsForeignIdentity(t *testing.T) {
	now := time.Now().UTC()
	batch := NewBatch("A", now)
	batch.Stage = StageSettling

	result := LabResult{SampleID: uuid.New(), LineID: "A", BatchID: batch.ID, Revision: 1, Valid: true, COD: 10, Ammonia: 1}

	// Result claiming another line is rejected.
	forged := result
	forged.LineID = "B"
	if _, err := batch.ApplyQualification(forged, now); err == nil {
		t.Fatal("foreign line result accepted")
	}

	// Result citing another batch ID is rejected even if the line matches.
	forged = result
	forged.BatchID = uuid.New()
	if _, err := batch.ApplyQualification(forged, now); err == nil {
		t.Fatal("foreign batch result accepted")
	}

	// A consistent, qualified result applies and advances the batch.
	updated, err := batch.ApplyQualification(result, now)
	if err != nil {
		t.Fatalf("consistent result rejected: %v", err)
	}
	if !updated.Qualified || updated.Stage != StageQualified {
		t.Fatalf("batch not qualified: %+v", updated)
	}
	if batch.Qualified {
		t.Fatal("receiver was mutated by value method")
	}

	// A failing retest on the same line moves it back to settling.
	failing := result
	failing.Revision = 2
	failing.COD = 90
	updated, err = updated.ApplyQualification(failing, now)
	if err != nil {
		t.Fatalf("legal retest rejected: %v", err)
	}
	if updated.Qualified || updated.Stage != StageSettling {
		t.Fatalf("retest did not reset qualification: %+v", updated)
	}

	// A stale revision is rejected and leaves state intact.
	stale := result
	if _, err := updated.ApplyQualification(stale, now); err == nil {
		t.Fatal("stale revision accepted")
	}
}
