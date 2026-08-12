package store

import "testing"

func f(v float64) *float64 { return &v }

func TestCalibrationFactor(t *testing.T) {
	cases := []struct {
		name           string
		pSurvive, pace *float64
		want           float64
	}{
		// The 0.15 floor (raised from 0.05, 2026-08-11) also lifts the
		// cold-start default product 0.125 to the floor: cold starts and the
		// clamp agree on one number.
		{"cold start clamps defaults to the floor", nil, nil, 0.15},
		{"survival gated, pace known", nil, f(0.8), 0.2},
		{"survival known, pace gated", f(0.4), nil, 0.2},
		{"both known", f(0.5), f(0.6), 0.3},
		{"floor clamp keeps a bad fortnight from vetoing everything", f(0.02), f(0.1), 0.15},
		{"negative survivor pace clamps to floor, not below", f(0.5), f(-0.4), 0.15},
		{"ceiling clamp keeps a lucky sample from inflating claims", f(1.0), f(1.5), 1.2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := calibrationFactor(tc.pSurvive, tc.pace); got != tc.want {
				t.Fatalf("calibrationFactor(%v, %v) = %v, want %v", tc.pSurvive, tc.pace, got, tc.want)
			}
		})
	}
}
