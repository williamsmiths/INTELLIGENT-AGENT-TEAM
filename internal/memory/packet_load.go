package memory

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/williamsmiths/kongcowork-harness/internal/authn"
)

// Fixed caps per block, so one busy scope cannot crowd out the rest of a packet.
const (
	maxRelated    = 20
	maxScopeItems = 40
	maxGlobal     = 3 // spec §4: at most 3 GLOBAL experiences
	maxNotes      = 10
	maxBlackboard = 10
)

func queryItems(ctx context.Context, tx pgx.Tx, what, sql string, args ...any) ([]Item, error) {
	rows, err := tx.Query(ctx, sql, args...)
	if err != nil {
		return nil, wrap(what, err)
	}
	items, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Item, error) { return scanItem(row) })
	return items, wrap(what, err)
}

// loadPinned loads exactly the versions the task pinned (dependency_versions).
// Only an AUTHORITATIVE row is shown as a requirement; anything else is a note
// without content, so nothing unconfirmed enters the requirements block.
func loadPinned(ctx context.Context, tx pgx.Tx, in PacketInput, keys []string) (block, error) {
	b := block{id: BlockPinned, head: []string{lblPinned}}
	if len(keys) == 0 {
		b.head = append(b.head, lblNoPinned)
		return b, nil
	}
	versions := make([]int32, len(keys))
	for i, k := range keys {
		versions[i] = int32(in.DependencyVersions[k])
	}
	items, err := queryItems(ctx, tx, "load pinned requirements", `SELECT `+itemCols+` FROM knowledge k
		JOIN unnest($2::text[], $3::int[]) AS pin(key, version) ON pin.key = k.key AND pin.version = k.version
		WHERE k.project_id = $1 AND k.level = 'PROJECT'`, in.ProjectID, keys, versions)
	if err != nil {
		return b, err
	}
	for i, key := range keys {
		at := slices.IndexFunc(items, func(it Item) bool { return it.Key == key })
		switch {
		case at < 0:
			b.lines = append(b.lines, line{text: fmt.Sprintf(lblPinMissing, key, versions[i])})
		case items[at].Trust != TrustAuthoritative:
			b.lines = append(b.lines, line{text: fmt.Sprintf(lblPinStale, key, versions[i], items[at].Trust)})
		default:
			b.lines = append(b.lines, line{item: &items[at]})
		}
	}
	return b, nil
}

// loadRelated follows ONE live relation hop from the pinned keys to current
// PROJECT-level items of the same project (AC-H-103). The item keeps its own trust.
func loadRelated(ctx context.Context, tx pgx.Tx, in PacketInput, keys []string) (block, error) {
	b := block{id: BlockRelated, head: []string{lblRelated}}
	if len(keys) == 0 {
		return b, nil
	}
	rows, err := tx.Query(ctx, `SELECT r.type, r.from_key = ANY($2::text[]) AS outgoing,
			CASE WHEN r.from_key = ANY($2::text[]) THEN r.from_key ELSE r.to_key END AS pinned_key, `+itemCols+`
		FROM relation r
		JOIN knowledge k ON k.project_id = r.project_id AND k.level = 'PROJECT' AND k.is_current
		 AND k.key = CASE WHEN r.from_key = ANY($2::text[]) THEN r.to_key ELSE r.from_key END
		WHERE r.project_id = $1 AND r.valid_to IS NULL AND (r.from_key = ANY($2::text[]) OR r.to_key = ANY($2::text[]))
		ORDER BY pinned_key, r.type, k.key, r.id LIMIT $3`, in.ProjectID, keys, maxRelated+1)
	if err != nil {
		return b, wrap("load related items", err)
	}
	defer rows.Close()
	for rows.Next() {
		var typ, pinned string
		var outgoing bool
		it := new(Item)
		if err := rows.Scan(append([]any{&typ, &outgoing, &pinned}, itemDest(it)...)...); err != nil {
			return b, wrap("load related items", err)
		}
		label := fmt.Sprintf(lblRelIncoming, typ, pinned) // item —type→ pinned
		if outgoing {
			label = fmt.Sprintf(lblRelOutgoing, pinned, typ) // pinned —type→ item
		}
		b.lines = append(b.lines, line{text: label, item: it})
	}
	b.cap(maxRelated)
	return b, wrap("load related items", rows.Err())
}

// loadScope reads the current PROJECT-level items of the task scope once and
// builds two blocks: AS_IS / TO_BE pairs per kind (AC-H-009) and the CANDIDATE
// items to treat as uncertain (AC-H-020).
func loadScope(ctx context.Context, tx pgx.Tx, in PacketInput) (pairs, uncertain block, err error) {
	scope := strings.TrimSpace(in.Scope)
	pairs = block{id: BlockPairs, head: []string{fmt.Sprintf(lblPairs, scope)}}
	uncertain = block{id: BlockUncertain, head: []string{lblUncertain, lblUncertainHow}}
	if scope == "" {
		return pairs, uncertain, nil
	}
	items, err := queryItems(ctx, tx, "load scope items", `SELECT `+itemCols+` FROM knowledge k
		WHERE k.project_id = $1 AND k.level = 'PROJECT' AND k.is_current AND k.scope = $2
		ORDER BY k.kind, k.aspect, k.key, k.id LIMIT $3`, in.ProjectID, scope, maxScopeItems+1)
	if err != nil {
		return pairs, uncertain, err
	}
	capped := len(items) > maxScopeItems
	if capped {
		items = items[:maxScopeItems]
	}
	for i := range items {
		it := &items[i]
		other := AspectToBe
		if it.Aspect == AspectToBe {
			other = AspectAsIs
		}
		paired := slices.ContainsFunc(items, func(o Item) bool { return o.Kind == it.Kind && o.Aspect == other })
		if paired && it.Aspect == AspectAsIs { // ordered by kind, AS_IS before TO_BE
			pairs.lines = append(pairs.lines, line{text: fmt.Sprintf(lblAsIs, it.Kind), item: it})
		} else if paired {
			pairs.lines = append(pairs.lines, line{text: fmt.Sprintf(lblToBe, it.Kind), item: it})
		}
		if it.Trust == TrustCandidate {
			uncertain.lines = append(uncertain.lines, line{item: it})
		}
	}
	if capped {
		pairs.lines = append(pairs.lines, line{text: fmt.Sprintf(lblCapped, maxScopeItems)})
	}
	return pairs, uncertain, nil
}

// loadGlobal picks at most 3 confirmed GLOBAL experiences by a fixed rule (spec
// §4): full-text match of any word of title + desired_outcome + scope, best rank first.
func loadGlobal(ctx context.Context, tx pgx.Tx, in PacketInput) (block, error) {
	b := block{id: BlockGlobal, head: []string{lblGlobal}}
	text := strings.TrimSpace(in.TaskTitle + " " + in.DesiredOutcome + " " + in.Scope)
	if text == "" {
		return b, nil
	}
	items, err := queryItems(ctx, tx, "load global experiences", `WITH q AS (
			SELECT replace(plainto_tsquery('simple', kch_unaccent($1))::text, ' & ', ' | ')::tsquery AS any_word)
		SELECT `+itemCols+` FROM knowledge k, q
		WHERE k.level = 'GLOBAL' AND k.trust = 'AUTHORITATIVE' AND k.is_current AND k.kind = 'experience'
		  AND k.search @@ q.any_word
		ORDER BY ts_rank(k.search, q.any_word) DESC, k.key, k.id LIMIT $2`, text, maxGlobal)
	for i := range items {
		b.lines = append(b.lines, line{item: &items[i]})
	}
	return b, err
}

// loadNotes reads the recipient's own AGENT memory of THIS project only (AC-H-084, PM-H-13).
func loadNotes(ctx context.Context, tx pgx.Tx, actor authn.Principal, in PacketInput) (block, error) {
	b := block{id: BlockNotes, head: []string{lblNotes}}
	items, err := queryItems(ctx, tx, "load agent notes", `SELECT `+itemCols+` FROM knowledge k
		WHERE k.project_id = $1 AND k.level = 'AGENT' AND k.created_by = $2 AND k.is_current
		ORDER BY k.key, k.id LIMIT $3`, in.ProjectID, actor.ID, maxNotes+1)
	for i := range items {
		b.lines = append(b.lines, line{item: &items[i]})
	}
	b.cap(maxNotes)
	return b, err
}

// loadBlackboard reads the most recent deltas of the task, oldest first.
func loadBlackboard(ctx context.Context, tx pgx.Tx, in PacketInput) (block, error) {
	b := block{id: BlockBlackboard, head: []string{lblBlackboard}}
	if in.TaskID == "" {
		return b, nil
	}
	rows, err := tx.Query(ctx, `SELECT `+blackboardCols+` FROM blackboard
		WHERE project_id = $1 AND task_id = $2 ORDER BY id DESC LIMIT $3`, in.ProjectID, in.TaskID, maxBlackboard)
	if err != nil {
		return b, wrap("load blackboard", err)
	}
	posts, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (BlackboardEntry, error) { return scanBlackboard(row) })
	for i := len(posts) - 1; i >= 0; i-- {
		b.lines = append(b.lines, line{post: &posts[i]})
	}
	return b, wrap("load blackboard", err)
}
