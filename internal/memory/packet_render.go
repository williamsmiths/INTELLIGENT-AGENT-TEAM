package memory

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

// renderer turns blocks into text. AUTHORITATIVE items are printed where they
// occur (once in full); everything else is printed as a [U#] reference and its
// content goes to the unverified block at the end (AC-H-105).
type renderer struct {
	names   map[string]string
	out     []string
	printed map[string]bool
	numbers map[string]int // unverified entry → its U number
	unv     []string
	blocks  []string
	refs    []PacketRef
}

func (r *renderer) author(id string) string {
	if name := oneLine(r.names[id], maxAuthorName); name != "" {
		return name
	}
	return "id:" + id[:min(8, len(id))]
}

func (r *renderer) number(id string, body func(n int) []string) int {
	if n, ok := r.numbers[id]; ok {
		return n
	}
	n := len(r.numbers) + 1
	r.numbers[id] = n
	r.unv = append(r.unv, body(n)...)
	return n
}

func (r *renderer) line(blockID string, l line) {
	switch {
	case l.post != nil:
		p := l.post
		n := r.number("b:"+strconv.FormatInt(p.ID, 10), func(n int) []string {
			out := []string{fmt.Sprintf("[U%d] blackboard/%s · trust: chưa kiểm chứng · tác giả: %s", n, p.Kind, r.author(p.Author))}
			out = append(out, quote("nội dung: ", p.Body, maxUnverifiedBody)...)
			if len(p.Refs) > 0 {
				out = append(out, quote("refs: ", strings.Join(p.Refs, ", "), maxUnverifiedRefs)...)
			}
			return out
		})
		r.out = append(r.out, "- "+fmt.Sprintf(lblSeeBlock, n, "blackboard/"+p.Kind))
		r.refs = append(r.refs, PacketRef{Block: blockID, BlackboardID: p.ID, Unverified: true})
		return
	case l.item == nil:
		r.out = append(r.out, "- "+l.text)
		return
	}
	it := l.item
	ref := PacketRef{Block: blockID, KnowledgeID: it.ID, Key: it.Key, Version: it.Version, Level: it.Level, Trust: it.Trust}
	switch {
	case it.Trust != TrustAuthoritative:
		label := it.Trust
		if it.Basis != "" {
			label += ", basis: " + it.Basis
		}
		n := r.number("k:"+it.ID, func(n int) []string {
			out := []string{fmt.Sprintf("[U%d] trust: %s · tác giả: %s · %s · v%d · %s · %s",
				n, label, r.author(it.CreatedBy), it.Level, it.Version, it.Kind, it.Aspect)}
			out = append(out, quote("key: ", it.Key, maxUnverifiedKey)...)
			out = append(out, quote("tiêu đề: ", it.Title, maxUnverifiedTitle)...)
			if strings.TrimSpace(it.Body) != "" {
				out = append(out, quote("nội dung: ", it.Body, maxUnverifiedBody)...)
			}
			return out
		})
		r.out = append(r.out, "- "+l.text+fmt.Sprintf(lblSeeBlock, n, label))
		ref.Unverified = true
	case r.printed[it.ID]:
		r.out = append(r.out, "- "+l.text+fmt.Sprintf(lblSeeAbove, it.Key, it.Version))
	default:
		r.printed[it.ID] = true
		tag := "[AUTHORITATIVE]"
		if it.Level == LevelGlobal {
			tag = "[GLOBAL · AUTHORITATIVE]"
		}
		r.out = append(r.out, fmt.Sprintf("- %s%s %s v%d (%s, %s): %s", l.text, tag, it.Key, it.Version, it.Kind, it.Aspect, it.Title))
		for _, extra := range []string{it.Body, prefixed(lblAppliesWhen, it.AppliesWhen), prefixed(lblLimits, it.Limits)} {
			if strings.TrimSpace(extra) != "" {
				r.out = append(r.out, "  "+strings.ReplaceAll(strings.TrimSpace(extra), "\n", "\n  "))
			}
		}
	}
	r.refs = append(r.refs, ref)
}

func prefixed(label, value string) string {
	if strings.TrimSpace(value) == "" {
		return ""
	}
	return label + strings.TrimSpace(value)
}

func render(blocks []block, names map[string]string, skip map[string]bool) *renderer {
	r := &renderer{names: names, printed: map[string]bool{}, numbers: map[string]int{}}
	for _, b := range blocks {
		if skip[dropSemantic] {
			b = b.structured()
		}
		if skip[b.id] || !b.present() {
			continue
		}
		r.blocks = append(r.blocks, b.id)
		r.out = append(r.out, b.head...)
		for _, l := range b.lines {
			r.line(b.id, l)
		}
		r.out = append(r.out, "")
	}
	if len(r.unv) > 0 {
		r.out = append(r.out, UnverifiedBegin, lblUnverified)
		r.out = append(append(r.out, r.unv...), UnverifiedEnd, "")
	}
	return r
}

// finishPacket renders within the character budget, dropping the lowest-priority
// blocks first and never the task block or the pinned requirements, then hashes.
func finishPacket(in PacketInput, keys []string, blocks []block, names map[string]string) Packet {
	budget := in.MaxChars
	if budget <= 0 {
		budget = DefaultPacketChars
	}
	p := Packet{ProjectID: in.ProjectID, TaskID: in.TaskID, Revision: in.Revision}
	skip := map[string]bool{}
	for {
		r := render(blocks, names, skip)
		// Dropped semantic lines are not announced: the text is then exactly the packet without them.
		if noted := slices.DeleteFunc(slices.Clone(p.Dropped), func(d string) bool { return d == dropSemantic }); len(noted) > 0 {
			r.out = append(r.out, "(Đã lược bớt để vừa giới hạn ký tự: "+strings.Join(noted, ", ")+")", "")
		}
		body := strings.Join(r.out, "\n")
		h := sha256.New()
		fmt.Fprintf(h, "kch-context-v1\nproject=%s\ntask=%s\nrevision=%d\n", in.ProjectID, in.TaskID, in.Revision)
		for _, k := range keys {
			fmt.Fprintf(h, "dep=%d:%s@%d\n", len(k), k, in.DependencyVersions[k])
		}
		h.Write([]byte(body))
		p.Hash = hex.EncodeToString(h.Sum(nil))
		p.Text = body + lblHash + p.Hash + "\n"
		p.Chars, p.Blocks, p.Refs = utf8.RuneCountInString(p.Text), r.blocks, r.refs
		if p.Chars <= budget {
			return p
		}
		// Lines added by the semantic stage go before any structured block (spec S2 B.1).
		if !skip[dropSemantic] && slices.ContainsFunc(blocks, block.hasSemantic) {
			skip[dropSemantic] = true
			p.Dropped = append(p.Dropped, dropSemantic)
			continue
		}
		at := slices.IndexFunc(dropOrder, func(id string) bool { return slices.Contains(r.blocks, id) })
		if at < 0 {
			p.OverBudget = true
			return p
		}
		skip[dropOrder[at]] = true
		p.Dropped = append(p.Dropped, dropOrder[at])
	}
}
