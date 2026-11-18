package intel

import (
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/williamsmiths/kongcowork-harness/internal/kerr"
)

// Understanding is the body of `kch understand` (C.2), or a delta after a
// feedback or a requirement change (C.5) when Delta is set.
type Understanding struct {
	Goal          string          `json:"goal"`
	Preserve      []string        `json:"preserve"`
	Affected      []string        `json:"affected"`
	NotApplicable []NotApplicable `json:"not_applicable"`
	Assumptions   []Assumption    `json:"assumptions"`
	Questions     []Question      `json:"questions"`
	Delta         *Delta          `json:"delta,omitempty"`
}

// NotApplicable declares that an item does not concern the task, with a reason.
type NotApplicable struct {
	Key    string `json:"key"`
	Reason string `json:"reason"`
}

// Assumption is a choice the agent makes without the owner (stored as CANDIDATE knowledge).
type Assumption struct {
	Text     string `json:"text"`
	Basis    string `json:"basis"` // proposed, inferred or unknown
	Evidence string `json:"evidence"`
}

// Question is a question to the owner with options and the default the agent goes on with.
type Question struct {
	Text     string   `json:"text"`
	Options  []string `json:"options"`
	Default  string   `json:"default"`
	Impact   string   `json:"impact"` // low, medium, high
	Blocking bool     `json:"blocking"`
}

// Delta is what changed and what stays after a feedback (C.5).
type Delta struct {
	Changed   []string `json:"changed"`   // keys or paths
	Preserved []string `json:"preserved"` // keys
}

// Missing is one item an understanding did not address, with the rule it breaks.
type Missing struct {
	Key   string `json:"key"`
	Title string `json:"title"`
	Rule  string `json:"rule"`
}

// Limits of C.2 and of the stored rows.
const (
	MinReasonChars = 15 // U5: a not_applicable reason
	MaxHighImpact  = 3  // U4
	maxAssumptions = 20
	maxQuestions   = 10
	maxTextChars   = 2000
)

var bases = []string{"proposed", "inferred", "unknown"}
var impacts = []string{"low", "medium", "high"}

// CheckShape rejects a body that cannot be stored at all (422, not a counted
// rejection): unknown basis or impact, empty texts, oversized lists.
func (u Understanding) CheckShape() error {
	if len(u.Assumptions) > maxAssumptions || len(u.Questions) > maxQuestions {
		return kerr.New(kerr.Invalid, "tối đa %d giả định và %d câu hỏi", maxAssumptions, maxQuestions)
	}
	for i, a := range u.Assumptions {
		if strings.TrimSpace(a.Text) == "" || !slices.Contains(bases, a.Basis) || utf8.RuneCountInString(a.Text+a.Evidence) > maxTextChars {
			return kerr.New(kerr.Invalid, "giả định %d cần text và basis là proposed, inferred hoặc unknown", i+1)
		}
	}
	for i, q := range u.Questions {
		if err := q.CheckShape(); err != nil {
			return kerr.New(kerr.Invalid, "câu hỏi %d: %s", i+1, err.Error())
		}
	}
	return nil
}

// CheckShape rejects a question that cannot be stored.
func (q Question) CheckShape() error {
	if strings.TrimSpace(q.Text) == "" || utf8.RuneCountInString(q.Text) > maxTextChars {
		return kerr.New(kerr.Invalid, "cần text (tối đa %d ký tự)", maxTextChars)
	}
	if !slices.Contains(impacts, q.Impact) {
		return kerr.New(kerr.Invalid, "impact phải là low, medium hoặc high")
	}
	return nil
}

// HasOptions is U4 for one question: at least two options and a default among them.
func (q Question) HasOptions() bool {
	return len(q.Options) >= 2 && strings.TrimSpace(q.Default) != "" && slices.Contains(q.Options, q.Default)
}

// Validate applies U1–U5 to a full understanding (C.2). It checks structure
// only: every listed item was put in front of the agent and its choice is
// recorded; it cannot judge whether the understanding is right.
func Validate(u Understanding, s Sets) []Missing {
	var out []Missing
	addressed := map[string]bool{}
	for _, k := range append(slices.Clone(u.Preserve), u.Affected...) {
		addressed[k] = true
	}
	na := map[string]bool{}
	for _, n := range u.NotApplicable {
		short := utf8.RuneCountInString(strings.TrimSpace(n.Reason)) < MinReasonChars
		switch {
		case containsKey(s.Pinned, n.Key):
			out = append(out, Missing{Key: n.Key, Title: titleOf(s, n.Key), Rule: "U5-pinned"})
		case short:
			out = append(out, Missing{Key: n.Key, Title: titleOf(s, n.Key), Rule: "U5-reason"})
		default:
			na[n.Key] = true
		}
	}
	for _, r := range s.Pinned { // U1
		if !addressed[r.Key] {
			out = append(out, Missing{Key: r.Key, Title: r.Title, Rule: "U1"})
		}
	}
	for _, r := range s.Related { // U2
		if !addressed[r.Key] && !na[r.Key] && !containsMissing(out, r.Key) {
			out = append(out, Missing{Key: r.Key, Title: r.Title, Rule: "U2"})
		}
	}
	mentions := u.mentionText()
	for _, r := range s.Uncertain { // U3
		if !na[r.Key] && !strings.Contains(mentions, r.Key) && !containsMissing(out, r.Key) {
			out = append(out, Missing{Key: r.Key, Title: r.Title, Rule: "U3"})
		}
	}
	high := 0
	for i, q := range u.Questions { // U4
		if q.Impact == "high" {
			high++
		}
		if !q.HasOptions() {
			out = append(out, Missing{Key: fmt.Sprintf("question-%d", i+1), Title: clip(q.Text, 80), Rule: "U4"})
		}
	}
	if high > MaxHighImpact {
		out = append(out, Missing{Key: "questions", Title: fmt.Sprintf("%d câu impact high", high), Rule: "U4-high"})
	}
	if s.BriefOnly() && len(u.Assumptions) == 0 && len(u.Questions) == 0 { // U5
		out = append(out, Missing{Key: "assumptions", Title: "task chỉ có brief", Rule: "U5-brief"})
	}
	return out
}

// ValidateDelta applies C.5: every pinned key is in changed or preserved.
func ValidateDelta(d Delta, s Sets) []Missing {
	var out []Missing
	for _, r := range s.Pinned {
		if !slices.Contains(d.Changed, r.Key) && !slices.Contains(d.Preserved, r.Key) {
			out = append(out, Missing{Key: r.Key, Title: r.Title, Rule: "DELTA"})
		}
	}
	return out
}

// mentionText is where an uncertain key counts as mentioned (U3).
func (u Understanding) mentionText() string {
	var b strings.Builder
	for _, a := range u.Assumptions {
		b.WriteString(a.Text + "\n" + a.Evidence + "\n")
	}
	for _, q := range u.Questions {
		b.WriteString(q.Text + "\n" + strings.Join(q.Options, "\n") + "\n")
	}
	return b.String()
}

func titleOf(s Sets, key string) string {
	for _, list := range [][]Ref{s.Pinned, s.Related, s.Uncertain} {
		for _, r := range list {
			if r.Key == key {
				return r.Title
			}
		}
	}
	return ""
}

func containsMissing(m []Missing, key string) bool {
	return slices.ContainsFunc(m, func(x Missing) bool { return x.Key == key })
}

// clip collapses whitespace and cuts s to n characters.
func clip(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}
