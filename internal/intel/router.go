// Package intel holds the Intelligence Layer of spec S2 part C as runtime
// mechanisms: the router (C.1), structural validation of an understanding
// (C.2), questions, consults (C.3), attempts and convergence (C.4), lessons
// (C.6) and the cost report (C.7). The harness never calls a model: this
// package decides when something is required, allowed or stopped, and
// measures. Pure rules live in files without SQL; the *_sql.go files read and
// write the tables of migration 00005 in the caller's transaction.
//
// intel does not import internal/work (work imports intel for the router);
// task and lease state changes stay in work and are orchestrated by internal/api.
package intel

import (
	"slices"

	"github.com/williamsmiths/kongcowork-harness/internal/kerr"
)

// Modes of the router (C.1).
const (
	ModeDirect   = "DIRECT"
	ModeAssisted = "ASSISTED"
	ModeDeep     = "DEEP"
)

// Mechanisms a lease may switch off for a measurement (C.1 intel_features).
const (
	FeatGate        = "gate"
	FeatQuestions   = "questions"
	FeatConsult     = "consult"
	FeatConvergence = "convergence"
	FeatLesson      = "lesson"
)

// AllFeatures lists every mechanism. The default of a lease is all of them in DEEP and all but the
// gate in ASSISTED and DIRECT (spec S2 C.1, Task 2 ablation).
var AllFeatures = []string{FeatGate, FeatQuestions, FeatConsult, FeatConvergence, FeatLesson}

// DefaultFeatures is the feature list a lease of this mode gets when the Owner sets none, sorted.
func DefaultFeatures(mode string) []string {
	out := slices.Clone(AllFeatures)
	if mode != ModeDeep {
		out = slices.DeleteFunc(out, func(f string) bool { return f == FeatGate })
	}
	slices.Sort(out)
	return out
}

// IsDefaultFeatures reports whether a lease's features are the default of its mode.
func IsDefaultFeatures(features []string, mode string) bool {
	got := slices.Clone(features)
	slices.Sort(got)
	return slices.Equal(got, DefaultFeatures(mode))
}

// Cost phases of a ledger row (C.7).
const (
	PhaseDiscovery = "DISCOVERY"
	PhaseBuild     = "BUILD"
	PhaseRework    = "REWORK"
	PhaseCollab    = "COLLAB"
)

// Router thresholds (C.1): score = F + 2U + B. They are the proposed defaults,
// to be calibrated against measurements.
const (
	UncertainWeight = 2 // weight of U
	BriefOnlyBonus  = 2 // B when the task pins no requirement
	DirectMaxScore  = 1 // DIRECT <= 1
	DeepMinScore    = 6 // DEEP >= 6; ASSISTED in between
)

// Signals are what the router saw for one lease (lease.intel_signals).
type Signals struct {
	F        int    `json:"F"`
	U        int    `json:"U"`
	B        int    `json:"B"`
	Score    int    `json:"score"`
	Computed string `json:"computed"`         // the mode the score gives
	Forced   string `json:"forced,omitempty"` // the owner's choice at assign, when set
}

// Score is F + 2U + B (C.1).
func Score(f, u, b int) int { return f + UncertainWeight*u + b }

// ModeFor maps a score to a mode (C.1).
func ModeFor(score int) string {
	switch {
	case score <= DirectMaxScore:
		return ModeDirect
	case score >= DeepMinScore:
		return ModeDeep
	}
	return ModeAssisted
}

// Mode is the effective mode: the forced one when the owner set it.
func (s Signals) Mode() string {
	if s.Forced != "" {
		return s.Forced
	}
	return s.Computed
}

// ConsultAllowance is the number of consults a mode allows for a task (C.1 table).
func ConsultAllowance(mode string) int {
	switch mode {
	case ModeAssisted:
		return 1
	case ModeDeep:
		return 3
	}
	return 0
}

// PacketChars is the character budget of the packet of a FULL lease (C.1 table).
func PacketChars(mode string) int {
	switch mode {
	case ModeAssisted:
		return 6000
	case ModeDeep:
		return 9000
	}
	return 3000
}

// NeedsUnderstanding reports whether a mode requires an understanding before the first write (C.1 table).
func NeedsUnderstanding(mode string) bool { return mode == ModeAssisted || mode == ModeDeep }

// ValidMode accepts "" (the router decides) or one of the three modes.
func ValidMode(mode string) bool {
	return mode == "" || mode == ModeDirect || mode == ModeAssisted || mode == ModeDeep
}

// NormalizeFeatures checks a feature list against C.1 and returns it sorted
// without duplicates; nil means every feature.
func NormalizeFeatures(in []string) ([]string, error) {
	if in == nil {
		return slices.Clone(AllFeatures), nil
	}
	out := []string{}
	for _, f := range in {
		if !slices.Contains(AllFeatures, f) {
			return nil, kerr.New(kerr.Invalid, "intel_features must be a subset of gate, questions, consult, convergence, lesson")
		}
		if !slices.Contains(out, f) {
			out = append(out, f)
		}
	}
	slices.Sort(out)
	return out, nil
}

// Has reports whether a feature is on.
func Has(features []string, f string) bool { return slices.Contains(features, f) }

// InitialPhase is the cost phase a lease starts in (C.7): DISCOVERY while an
// understanding is required and missing, otherwise the task's work phase.
func InitialPhase(gatePending bool, workPhase string) string {
	if gatePending {
		return PhaseDiscovery
	}
	return workPhase
}
