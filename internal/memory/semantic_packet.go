package memory

import (
	"context"
	"strings"

	"github.com/jackc/pgx/v5"
)

// dropSemantic names what a packet over budget gives up first: the lines the
// semantic stage added. They never cost a structured block its place.
const dropSemantic = "semantic"

// packetFill adds semantically similar items to a packet block that the
// structured selection left below its limit (spec S2 B.1: "semantic chỉ dùng
// để chọn mục GLOBAL/PROJECT experience và mục liên quan khi các tầng trước
// trả ít hơn giới hạn"). The query is the task itself, as in the fixed rule of
// the GLOBAL block; the threshold and the availability check are those of
// search. Without a searcher, or when the sidecar fails, blocks stay as they are.
type packetFill struct {
	ctx  context.Context
	tx   pgx.Tx
	text string
	down bool // the sidecar failed once in this packet: do not wait for it again
}

func newPacketFill(ctx context.Context, tx pgx.Tx, in PacketInput) *packetFill {
	return &packetFill{ctx: ctx, tx: tx, text: strings.TrimSpace(in.TaskTitle + " " + in.DesiredOutcome + " " + in.Scope)}
}

// fill appends up to limit - len(lines) items passing f. Items already in the
// block are skipped and nothing already there is moved or removed.
func (p *packetFill) fill(b block, f filter, limit int, label string) block {
	if p.text == "" || p.down {
		return b
	}
	seen := []string{}
	for _, l := range b.lines {
		if l.item != nil {
			seen = append(seen, l.item.ID)
		}
	}
	hits, status := semanticFill(p.ctx, p.tx, f, p.text, seen, limit-len(b.lines))
	p.down = status == SemanticUnavailable
	for i := range hits {
		b.lines = append(b.lines, line{text: label, item: &hits[i].Item, semantic: true})
	}
	return b
}

// related fills the related block with current PROJECT-level items of the same
// project (the population loadRelated draws from), never a pinned key.
func (p *packetFill) related(b block, in PacketInput, pinned []string) block {
	if pinned == nil {
		pinned = []string{}
	}
	f := filter{where: `k.project_id = $1 AND k.level = 'PROJECT' AND k.is_current AND k.key <> ALL($2::text[])`,
		args: []any{in.ProjectID, pinned}}
	return p.fill(b, f, maxRelated, lblRelSemantic)
}

// global fills the GLOBAL block with confirmed GLOBAL experiences (the population of loadGlobal).
func (p *packetFill) global(b block) block {
	f := filter{where: `k.level = 'GLOBAL' AND k.trust = 'AUTHORITATIVE' AND k.is_current AND k.kind = 'experience'`}
	return p.fill(b, f, maxGlobal, "")
}
