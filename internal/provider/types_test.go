package provider

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestUsageZeroValueMarshal(t *testing.T) {
	var u Usage
	b, err := json.Marshal(u)
	if err != nil {
		t.Fatalf("zero Usage marshal error: %v", err)
	}
	if len(b) == 0 {
		t.Fatal("zero Usage marshal produced empty output")
	}
}

func TestUsageMarshalFields(t *testing.T) {
	ts := time.Date(2026, 8, 12, 10, 0, 0, 0, time.UTC)
	u := Usage{
		BalanceType: "balance",
		Balance: &Money{
			Amount:   "12.34",
			Currency: "CNY",
		},
		Quotas: []Quota{
			{
				Period:    "monthly",
				Used:      "100",
				Limit:     "1000",
				Remaining: "900",
				Percent:   "90",
				ResetAt:   ts,
			},
		},
		Plan:      &PlanInfo{Level: "LEVEL_INTERMEDIATE"},
		Raw:       json.RawMessage(`{"foo":"bar"}`),
		UpdatedAt: ts,
	}
	b, err := json.Marshal(u)
	if err != nil {
		t.Fatalf("marshal error: %v", err)
	}
	s := string(b)
	for _, want := range []string{
		`"balanceType":"balance"`,
		`"amount":"12.34"`,
		`"currency":"CNY"`,
		`"period":"monthly"`,
		`"used":"100"`,
		`"limit":"1000"`,
		`"remaining":"900"`,
		`"percent":"90"`,
		`"level":"LEVEL_INTERMEDIATE"`,
		`"foo":"bar"`,
		`"updatedAt":"2026-08-12T10:00:00Z"`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("marshaled output missing %q; got: %s", want, s)
		}
	}
	omittedWhenNil := []string{`"error"`, `"balance":null`}
	for _, notWant := range omittedWhenNil {
		if strings.Contains(s, notWant) {
			t.Errorf("marshaled output should not contain %q; got: %s", notWant, s)
		}
	}
}

func TestUsageNilFieldsOmit(t *testing.T) {
	u := Usage{BalanceType: "quota"}
	b, err := json.Marshal(u)
	if err != nil {
		t.Fatalf("marshal error: %v", err)
	}
	s := string(b)
	if strings.Contains(s, `"balance"`) || strings.Contains(s, `"quotas"`) || strings.Contains(s, `"plan"`) {
		t.Errorf("nil fields should be omitted; got: %s", s)
	}
}

func TestPlanMetaStatic(t *testing.T) {
	m := PlanMeta{PlanName: "pro", PlanPrice: "100", ApiPrice: "0.01"}
	if m.PlanName != "pro" || m.PlanPrice != "100" || m.ApiPrice != "0.01" {
		t.Errorf("PlanMeta fields not set correctly: %+v", m)
	}
}
