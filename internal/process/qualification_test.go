package process

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/wyw14/cry-97/internal/discharge"
	"github.com/wyw14/cry-97/internal/model"
)

type testHarness struct {
	plant *Plant
	now   time.Time
}

func newTestPlant(t *testing.T) *testHarness {
	t.Helper()
	now := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	plant, err := NewPlant(t.TempDir(), []model.LineID{"A", "B"}, clock)
	if err != nil {
		t.Fatalf("new plant: %v", err)
	}
	return &testHarness{plant: plant, now: now}
}

func (h *testHarness) settleBatch(t *testing.T, lineID model.LineID) model.Batch {
	t.Helper()
	ctx := context.Background()
	batch, err := h.plant.StartBatch(ctx, lineID, 100, "influent")
	if err != nil {
		t.Fatalf("start batch on %s: %v", lineID, err)
	}
	for _, stage := range []model.ProcessStage{
		model.StageEqualizing, model.StageBiological, model.StageSettling,
	} {
		batch, err = h.plant.AdvanceBatch(ctx, lineID, stage)
		if err != nil {
			t.Fatalf("advance %s to %s: %v", lineID, stage, err)
		}
	}
	return batch
}

func (h *testHarness) registerSample(t *testing.T, lineID model.LineID, seq uint64) model.Sample {
	t.Helper()
	sample := model.NewSample(lineID, "basin-"+string(lineID), model.SampleCOD, seq, 12, "mg/L", h.now)
	if err := h.plant.ReceiveSample(context.Background(), sample); err != nil {
		t.Fatalf("receive sample on %s: %v", lineID, err)
	}
	return sample
}

func labPayload(t *testing.T, sample model.Sample, lineID model.LineID, batchID uuid.UUID, revision uint64, cod, ammonia float64) []byte {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"sample_id": sample.ID.String(),
		"line_id":   string(lineID),
		"batch_id":  batchID.String(),
		"revision":  revision,
		"cod":       cod,
		"ammonia":   ammonia,
	})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return payload
}

func (h *testHarness) valve(lineID model.LineID) discharge.Valve {
	for _, valve := range h.plant.valves.Snapshot() {
		if valve.LineID == lineID {
			return valve
		}
	}
	return discharge.Valve{}
}

// A result for line A's batch that claims line_id B must be rejected and
// leave A's batch, both lines' permits and both valves exactly as they were.
func TestApplyLabResultRejectsForgedLineIdentity(t *testing.T) {
	h := newTestPlant(t)
	batchA := h.settleBatch(t, "A")
	h.settleBatch(t, "B")
	sampleA := h.registerSample(t, "A", 1)

	got, err := h.plant.SubmitLabPayload(context.Background(),
		labPayload(t, sampleA, "B", batchA.ID, 1, 10, 1))
	if err == nil {
		t.Fatalf("forged line_id accepted: %+v", got)
	}

	if updated, ok := h.plant.ActiveBatch("A"); !ok || updated.Qualified || updated.Stage != model.StageSettling {
		t.Fatalf("line A batch changed after rejected result: %+v", updated)
	}
	if updated, ok := h.plant.ActiveBatch("B"); !ok || updated.Qualified || updated.Stage != model.StageSettling {
		t.Fatalf("line B batch changed after rejected result: %+v", updated)
	}
	if permits := h.plant.Permits(); len(permits) != 0 {
		t.Fatalf("permits created despite rejection: %+v", permits)
	}
	if valve := h.valve("A"); valve.Open {
		t.Fatalf("line A valve opened despite rejection: %+v", valve)
	}
	if valve := h.valve("B"); valve.Open {
		t.Fatalf("line B valve opened by forged result: %+v", valve)
	}
	if _, recorded := h.plant.labResults.Get(sampleA.ID); recorded {
		t.Fatal("rejected result was recorded in the lab ledger")
	}
}

// A result pairing a sample collected on A with B's batch must be rejected
// without touching B's batch or B's valve.
func TestApplyLabResultRejectsSampleFromAnotherLine(t *testing.T) {
	h := newTestPlant(t)
	h.settleBatch(t, "A")
	batchB := h.settleBatch(t, "B")
	sampleA := h.registerSample(t, "A", 1)

	_, err := h.plant.SubmitLabPayload(context.Background(),
		labPayload(t, sampleA, "B", batchB.ID, 1, 10, 1))
	if err == nil {
		t.Fatal("result with sample from another line was accepted")
	}
	if updated, ok := h.plant.ActiveBatch("B"); !ok || updated.Qualified || updated.Stage != model.StageSettling {
		t.Fatalf("line B batch changed: %+v", updated)
	}
	if valve := h.valve("B"); valve.Open {
		t.Fatalf("line B valve opened by foreign sample: %+v", valve)
	}
}

// A result citing a sample that was never received cannot qualify anything.
func TestApplyLabResultRejectsUnknownSample(t *testing.T) {
	h := newTestPlant(t)
	batchA := h.settleBatch(t, "A")
	unknown := model.NewSample("A", "basin-A", model.SampleCOD, 99, 12, "mg/L", h.now)

	_, err := h.plant.SubmitLabPayload(context.Background(),
		labPayload(t, unknown, "A", batchA.ID, 1, 10, 1))
	if err == nil {
		t.Fatal("result for unregistered sample was accepted")
	}
	if valve := h.valve("A"); valve.Open {
		t.Fatalf("line A valve opened for unknown sample: %+v", valve)
	}
}

// A matching, qualified result qualifies the batch, issues a permit and
// opens only that line's valve.
func TestApplyLabResultAcceptsConsistentQualifiedResult(t *testing.T) {
	h := newTestPlant(t)
	batchA := h.settleBatch(t, "A")
	h.settleBatch(t, "B")
	sampleA := h.registerSample(t, "A", 1)

	outcome, err := h.plant.SubmitLabPayload(context.Background(),
		labPayload(t, sampleA, "A", batchA.ID, 1, 42, 3))
	if err != nil {
		t.Fatalf("consistent result rejected: %v", err)
	}
	if !outcome.Batch.Qualified || outcome.Batch.Stage != model.StageQualified {
		t.Fatalf("batch not qualified: %+v", outcome.Batch)
	}
	if outcome.Permit == nil || !outcome.Permit.Active || outcome.Permit.LineID != "A" {
		t.Fatalf("permit not issued on line A: %+v", outcome.Permit)
	}
	if valve := h.valve("A"); !valve.Open || valve.PermitID != outcome.Permit.ID {
		t.Fatalf("line A valve not opened by issued permit: %+v", valve)
	}
	if valve := h.valve("B"); valve.Open {
		t.Fatalf("line B valve opened unexpectedly: %+v", valve)
	}
}

// A later revision of the same sample may overturn qualification: the batch
// returns to settling, the permit is revoked and the valve closes.
func TestApplyLabResultAllowsLegalRetest(t *testing.T) {
	h := newTestPlant(t)
	batchA := h.settleBatch(t, "A")
	sampleA := h.registerSample(t, "A", 1)

	if _, err := h.plant.SubmitLabPayload(context.Background(),
		labPayload(t, sampleA, "A", batchA.ID, 1, 42, 3)); err != nil {
		t.Fatalf("initial result rejected: %v", err)
	}

	outcome, err := h.plant.SubmitLabPayload(context.Background(),
		labPayload(t, sampleA, "A", batchA.ID, 2, 80, 3))
	if err != nil {
		t.Fatalf("legal retest rejected: %v", err)
	}
	if outcome.Batch.Qualified || outcome.Batch.Stage != model.StageSettling {
		t.Fatalf("batch not returned to settling after failing retest: %+v", outcome.Batch)
	}
	if outcome.Permit == nil || outcome.Permit.Active || outcome.Permit.RevokedAt == nil {
		t.Fatalf("permit not revoked after failing retest: %+v", outcome.Permit)
	}
	if valve := h.valve("A"); valve.Open {
		t.Fatalf("line A valve remained open after failing retest: %+v", valve)
	}

	// A stale revision must not change anything.
	if _, err := h.plant.SubmitLabPayload(context.Background(),
		labPayload(t, sampleA, "A", batchA.ID, 1, 10, 1)); err == nil {
		t.Fatal("stale revision accepted")
	}
	updated, _ := h.plant.ActiveBatch("A")
	if updated.Qualified || updated.QualificationRev != 2 {
		t.Fatalf("stale revision altered batch state: %+v", updated)
	}
	if valve := h.valve("A"); valve.Open {
		t.Fatalf("stale revision reopened valve: %+v", valve)
	}
}

// A fresh sample with a passing result re-qualifies a batch after a failed
// retest, proving legitimate follow-up testing still works.
func TestApplyLabResultAllowsRequalificationWithNewSample(t *testing.T) {
	h := newTestPlant(t)
	batchA := h.settleBatch(t, "A")
	first := h.registerSample(t, "A", 1)

	if _, err := h.plant.SubmitLabPayload(context.Background(),
		labPayload(t, first, "A", batchA.ID, 1, 42, 3)); err != nil {
		t.Fatalf("initial result rejected: %v", err)
	}
	if _, err := h.plant.SubmitLabPayload(context.Background(),
		labPayload(t, first, "A", batchA.ID, 2, 80, 3)); err != nil {
		t.Fatalf("retest rejected: %v", err)
	}

	second := h.registerSample(t, "A", 2)
	outcome, err := h.plant.SubmitLabPayload(context.Background(),
		labPayload(t, second, "A", batchA.ID, 1, 20, 2))
	if err != nil {
		t.Fatalf("requalification rejected: %v", err)
	}
	if !outcome.Batch.Qualified || outcome.Batch.Stage != model.StageQualified {
		t.Fatalf("batch not requalified: %+v", outcome.Batch)
	}
	if outcome.Permit == nil || !outcome.Permit.Active {
		t.Fatalf("new permit not issued: %+v", outcome.Permit)
	}
	if valve := h.valve("A"); !valve.Open {
		t.Fatalf("line A valve not reopened: %+v", valve)
	}
}

func TestPermitBookRejectsResultLineMismatch(t *testing.T) {
	valves, err := discharge.NewValveController([]model.LineID{"A", "B"})
	if err != nil {
		t.Fatalf("valves: %v", err)
	}
	book, err := discharge.NewPermitBook(valves)
	if err != nil {
		t.Fatalf("permit book: %v", err)
	}
	now := time.Now().UTC()
	batchID := uuid.New()
	result := model.LabResult{
		SampleID: uuid.New(), LineID: "A", BatchID: batchID,
		Revision: 1, COD: 10, Ammonia: 1, Valid: true, ReceivedAt: now,
	}
	if _, err := book.ApplyResult(result, now); err != nil {
		t.Fatalf("initial permit: %v", err)
	}
	forged := result
	forged.LineID = "B"
	forged.Revision = 2
	err = book.Check(forged)
	if err == nil || errors.Is(err, discharge.ErrNotQualified) {
		t.Fatalf("line mismatch not rejected: %v", err)
	}
	for _, valve := range valves.Snapshot() {
		if valve.LineID == "B" && valve.Open {
			t.Fatal("line B valve opened by mismatched result")
		}
	}
}
