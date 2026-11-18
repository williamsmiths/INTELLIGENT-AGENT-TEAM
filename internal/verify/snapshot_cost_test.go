package verify

import (
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// Cost of one snapshot on a repository of about 2 000 files: it runs after
// every Bash call of a FULL lease holder, inside a 4-second budget (REV-003 #8).
func TestSnapshot_CostOn2000Files(t *testing.T) {
	repo := t.TempDir()
	gitRun(t, repo, "init", "-q", "-b", "main")
	body := make([]byte, 1500)
	for i := range body {
		body[i] = byte('a' + i%23)
	}
	for d := 0; d < 40; d++ {
		for f := 0; f < 50; f++ {
			write(t, repo, filepath.Join("pkg", "d"+string(rune('a'+d%26))+string(rune('a'+d/26)), "f"+string(rune('a'+f%26))+string(rune('a'+f/26))+".go"),
				string(body)+string(rune('a'+d%26))+string(rune('a'+f%26))+"\n")
		}
	}
	gitRun(t, repo, "add", "-A")
	gitRun(t, repo, "commit", "-q", "-m", "base")
	pin(t, repo, gitRun(t, repo, "rev-parse", "HEAD"))
	write(t, repo, "pkg/new.go", "package pkg\n")
	const rounds = 5
	start := time.Now()
	for i := 0; i < rounds; i++ {
		if got := changedFiles(t, repo); !reflect.DeepEqual(got, []string{"pkg/new.go"}) {
			t.Fatalf("changed = %v", got)
		}
	}
	per := time.Since(start) / rounds
	t.Logf("one snapshot + diff of 2001 files: %s", per)
	if per > 4*time.Second {
		t.Fatalf("one snapshot takes %s, more than the post-tool budget", per)
	}
}
