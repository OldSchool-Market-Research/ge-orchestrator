package store

import "testing"

func TestRealizedEstimate(t *testing.T) {
	med := 1500.0
	equity := int64(42)
	if est, sim := realizedEstimate(&med, &equity, 10); !sim || *est != 42 {
		t.Errorf("a sim-scored close reports its frozen equity, got %v sim=%v", est, sim)
	}
	if est, sim := realizedEstimate(&med, nil, 2.5); sim || *est != 3750 {
		t.Errorf("median pace x hours, got %v sim=%v", est, sim)
	}
	if est, _ := realizedEstimate(nil, nil, 10); est != nil {
		t.Errorf("no priced evaluation means no estimate, never zero, got %v", *est)
	}
}
