package hookinstall

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// The sandbox placeholders of Claude Code (empty read-only files) are not
// extra configuration; anything with content, a symlink or a directory is.
func TestExtraConfigIgnoresSandboxPlaceholders(t *testing.T) {
	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, ".claude"), 0o700); err != nil {
		t.Fatal(err)
	}
	local, mcp := filepath.Join(ws, ".claude", ExtraSettingsLocal), filepath.Join(ws, ExtraMCP)
	if got := ExtraConfig(ws); len(got) != 0 {
		t.Fatalf("nothing there: %v", got)
	}
	for _, p := range []string{local, mcp} {
		if err := os.WriteFile(p, nil, 0o444); err != nil {
			t.Fatal(err)
		}
	}
	if got := ExtraConfig(ws); len(got) != 0 {
		t.Fatalf("empty placeholders reported: %v", got)
	}
	if err := os.Chmod(mcp, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mcp, []byte(`{"mcpServers":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(local); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/hostname", local); err != nil {
		t.Fatal(err)
	}
	got := ExtraConfig(ws)
	if !slices.Equal(got, []string{ExtraMCP, ExtraSettingsLocal}) {
		t.Fatalf("got %v, want both", got)
	}
}
