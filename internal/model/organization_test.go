package model

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestOrganizationKeepsUnknownFields(t *testing.T) {
	var o Organization
	if err := json.Unmarshal([]byte(`{"id":"o","slug":"acme","name":"Acme","members":{"u":"owner"},"stripe_customer":"cus_old","monthly_cap_micros":5}`), &o); err != nil {
		t.Fatal(err)
	}
	o.Name = "Renamed"
	b, err := json.Marshal(o)
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)
	if !strings.Contains(got, `"stripe_customer":"cus_old"`) || !strings.Contains(got, `"monthly_cap_micros":5`) || !strings.Contains(got, `"name":"Renamed"`) || strings.Contains(got, "Acme\"") {
		t.Fatal(got)
	}
	b, err = json.Marshal(Organization{ID: "plain"})
	if err != nil || strings.Contains(string(b), "stripe") {
		t.Fatal(string(b), err)
	}
}
