package memory_test

import (
	"net/http"
	"testing"

	"github.com/williamsmiths/kongcowork-harness/internal/embed"
	"github.com/williamsmiths/kongcowork-harness/internal/memory"
)

// TestSemanticRealSidecarPacket shows what the semantic fill does to a context
// packet with the real model (evals/retrieval did not measure packets). For
// each fixture project: a task that pins REQ-1 and is described by the first
// paraphrase query of that project. It asserts isolation and that the
// structured selection stays in place; the sizes are logged for the owner.
func TestSemanticRealSidecarPacket(t *testing.T) {
	r := newRealEnv(t)
	e := r.env
	client := &embed.Client{BaseURL: realSidecar, Model: embed.DefaultModel, Dim: embed.DefaultDim, HTTP: http.DefaultClient}
	worker := memory.EnableSemantic(e.pool, client, embed.DefaultMinSim)
	t.Cleanup(func() { memory.UseSemantic(nil) })
	e.drain(worker)

	for name, pr := range r.projects {
		title := ""
		for _, q := range r.fx.Queries {
			if q.Project == name && q.Group == "paraphrase_vi" {
				title = q.Q
				break
			}
		}
		in := memory.PacketInput{ProjectID: pr.id, TaskID: e.task(pr.id, "T-REAL", "RUNNING", "REQ-1"), TaskKey: "T-REAL", TaskTitle: title,
			DesiredOutcome: title, Revision: 1, DependencyVersions: map[string]int{"REQ-1": 1}}
		withSem := e.packet(pr.member, in)
		memory.UseSemantic(nil)
		lexical := e.packet(pr.member, in)
		memory.EnableSemantic(e.pool, client, embed.DefaultMinSim)

		count := func(p memory.Packet, block string) (n int) {
			for _, ref := range p.Refs {
				if ref.Block == block {
					n++
				}
				if from := r.owner[ref.KnowledgeID]; ref.KnowledgeID != "" && from != name && from != "global" {
					t.Errorf("AC-S2-022: packet of %s contains %s of %q", name, ref.Key, from)
				}
			}
			return n
		}
		for i, ref := range lexical.Refs { // nothing structured moved or disappeared
			if ref.Block == memory.BlockPinned && (i >= len(withSem.Refs) || withSem.Refs[i] != ref) {
				t.Errorf("%s: pinned entry %s changed with semantic on", name, ref.Key)
			}
		}
		t.Logf("%s task %q: related %d → %d, global %d → %d, packet %d → %d chars (limit %d), dropped %v",
			name, title, count(lexical, memory.BlockRelated), count(withSem, memory.BlockRelated),
			count(lexical, memory.BlockGlobal), count(withSem, memory.BlockGlobal), lexical.Chars, withSem.Chars, memory.DefaultPacketChars, withSem.Dropped)
	}
}
