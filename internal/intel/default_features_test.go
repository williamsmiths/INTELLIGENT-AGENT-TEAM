package intel

import "testing"

// REV-008 M-8a: the default depends on the mode and is recognised again on a re-grant.
func TestDefaultFeaturesByMode(t *testing.T) {
	if got := DefaultFeatures(ModeDeep); !IsDefaultFeatures(got, ModeDeep) || len(got) != 5 {
		t.Fatalf("DEEP default %v", got)
	}
	assisted := DefaultFeatures(ModeAssisted)
	for _, f := range assisted {
		if f == FeatGate {
			t.Fatalf("ASSISTED default has the gate: %v", assisted)
		}
	}
	if !IsDefaultFeatures([]string{FeatLesson, FeatQuestions, FeatConsult, FeatConvergence}, ModeDirect) {
		t.Fatal("unsorted default not recognised")
	}
	if IsDefaultFeatures(assisted, ModeDeep) || IsDefaultFeatures([]string{FeatGate, FeatQuestions}, ModeAssisted) {
		t.Fatal("a non-default list was taken for the default")
	}
}
