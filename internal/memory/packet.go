package memory

import (
	"context"
	"sort"

	"github.com/jackc/pgx/v5"

	"github.com/williamsmiths/kongcowork-harness/internal/authn"
	"github.com/williamsmiths/kongcowork-harness/internal/kerr"
)

// Blocks of a context packet, in the order they appear.
const (
	BlockTask       = "task"       // never dropped
	BlockPinned     = "pinned"     // AUTHORITATIVE requirements pinned by the task; never dropped
	BlockRelated    = "related"    // one live relation hop from the pinned keys (AC-H-103)
	BlockPairs      = "pairs"      // "hiện trạng → mong muốn" of the task scope (AC-H-009)
	BlockGlobal     = "global"     // up to 3 confirmed GLOBAL experiences (AC-H-085)
	BlockNotes      = "notes"      // the recipient's AGENT memory of this project (AC-H-084)
	BlockUncertain  = "uncertain"  // CANDIDATE items of the scope (AC-H-020)
	BlockBlackboard = "blackboard" // recent deltas of the task
)

// dropOrder lists the blocks given up first when the packet is over budget.
var dropOrder = []string{BlockGlobal, BlockNotes, BlockBlackboard, BlockUncertain, BlockPairs, BlockRelated}

// DefaultPacketChars is the character budget used when PacketInput.MaxChars is not set.
const DefaultPacketChars = 12000

// PacketInput is filled by the caller from the task, its lease and its budget.
// This package reads only memory tables; who may see the task (PM-H-12, PM-H-13)
// is the caller's check.
type PacketInput struct {
	ProjectID          string
	TaskID             string
	TaskKey            string
	TaskTitle          string
	DesiredOutcome     string
	Scope              string
	Revision           int
	DependencyVersions map[string]int // requirement key → pinned version
	AllowedPaths       []string
	ProtectedPaths     []string
	RepoPath           string
	BudgetLine         string // preformatted "remaining budget" line
	PendingNotice      string // verbatim feedback or change summary of the lease
	MaxChars           int
	// AuthorNames maps principal id → display name. Agents cannot read other
	// principals' rows, so the caller supplies the names it wants shown; missing
	// ones fall back to a short id.
	AuthorNames map[string]string
	// DelegatedBy is the name of the Lead that wrote the task through
	// `kch delegate` (spec S2 E.1-11, REV-006b N1); "" for an Owner's task.
	DelegatedBy string
}

// PacketRef names one knowledge version or blackboard entry contained in a packet.
type PacketRef struct {
	Block        string
	KnowledgeID  string
	BlackboardID int64
	Key          string
	Version      int
	Level        string
	Trust        string
	Unverified   bool // rendered inside the unverified-data block
}

// Packet is the context of one task for one recipient at one moment.
type Packet struct {
	ProjectID  string
	TaskID     string
	Revision   int
	Hash       string // sha256 over project, task, revision, dependency versions and the text
	Text       string
	Chars      int      // characters of Text
	Blocks     []string // blocks present in Text, in order
	Dropped    []string // blocks left out to respect the budget
	OverBudget bool     // the task block and pinned requirements alone exceed the budget
	Refs       []PacketRef
}

// AssemblePacket builds the context packet of a task for the actor (spec §0
// "Dynamic context assembly"). Everything not AUTHORITATIVE, and every
// blackboard body, is rendered inside one delimited unverified-data block
// (AC-H-105). Only rows of in.ProjectID and confirmed GLOBAL items are read.
func AssemblePacket(ctx context.Context, tx pgx.Tx, actor authn.Principal, in PacketInput) (Packet, error) {
	if _, err := requireMember(ctx, tx, actor, in.ProjectID); err != nil {
		return Packet{}, err
	}
	if in.TaskID != "" && !isUUID(in.TaskID) {
		return Packet{}, kerr.New(kerr.Invalid, "task id is not valid")
	}
	keys := make([]string, 0, len(in.DependencyVersions))
	for k := range in.DependencyVersions {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	blocks := []block{taskBlock(in)}
	fill := newPacketFill(ctx, tx, in) // spec S2 B.1: semantic only fills what the structured rules left empty
	loaders := []func() (block, error){
		func() (block, error) { return loadPinned(ctx, tx, in, keys) },
		func() (block, error) {
			b, err := loadRelated(ctx, tx, in, keys)
			return fill.related(b, in, keys), err
		},
	}
	var uncertain block
	loaders = append(loaders,
		func() (block, error) {
			pairs, unc, err := loadScope(ctx, tx, in)
			uncertain = unc
			return pairs, err
		},
		func() (block, error) {
			b, err := loadGlobal(ctx, tx, in)
			return fill.global(b), err
		},
		func() (block, error) { return loadNotes(ctx, tx, actor, in) },
		func() (block, error) { return uncertain, nil },
		func() (block, error) { return loadBlackboard(ctx, tx, in) },
	)
	for _, load := range loaders {
		b, err := load()
		if err != nil {
			return Packet{}, err
		}
		blocks = append(blocks, b)
	}
	names, err := authorNames(ctx, tx, blocks, in.AuthorNames)
	if err != nil {
		return Packet{}, err
	}
	return finishPacket(in, keys, blocks, names), nil
}
