package hookinstall

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/williamsmiths/kongcowork-harness/internal/journal"
)

// Journal event types written by this package.
const (
	EventTampered   = "HOOK_TAMPERED"
	EventFailOpen   = "HOOK_FAILOPEN"
	EventFailClosed = "HOOK_FAILCLOSED"
)

// Values used in place of a hash when the file cannot be hashed.
const (
	HashMissing    = "missing"
	HashUnreadable = "unreadable"
)

// Config files next to settings.json that change what the provider does and
// that an enrolled workspace must not have (spec S2 A.1, AC-S2-005).
const (
	ExtraSettingsLocal = "settings.local.json" // <workspace>/.claude/settings.local.json
	ExtraMCP           = ".mcp.json"           // <workspace>/.mcp.json
)

var hexHash = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Tamper is one newly detected change of an installed hook file.
type Tamper struct {
	PrincipalID  string
	SettingsPath string
	Reason       string // "changed", "missing" or "extra_config"
	Seq          int64  // journal sequence of the HOOK_TAMPERED event
}

// Register records (or replaces) the hash of the settings file installed for a
// principal and clears the tampered flag: call it after every legitimate
// Install. The transaction must run under store.Runtime.
func Register(ctx context.Context, tx pgx.Tx, principalID, settingsPath, sha256 string) error {
	_, err := tx.Exec(ctx,
		`INSERT INTO hook_install (principal_id, settings_path, sha256, tampered, checked_at)
		 VALUES ($1, $2, $3, false, now())
		 ON CONFLICT (principal_id) DO UPDATE
		   SET settings_path = EXCLUDED.settings_path, sha256 = EXCLUDED.sha256, seen_sha256 = '', tampered = false, checked_at = now()`,
		principalID, settingsPath, sha256)
	if err != nil {
		return fmt.Errorf("hookinstall: register %s: %w", principalID, err)
	}
	return nil
}

// ExtraConfig lists the extra config files that exist in a workspace and can
// carry configuration. Claude Code's sandbox creates EMPTY read-only regular
// files at these paths as bind-mount targets that block writes (observed with
// Claude Code 2.1.323); an empty regular file configures nothing and is not
// reported. Anything else that exists — content, a symlink, a directory, an
// unreadable path — is reported.
func ExtraConfig(workspace string) []string {
	var found []string
	for name, path := range map[string]string{ExtraSettingsLocal: filepath.Join(workspace, ".claude", ExtraSettingsLocal),
		ExtraMCP: filepath.Join(workspace, ExtraMCP)} {
		fi, err := os.Lstat(path)
		switch {
		case errors.Is(err, os.ErrNotExist):
		case err == nil && fi.Mode().IsRegular() && fi.Size() == 0:
		default:
			found = append(found, name)
		}
	}
	sort.Strings(found)
	return found
}

// fingerprint is what one observation is compared by: the hash of the
// settings file plus the extra config files present. Values that are not a
// hash or a known file name are normalised, so nothing an agent can choose
// reaches the journal.
func fingerprint(sum string, extras []string) (fp, reason string) {
	reason = "changed"
	switch {
	case hexHash.MatchString(sum):
	case sum == HashMissing:
		reason = "missing"
	default:
		sum = HashUnreadable
	}
	var names []string
	for _, e := range extras {
		if e == ExtraSettingsLocal || e == ExtraMCP {
			names = append(names, e)
		}
	}
	if len(names) == 0 {
		return sum, reason
	}
	sort.Strings(names)
	return sum + "+" + strings.Join(names, "+"), "extra_config"
}

// Observe judges the effective config a hook client (or the scan) just saw for
// a principal (AC-S2-005, AC-H-070). ok is true only when the settings file
// has the registered hash and no extra config file exists. A deviation sets
// tampered = true and journals HOOK_TAMPERED once per distinct fingerprint —
// payload: principal id and reason, never file content; a file restored to
// its registered content clears the flag without an event. registered is
// false when the principal has no install on record. The registered hash is
// only replaced by Register. The transaction must run under store.Runtime.
func Observe(ctx context.Context, tx pgx.Tx, principalID, sum string, extras []string) (ok, registered bool, err error) {
	t, registered, err := observe(ctx, tx, principalID, sum, extras)
	return t == nil && registered && err == nil, registered, err
}

func observe(ctx context.Context, tx pgx.Tx, principalID, sum string, extras []string) (*Tamper, bool, error) {
	var want, last, path string
	err := tx.QueryRow(ctx, `SELECT sha256, seen_sha256, settings_path FROM hook_install WHERE principal_id = $1 FOR UPDATE`,
		principalID).Scan(&want, &last, &path)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("hookinstall: read install %s: %w", principalID, err)
	}
	seen, reason := fingerprint(sum, extras)
	t := &Tamper{PrincipalID: principalID, SettingsPath: path, Reason: reason}
	switch {
	case seen == want:
		if last != "" {
			if _, err := tx.Exec(ctx, `UPDATE hook_install SET tampered = false, seen_sha256 = '' WHERE principal_id = $1`, principalID); err != nil {
				return nil, true, fmt.Errorf("hookinstall: clear %s: %w", principalID, err)
			}
		}
		return nil, true, nil
	case seen == last:
		return t, true, nil // already reported, unchanged since
	}
	t.Seq, err = journal.Append(ctx, tx, journal.Event{Type: EventTampered,
		Payload: map[string]any{"principal_id": principalID, "reason": reason}})
	if err != nil {
		return nil, true, fmt.Errorf("hookinstall: journal %s: %w", EventTampered, err)
	}
	if _, err := tx.Exec(ctx, `UPDATE hook_install SET tampered = true, seen_sha256 = $2 WHERE principal_id = $1`, principalID, seen); err != nil {
		return nil, true, fmt.Errorf("hookinstall: flag %s: %w", principalID, err)
	}
	return t, true, nil
}

// Scan checks every registered settings file on disk the way Observe does and
// returns the changes it newly journaled (AC-H-070, limitation L2). It also
// finds a file whose hooks were removed, which no hook client reports.
// The transaction must run under store.Runtime.
func Scan(ctx context.Context, tx pgx.Tx) ([]Tamper, error) {
	rows, err := tx.Query(ctx, `SELECT principal_id::text, settings_path FROM hook_install ORDER BY principal_id FOR UPDATE`)
	if err != nil {
		return nil, fmt.Errorf("hookinstall: read installs: %w", err)
	}
	type install struct{ principal, path string }
	installs, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (install, error) {
		var i install
		return i, row.Scan(&i.principal, &i.path)
	})
	if err != nil {
		return nil, fmt.Errorf("hookinstall: read installs: %w", err)
	}
	found := []Tamper{}
	for _, i := range installs {
		t, _, err := observe(ctx, tx, i.principal, Current(i.path), ExtraConfig(WorkspaceOf(i.path)))
		if err != nil {
			return nil, err
		}
		if t != nil && t.Seq != 0 {
			found = append(found, *t)
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE hook_install SET checked_at = now()`); err != nil {
		return nil, fmt.Errorf("hookinstall: mark checked: %w", err)
	}
	return found, nil
}

// Current returns the hash of the settings file as it is now, or HashMissing /
// HashUnreadable. A file that exists but cannot be read counts as changed.
func Current(path string) string {
	sum, err := FileHash(path)
	switch {
	case err == nil:
		return sum
	case errors.Is(err, os.ErrNotExist):
		return HashMissing
	default:
		return HashUnreadable
	}
}
