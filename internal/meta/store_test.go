package meta

import "testing"

// TestValidatePlacementMode covers the four-way truth table for the
// PlacementMode validator: empty (legacy default), explicit weighted,
// explicit strict, and anything else (rejected with
// ErrInvalidPlacementMode). US-001 effective-placement.
func TestValidatePlacementMode(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want error
	}{
		{"empty-default", "", nil},
		{"weighted", PlacementModeWeighted, nil},
		{"strict", PlacementModeStrict, nil},
		{"uppercase-weighted", "WEIGHTED", ErrInvalidPlacementMode},
		{"uppercase-strict", "STRICT", ErrInvalidPlacementMode},
		{"unknown", "loose", ErrInvalidPlacementMode},
		{"surrounding-space", " strict", ErrInvalidPlacementMode},
		{"trailing-space", "strict ", ErrInvalidPlacementMode},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ValidatePlacementMode(tc.in); got != tc.want {
				t.Errorf("ValidatePlacementMode(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// TestNormalizePlacementMode confirms the empty-string default coerces
// to "weighted" while explicit values pass through verbatim. Downstream
// EffectivePolicy + UI renderers branch on the canonical form.
func TestNormalizePlacementMode(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", PlacementModeWeighted},
		{PlacementModeWeighted, PlacementModeWeighted},
		{PlacementModeStrict, PlacementModeStrict},
		// Unknown strings pass through — the function is not a validator.
		// Callers that need rejection use ValidatePlacementMode.
		{"loose", "loose"},
	}
	for _, tc := range cases {
		if got := NormalizePlacementMode(tc.in); got != tc.want {
			t.Errorf("NormalizePlacementMode(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
