package memory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/williamsmiths/kongcowork-harness/internal/journal"
	"github.com/williamsmiths/kongcowork-harness/internal/kerr"
)

// Item is one version of a knowledge item (spec §2.3).
type Item struct {
	ID          string
	Level       string
	ProjectID   string // empty for GLOBAL
	Key         string
	NS          string // key namespace: the author for AGENT and unconfirmed GLOBAL items
	Version     int
	Kind        string
	Aspect      string
	Trust       string
	Basis       string
	Scope       string
	Title       string
	Body        string
	AppliesWhen string
	Limits      string
	SourceIDs   []string
	IsCurrent   bool
	ContentHash string
	EndedReason string // "", superseded, invalidated, merged
	ReplacedBy  string
	CreatedBy   string
	CreatedAt   time.Time
}

// Content is the part of an item that a new version may change.
type Content struct {
	Aspect      string
	Trust       string
	Basis       string
	Scope       string
	Title       string
	Body        string
	AppliesWhen string
	Limits      string
	SourceIDs   []string
}

// Repin tells the caller which tasks pinned the changed key and what they must
// pin now (T-H-32, T-H-34). The caller replaces their leases (T-H-22, T-H-04).
type Repin struct {
	TaskIDs []string
	Key     string
	Version int
}

// WriteResult is the outcome of a knowledge write.
type WriteResult struct {
	Item    Item
	Created bool   // false: selective remembering returned the existing row (AC-H-101)
	Repin   *Repin // set when open tasks of the project pinned the key
	// ClosedRelations is the number of relations closed by an invalidation.
	ClosedRelations int
	// Deferred holds journal events this transaction's scope may not write
	// (project-less events under an agent scope). The caller appends them
	// under store.Runtime.
	Deferred []journal.Event
}

const itemCols = `k.id::text, k.level, coalesce(k.project_id::text,''), k.key, coalesce(k.ns::text,''), k.version,
	k.kind, k.aspect, k.trust, coalesce(k.basis,''), k.scope, k.title, k.body, k.applies_when, k.limits,
	k.source_ids::text[], k.is_current, k.content_hash, k.ended_reason, coalesce(k.replaced_by::text,''),
	k.created_by::text, k.created_at`

func itemDest(it *Item) []any {
	return []any{&it.ID, &it.Level, &it.ProjectID, &it.Key, &it.NS, &it.Version,
		&it.Kind, &it.Aspect, &it.Trust, &it.Basis, &it.Scope, &it.Title, &it.Body, &it.AppliesWhen, &it.Limits,
		&it.SourceIDs, &it.IsCurrent, &it.ContentHash, &it.EndedReason, &it.ReplacedBy,
		&it.CreatedBy, &it.CreatedAt}
}

func scanItem(row pgx.Row) (Item, error) {
	var it Item
	err := row.Scan(itemDest(&it)...)
	return it, err
}

// getItem loads one version by id. Rows hidden by row-level security do not exist.
func getItem(ctx context.Context, tx pgx.Tx, id string) (Item, error) {
	if !isUUID(id) {
		return Item{}, kerr.New(kerr.NotFound, "knowledge not found")
	}
	it, err := scanItem(tx.QueryRow(ctx, `SELECT `+itemCols+` FROM knowledge k WHERE k.id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Item{}, kerr.New(kerr.NotFound, "knowledge not found")
	}
	return it, wrap("load knowledge", err)
}

// currentInNamespace loads the current version of a key in one key namespace.
func currentInNamespace(ctx context.Context, tx pgx.Tx, projectID, level, ns, key string) (Item, bool, error) {
	it, err := scanItem(tx.QueryRow(ctx, `SELECT `+itemCols+` FROM knowledge k
		WHERE k.project_id IS NOT DISTINCT FROM nullif($1,'')::uuid AND k.level = $2
		  AND k.ns IS NOT DISTINCT FROM nullif($3,'')::uuid AND k.key = $4 AND k.is_current`,
		projectID, level, ns, key))
	if errors.Is(err, pgx.ErrNoRows) {
		return Item{}, false, nil
	}
	return it, err == nil, wrap("load current knowledge", err)
}

// nsFor is the key namespace rule (spec §0, AC-H-107): the author for AGENT and
// for unconfirmed GLOBAL items, none otherwise.
func nsFor(level, trust, author string) string {
	if level == LevelAgent || (level == LevelGlobal && trust != TrustAuthoritative) {
		return author
	}
	return ""
}

// normalizeText collapses whitespace so that re-typed content hashes the same.
func normalizeText(s string) string { return strings.Join(strings.Fields(s), " ") }

// contentHash identifies "the same knowledge" for selective remembering (AC-H-101):
// level, project, author, kind, aspect, scope and the normalized title + body.
func contentHash(level, projectID, author, kind string, c Content) string {
	h := sha256.New()
	for _, part := range []string{level, projectID, author, kind, c.Aspect, strings.TrimSpace(c.Scope),
		normalizeText(c.Title), normalizeText(c.Body)} {
		fmt.Fprintf(h, "%d:%s\n", len(part), part)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// insertVersion writes one new current version. It returns ok=false when the same
// author already stores the same content in a current row (knowledge_hash_uq);
// any other unique conflict is a 409.
func insertVersion(ctx context.Context, tx pgx.Tx, it Item) (Item, bool, error) {
	if it.SourceIDs == nil {
		it.SourceIDs = []string{}
	}
	out, err := scanItem(tx.QueryRow(ctx, `INSERT INTO knowledge AS k
		(level, project_id, key, ns, version, kind, aspect, trust, basis, scope, title, body,
		 applies_when, limits, source_ids, content_hash, created_by)
		VALUES ($1, nullif($2,'')::uuid, $3, nullif($4,'')::uuid, $5, $6, $7, $8, nullif($9,''), $10, $11, $12,
		        $13, $14, $15::uuid[], $16, $17)
		ON CONFLICT (project_id, level, created_by, content_hash) WHERE is_current DO NOTHING
		RETURNING `+itemCols,
		it.Level, it.ProjectID, it.Key, it.NS, it.Version, it.Kind, it.Aspect, it.Trust, it.Basis, it.Scope,
		it.Title, it.Body, it.AppliesWhen, it.Limits, it.SourceIDs, it.ContentHash, it.CreatedBy))
	if errors.Is(err, pgx.ErrNoRows) {
		return Item{}, false, nil
	}
	if c := uniqueViolation(err); c != "" {
		return Item{}, false, kerr.New(kerr.Conflict, "key %q already has this version or a current version (%s)", it.Key, c)
	}
	if err != nil {
		return Item{}, false, wrap("insert knowledge", err)
	}
	return out, true, nil
}

// eventPayload builds a journal payload with ids and codes only. The key is left
// out for private namespaces so a project journal never reveals another agent's
// key (AC-H-004, AC-H-107).
func eventPayload(it Item, extra map[string]any) map[string]any {
	p := map[string]any{"knowledge_id": it.ID, "level": it.Level, "version": it.Version, "trust": it.Trust}
	if it.NS == "" {
		p["key"] = it.Key
	}
	for k, v := range extra {
		p[k] = v
	}
	return p
}
