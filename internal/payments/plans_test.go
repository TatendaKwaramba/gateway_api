package payments

import (
	"testing"
)

func TestPlanStruct_HasPlanType(t *testing.T) {
	p := Plan{
		ID:        1,
		Name:      "Test Plan",
		Price:     5000,
		PlanType:  "subscription",
		Currency:  "ZAR",
	}

	if p.PlanType != "subscription" {
		t.Errorf("expected PlanType 'subscription', got %q", p.PlanType)
	}
}

func TestPlanStruct_HotspotType(t *testing.T) {
	p := Plan{
		ID:       1,
		Name:     "Hotspot Plan",
		Price:    1000,
		PlanType: "hotspot",
	}

	if p.PlanType != "hotspot" {
		t.Errorf("expected PlanType 'hotspot', got %q", p.PlanType)
	}
}

func TestPlanStruct_DisplayAmount(t *testing.T) {
	p := Plan{
		Price:         5000,
		DisplayAmount: float64(5000) / 100.0,
	}

	if p.DisplayAmount != 50.0 {
		t.Errorf("expected DisplayAmount 50.0, got %f", p.DisplayAmount)
	}
}

func TestPlanStruct_SubscriptionFields(t *testing.T) {
	p := Plan{
		ID:               1,
		Name:             "PPPoE Standard",
		Price:            5000,
		Currency:         "ZAR",
		DurationDays:     30,
		DownloadSpeed:    10240,
		UploadSpeed:      5120,
		MaxSessions:      1,
		FupDataQuotaMb:   0,
		PlanType:         "subscription",
	}

	if p.PlanType != "subscription" {
		t.Errorf("expected PlanType 'subscription', got %q", p.PlanType)
	}
	if p.DurationDays != 30 {
		t.Errorf("expected DurationDays 30, got %d", p.DurationDays)
	}
	if p.DownloadSpeed != 10240 {
		t.Errorf("expected DownloadSpeed 10240, got %d", p.DownloadSpeed)
	}
}
