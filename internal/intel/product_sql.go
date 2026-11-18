package intel

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// Ref is one knowledge key with its title, as shown to an agent.
type Ref struct {
	Key   string `json:"key"`
	Title string `json:"title"`
}

// Sets are the parts of the product model one task is measured against
// (C.1 signals, C.2 rules U1–U3).
type Sets struct {
	Pinned    []Ref // keys the task pins (dependency_versions)
	Related   []Ref // set F: one live relation hop from a pinned key, or AUTHORITATIVE in the task scope and not pinned
	Uncertain []Ref // set U: CANDIDATE in the task scope, without assumptions of ended tasks nor the task's own
}

// BriefOnly reports a task that pins no requirement (C.1 B, C.2 U5).
func (s Sets) BriefOnly() bool { return len(s.Pinned) == 0 }

// ProductInput names the task the sets are read for.
type ProductInput struct {
	ProjectID string
	Scope     string
	Pinned    []string
	TaskKey   string // the task's own assumptions ASM-<TaskKey>-n are not in U (REV-005 #12); "" = none excluded
}

// maxSet bounds each set: a busy scope cannot make a gate message or a router read unbounded.
const maxSet = 60

// LoadSets reads the three sets of a task. Only PROJECT-level current rows of
// the task's own project are read, so the result is the same under the
// runtime, the owner or a member's scope.
func LoadSets(ctx context.Context, tx pgx.Tx, in ProductInput) (Sets, error) {
	pinned := in.Pinned
	if pinned == nil {
		pinned = []string{}
	}
	var s Sets
	var err error
	if s.Pinned, err = refs(ctx, tx, `SELECT k.key, k.title FROM knowledge k
		WHERE k.project_id = $1 AND k.level = 'PROJECT' AND k.is_current AND k.key = ANY($2::text[]) ORDER BY k.key`,
		in.ProjectID, pinned); err != nil {
		return Sets{}, err
	}
	for _, k := range pinned { // a pinned key without a current row still has to be addressed
		if !containsKey(s.Pinned, k) {
			s.Pinned = append(s.Pinned, Ref{Key: k})
		}
	}
	if s.Related, err = refs(ctx, tx, `SELECT key, title FROM (
			SELECT k.key, k.title FROM relation r
			JOIN knowledge k ON k.project_id = r.project_id AND k.level = 'PROJECT' AND k.is_current
			 AND k.key = CASE WHEN r.from_key = ANY($2::text[]) THEN r.to_key ELSE r.from_key END
			WHERE r.project_id = $1 AND r.valid_to IS NULL AND (r.from_key = ANY($2::text[]) OR r.to_key = ANY($2::text[]))
			UNION
			SELECT k.key, k.title FROM knowledge k
			WHERE k.project_id = $1 AND k.level = 'PROJECT' AND k.is_current AND k.trust = 'AUTHORITATIVE'
			  AND $3 <> '' AND k.scope = $3) f
		WHERE NOT (key = ANY($2::text[])) ORDER BY key LIMIT $4`, in.ProjectID, pinned, in.Scope, maxSet); err != nil {
		return Sets{}, err
	}
	// An assumption ASM-<task key>-<n> of a SUPERSEDED or VERIFIED task no
	// longer counts (C.1), nor one the task itself made: its own declarations
	// would raise its own mode at the next lease (REV-005 #12).
	s.Uncertain, err = refs(ctx, tx, `SELECT k.key, k.title FROM knowledge k
		WHERE k.project_id = $1 AND k.level = 'PROJECT' AND k.is_current AND k.trust = 'CANDIDATE'
		  AND $2 <> '' AND k.scope = $2
		  AND NOT EXISTS (SELECT 1 FROM task t WHERE t.project_id = k.project_id
		        AND (t.state IN ('SUPERSEDED','VERIFIED') OR t.key = $4)
		        AND left(k.key, char_length(t.key) + 5) = 'ASM-' || t.key || '-'
		        AND substr(k.key, char_length(t.key) + 6) ~ '^[0-9]+$')
		ORDER BY k.key LIMIT $3`, in.ProjectID, in.Scope, maxSet, in.TaskKey)
	return s, err
}

// Route computes the signals of C.1 for a task.
func Route(ctx context.Context, tx pgx.Tx, in ProductInput) (Signals, error) {
	s, err := LoadSets(ctx, tx, in)
	if err != nil {
		return Signals{}, err
	}
	sig := Signals{F: len(s.Related), U: len(s.Uncertain)}
	if s.BriefOnly() {
		sig.B = BriefOnlyBonus
	}
	sig.Score = Score(sig.F, sig.U, sig.B)
	sig.Computed = ModeFor(sig.Score)
	return sig, nil
}

func refs(ctx context.Context, tx pgx.Tx, sql string, args ...any) ([]Ref, error) {
	rows, err := tx.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[Ref])
}

func containsKey(refs []Ref, key string) bool {
	for _, r := range refs {
		if r.Key == key {
			return true
		}
	}
	return false
}
