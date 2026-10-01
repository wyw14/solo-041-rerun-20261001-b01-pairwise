package process

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/wyw14/cry-97/internal/model"
)

type plantSnap struct {
	batchA model.Batch
	batchB model.Batch
	lineA  model.LineState
	lineB  model.LineState
}

func snapPlant(t *testing.T, p *Plant, batchA, batchB uuid.UUID) plantSnap {
	t.Helper()
	p.mu.RLock()
	defer p.mu.RUnlock()
	return plantSnap{
		batchA: p.batches[batchA],
		batchB: p.batches[batchB],
		lineA:  p.lines["line-1"],
		lineB:  p.lines["line-2"],
	}
}

func valveByLine(p *Plant, lineID model.LineID) (bool, uuid.UUID) {
	valves := p.valves.Snapshot()
	for _, valve := range valves {
		if valve.LineID == lineID {
			return valve.Open, valve.PermitID
		}
	}
	return false, uuid.Nil
}

func settleBatch(t *testing.T, p *Plant, lineID model.LineID) {
	t.Helper()
	ctx := context.Background()
	for _, stage := range []model.ProcessStage{model.StageEqualizing, model.StageBiological, model.StageSettling} {
		if _, err := p.AdvanceBatch(ctx, lineID, stage); err != nil {
			t.Fatalf("advance %s to %s: %v", lineID, stage, err)
		}
	}
}

// A lab result submitted for line A's batch but carrying line_id=B must be
// rejected outright: neither A's batch nor B's permit/valve may move. Legit
// results and retests on A still update A, and never touch B.
func TestApplyLabResultLineIdentity(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 30, 10, 0, 0, 0, time.UTC)
	plant, err := NewPlant(t.TempDir(), []model.LineID{"line-1", "line-2"}, func() time.Time { return now })
	if err != nil {
		t.Fatalf("new plant: %v", err)
	}

	batchA, err := plant.StartBatch(ctx, "line-1", 760, "north-intake")
	if err != nil {
		t.Fatalf("start batch A: %v", err)
	}
	batchB, err := plant.StartBatch(ctx, "line-2", 760, "north-intake")
	if err != nil {
		t.Fatalf("start batch B: %v", err)
	}
	settleBatch(t, plant, "line-1")
	settleBatch(t, plant, "line-2")

	registerSample := func(lineID model.LineID, sequence uint64) model.Sample {
		sample := model.NewSample(lineID, "basin-"+string(lineID), model.SampleCOD, sequence, 30, "mg/L", now)
		if err := plant.ReceiveSample(ctx, sample); err != nil {
			t.Fatalf("receive sample on %s: %v", lineID, err)
		}
		return sample
	}
	sampleA := registerSample("line-1", 1)
	sampleB := registerSample("line-2", 1)

	qualified := func(sampleID uuid.UUID, lineID model.LineID, batchID uuid.UUID, revision uint64, cod, ammonia float64) model.LabResult {
		return model.LabResult{
			SampleID: sampleID, LineID: lineID, BatchID: batchID, Revision: revision,
			COD: cod, Ammonia: ammonia, Valid: true, ReceivedAt: now,
		}
	}

	assertUntouched := func(label string, before plantSnap) {
		t.Helper()
		after := snapPlant(t, plant, batchA.ID, batchB.ID)
		if after.batchA != before.batchA {
			t.Fatalf("%s: batch A changed: before=%+v after=%+v", label, before.batchA, after.batchA)
		}
		if after.batchB != before.batchB {
			t.Fatalf("%s: batch B changed: before=%+v after=%+v", label, before.batchB, after.batchB)
		}
		if after.lineA != before.lineA {
			t.Fatalf("%s: line A state changed", label)
		}
		if after.lineB != before.lineB {
			t.Fatalf("%s: line B state changed", label)
		}
		if openA, permitA := valveByLine(plant, "line-1"); openA || permitA != uuid.Nil {
			t.Fatalf("%s: valve A moved: open=%v permit=%s", label, openA, permitA)
		}
		if openB, permitB := valveByLine(plant, "line-2"); openB || permitB != uuid.Nil {
			t.Fatalf("%s: valve B moved: open=%v permit=%s", label, openB, permitB)
		}
		if _, found := plant.permits.Current(batchA.ID); found {
			t.Fatalf("%s: permit for batch A appeared", label)
		}
		if _, found := plant.permits.Current(batchB.ID); found {
			t.Fatalf("%s: permit for batch B appeared", label)
		}
	}

	// 1. A's batch + A's sample, but line_id says B: rejected, nothing moves.
	before := snapPlant(t, plant, batchA.ID, batchB.ID)
	mismatched := qualified(sampleA.ID, "line-2", batchA.ID, 1, 10, 1)
	if _, err := plant.ApplyLabResult(ctx, mismatched, now); err == nil {
		t.Fatal("cross-line result (A sample/batch, line_id=B) was accepted")
	}
	assertUntouched("cross-line result", before)
	if _, found := plant.labResults.Get(sampleA.ID); found {
		t.Fatal("rejected cross-line result was recorded as a lab result")
	}

	// The same attack through the raw JSON payload endpoint must also fail.
	payload, _ := json.Marshal(map[string]any{
		"sample_id": sampleA.ID.String(), "line_id": "line-2",
		"batch_id": batchA.ID.String(), "revision": 1, "cod": 10, "ammonia": 1,
	})
	if _, err := plant.SubmitLabPayload(ctx, payload); err == nil {
		t.Fatal("cross-line payload was accepted")
	}
	assertUntouched("cross-line payload", before)

	// 2. Genuine sample on B but batch A with line_id=B (sample/line agree,
	// batch disagrees): still rejected with no side effects on either line.
	batchSwap := qualified(sampleB.ID, "line-2", batchA.ID, 1, 10, 1)
	if _, err := plant.ApplyLabResult(ctx, batchSwap, now); err == nil {
		t.Fatal("result linking B sample/line to batch A was accepted")
	}
	assertUntouched("batch/line swap", before)

	// 3. Legit qualified result on A: A qualifies and only A's valve opens.
	outcome, err := plant.ApplyLabResult(ctx, qualified(sampleA.ID, "line-1", batchA.ID, 1, 10, 1), now)
	if err != nil {
		t.Fatalf("legit qualification: %v", err)
	}
	if !outcome.Batch.Qualified || outcome.Batch.Stage != model.StageQualified {
		t.Fatalf("batch A not qualified: %+v", outcome.Batch)
	}
	if outcome.Permit == nil || !outcome.Permit.Active || outcome.Permit.LineID != "line-1" {
		t.Fatalf("permit A wrong: %+v", outcome.Permit)
	}
	if openA, permitA := valveByLine(plant, "line-1"); !openA || permitA != outcome.Permit.ID {
		t.Fatalf("valve A not opened by permit %s: open=%v id=%s", outcome.Permit.ID, openA, permitA)
	}
	if openB, permitB := valveByLine(plant, "line-2"); openB || permitB != uuid.Nil {
		t.Fatalf("valve B moved after A qualified: open=%v permit=%s", openB, permitB)
	}
	qualifiedPermitID := outcome.Permit.ID

	// 4. While A is qualified, retry the cross-line forgery with a newer
	// revision: A keeps its permit and B's valve stays shut.
	afterQual := snapPlant(t, plant, batchA.ID, batchB.ID)
	forgedRevision := qualified(sampleA.ID, "line-2", batchA.ID, 4, 5, 1)
	if _, err := plant.ApplyLabResult(ctx, forgedRevision, now); err == nil {
		t.Fatal("cross-line retest forgery was accepted")
	}
	if got := snapPlant(t, plant, batchA.ID, batchB.ID); got != afterQual {
		t.Fatal("cross-line retest forgery mutated plant state")
	}
	if openB, permitB := valveByLine(plant, "line-2"); openB || permitB != uuid.Nil {
		t.Fatalf("valve B opened by forged retest: open=%v permit=%s", openB, permitB)
	}

	// 5. Legit retest fails quality: A returns to settling, valve A held,
	// permit revoked; B is still untouched.
	if _, err := plant.ApplyLabResult(ctx, qualified(sampleA.ID, "line-1", batchA.ID, 2, 60, 1), now); err != nil {
		t.Fatalf("unqualified retest: %v", err)
	}
	currentA, _ := plant.permits.Current(batchA.ID)
	if currentA.Active {
		t.Fatal("permit A still active after failing retest")
	}
	if openA, _ := valveByLine(plant, "line-1"); openA {
		t.Fatal("valve A stayed open after failing retest")
	}
	storedA := snapPlant(t, plant, batchA.ID, batchB.ID).batchA
	if storedA.Qualified || storedA.Stage != model.StageSettling {
		t.Fatalf("batch A did not leave qualified state: %+v", storedA)
	}
	if openB, permitB := valveByLine(plant, "line-2"); openB || permitB != uuid.Nil {
		t.Fatalf("valve B moved during A retest: open=%v permit=%s", openB, permitB)
	}

	// 6. Legit passing retest requalifies A and opens a fresh A permit.
	outcome, err = plant.ApplyLabResult(ctx, qualified(sampleA.ID, "line-1", batchA.ID, 3, 20, 2), now)
	if err != nil {
		t.Fatalf("requalifying retest: %v", err)
	}
	if !outcome.Batch.Qualified || outcome.Permit == nil || !outcome.Permit.Active {
		t.Fatalf("batch A not requalified: %+v", outcome)
	}
	if outcome.Permit.ID == qualifiedPermitID {
		t.Fatal("requalification reused the revoked permit instead of issuing a new one")
	}
	if openA, permitA := valveByLine(plant, "line-1"); !openA || permitA != outcome.Permit.ID {
		t.Fatalf("valve A not opened by new permit: open=%v id=%s", openA, permitA)
	}

	// B finished the whole sequence without a single state change.
	finalB := snapPlant(t, plant, batchA.ID, batchB.ID).batchB
	if finalB.Qualified || finalB.Stage != model.StageSettling || finalB.QualificationSample != uuid.Nil {
		t.Fatalf("batch B was modified: %+v", finalB)
	}
	if _, found := plant.permits.Current(batchB.ID); found {
		t.Fatal("batch B acquired a permit")
	}
	if openB, permitB := valveByLine(plant, "line-2"); openB || permitB != uuid.Nil {
		t.Fatalf("valve B open at end: open=%v permit=%s", openB, permitB)
	}

	// Stale revisions remain rejected and do not move the valve.
	finalSnap := snapPlant(t, plant, batchA.ID, batchB.ID)
	if _, err := plant.ApplyLabResult(ctx, qualified(sampleA.ID, "line-1", batchA.ID, 1, 5, 1), now); err == nil {
		t.Fatal("stale revision was accepted")
	}
	if got := snapPlant(t, plant, batchA.ID, batchB.ID); got != finalSnap {
		t.Fatal("stale revision mutated plant state")
	}
	if openA, _ := valveByLine(plant, "line-1"); !openA {
		t.Fatal("stale revision moved valve A")
	}
}
