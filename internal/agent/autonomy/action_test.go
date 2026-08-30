package autonomy

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestActionReservationIdempotencyUncertaintyAndRedaction(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	intent := ActionIntent{ID: "a1", ObjectiveID: "o1", Kind: "external.call", Consequence: ExternalEffect, PolicyHash: "h", Request: json.RawMessage(`{"token":"burn-me-not","nested":{"password":"hide"},"safe":"ok"}`), IdempotencyKey: "key1"}
	got, fresh, err := s.ReserveAction(ctx, intent)
	if err != nil || !fresh || got.Status != string(ActionReserved) {
		t.Fatalf("action=%+v fresh=%v err=%v", got, fresh, err)
	}
	intent.ID = "a2"
	existing, fresh, err := s.ReserveAction(ctx, intent)
	if err != nil || fresh || existing.ID != "a1" {
		t.Fatalf("existing=%+v fresh=%v err=%v", existing, fresh, err)
	}
	var request string
	if err := s.db.QueryRowContext(ctx, `SELECT request_json FROM actions WHERE id='a1'`).Scan(&request); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(request, "burn-me-not") || strings.Contains(request, "hide") || !strings.Contains(request, "[redacted]") {
		t.Fatalf("request not redacted: %s", request)
	}
	if err := s.MarkActionRunning(ctx, "a1"); err != nil {
		t.Fatal(err)
	}
	if err := s.CompleteAction(ctx, "a1", ActionUncertain, json.RawMessage(`{"state":"unknown"}`), nil); err != nil {
		t.Fatal(err)
	}
	var status string
	if err := s.db.QueryRowContext(ctx, `SELECT status FROM actions WHERE id='a1'`).Scan(&status); err != nil || status != string(ActionUncertain) {
		t.Fatalf("status=%q err=%v", status, err)
	}
}
