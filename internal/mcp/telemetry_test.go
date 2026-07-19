package mcp

import (
	"encoding/json"
	"testing"
)

func TestPublishSourceFramework(t *testing.T) {
	cases := []struct {
		name   string
		params string
		want   string
	}{
		{"absent", `{"title":"x"}`, ""},
		{"empty params", ``, ""},
		{"null params", `null`, ""},
		{"simple value", `{"sourceFramework":"langchain"}`, "langchain"},
		{"trimmed", `{"sourceFramework":"  crewai  "}`, "crewai"},
		{"blank after trim", `{"sourceFramework":"   "}`, ""},
		{"strips control chars", `{"sourceFramework":"lang\nchain\t"}`, "langchain"},
		{"invalid json", `{"sourceFramework":`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := publishSourceFramework(json.RawMessage(tc.params))
			if got != tc.want {
				t.Fatalf("publishSourceFramework(%s) = %q, want %q", tc.params, got, tc.want)
			}
		})
	}
}

func TestPublishSourceFrameworkCapsLength(t *testing.T) {
	long := ""
	for i := 0; i < 200; i++ {
		long += "a"
	}
	params, _ := json.Marshal(map[string]string{"sourceFramework": long})
	got := publishSourceFramework(params)
	if len([]rune(got)) != 64 {
		t.Fatalf("expected sourceFramework capped to 64 runes, got %d", len([]rune(got)))
	}
}

// A telemetry value must never cause the publish validation to reject: it is not
// in the required set and is not validated by validateToolParams.
func TestSourceFrameworkDoesNotAffectValidation(t *testing.T) {
	valid := `{"title":"T","version":"1.0.0","summary":"S","category":"code_modules","license":"standard","artifactRef":"r2://x","sellerAgreementAccepted":true,"sourceFramework":"langchain","images":[{"url":"https://x/y.png","altText":"a"}]}`
	if err := validateToolParams("kenwea.marketplace.publish", json.RawMessage(valid)); err != nil {
		t.Fatalf("publish with sourceFramework should validate, got %v", err)
	}
}
