package work_test

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/williamsmiths/kongcowork-harness/internal/kerr"
	"github.com/williamsmiths/kongcowork-harness/internal/work"
)

// AC-S2-011: a FULL lease needs a registered, untampered hook install.
func TestAC_S2_011_FullAssignmentNeedsAnIntactHookInstall(t *testing.T) {
	e := newEnv(t)
	a := e.project("alpha", 100, 10)
	w := e.agent("w", a.ID, "worker", "HOOK")
	sup := e.agent("sup", a.ID, "worker", "SUPERVISED")
	task := e.runnable(a.ID, "T-1")
	set := func(sql string) {
		t.Helper()
		e.must(e.rt(func(tx pgx.Tx) error { _, err := tx.Exec(e.ctx, sql, w.ID); return err }))
	}

	set(`UPDATE hook_install SET tampered = true WHERE principal_id = $1`)
	_, err := e.assign(e.owner, task.ID, w.ID, "FULL")
	wantKind(t, err, kerr.Conflict, "tampered install")
	// The default mode is FULL.
	_, err = e.assign(e.owner, task.ID, w.ID, "")
	wantKind(t, err, kerr.Conflict, "tampered install, default mode")

	// OBSERVE and CONTEXT promise no enforcement, so they need no install.
	ch, err := e.assign(e.owner, task.ID, w.ID, "OBSERVE")
	e.must(err)
	e.must(e.rt(func(tx pgx.Tx) error { _, err := work.ReleaseLease(e.ctx, tx, w, ch.Task.ID); return err }))

	set(`UPDATE hook_install SET tampered = false WHERE principal_id = $1`)
	ch, err = e.assign(e.owner, task.ID, w.ID, "FULL")
	e.must(err)
	e.must(e.rt(func(tx pgx.Tx) error { _, err := work.ReleaseLease(e.ctx, tx, w, ch.Task.ID); return err }))

	// A principal without blocking hooks is only observed: nothing to install (L5).
	_, err = e.assign(e.owner, task.ID, sup.ID, "FULL")
	e.must(err)
}

// PM-S2-04: the Owner sets the specialty of an agent and the reference roots of a project.
func TestPM_S2_04_SpecialtyAndReferenceRoots(t *testing.T) {
	e := newEnv(t)
	a, b := e.project("alpha", 100, 10), e.project("beta", 100, 10)
	w := e.agent("w", a.ID, "worker", "HOOK")
	repoA, repoB := e.repo(a.ID), e.repo(b.ID)
	ref, err := filepath.EvalSymlinks(t.TempDir())
	e.must(err)
	e.must(os.Symlink(ref, filepath.Join(t.TempDir(), "link")))

	var view work.PrincipalView
	e.must(e.as(e.owner, func(tx pgx.Tx) (err error) {
		view, err = work.SetSpecialty(e.ctx, tx, e.owner, w.ID, "quality")
		return err
	}))
	if view.Specialty != "quality" || view.OpenBotAgentID != "ob-w" || !reflect.DeepEqual(view.RepoRoots, []string{repoA}) {
		t.Fatalf("view = %+v", view)
	}
	err = e.as(e.owner, func(tx pgx.Tx) error { _, err := work.SetSpecialty(e.ctx, tx, e.owner, w.ID, "wizard"); return err })
	wantKind(t, err, kerr.Invalid, "unknown specialty")
	err = e.as(w, func(tx pgx.Tx) error { _, err := work.SetSpecialty(e.ctx, tx, w, w.ID, "product"); return err })
	wantKind(t, err, kerr.Forbidden, "an agent sets its own specialty")
	err = e.as(w, func(tx pgx.Tx) error { _, err := work.GetPrincipal(e.ctx, tx, w, w.ID); return err })
	wantKind(t, err, kerr.Forbidden, "an agent reads the principal view")

	setRefs := func(project string, paths ...string) ([]string, error) {
		var roots []string
		err := e.as(e.owner, func(tx pgx.Tx) (err error) {
			roots, err = work.SetReferenceRoots(e.ctx, tx, e.owner, project, paths)
			return err
		})
		return roots, err
	}
	roots, err := setRefs(a.ID, ref, ref+"/.")
	if err != nil || !reflect.DeepEqual(roots, []string{ref}) {
		t.Fatalf("reference roots = %v, %v", roots, err)
	}
	for name, path := range map[string]string{"missing": filepath.Join(ref, "nope"), "relative": "docs",
		"inside a repo of another project": repoB, "containing a repo of another project": filepath.Dir(repoB)} {
		_, err := setRefs(a.ID, path)
		wantKind(t, err, kerr.Invalid, name)
	}
	if _, err := setRefs(a.ID, ref, repoA); err != nil { // its own repo root is not "another project"
		t.Fatalf("own repo as a reference root: %v", err)
	}
	err = e.as(w, func(tx pgx.Tx) error { _, err := work.SetReferenceRoots(e.ctx, tx, w, a.ID, []string{ref}); return err })
	wantKind(t, err, kerr.Forbidden, "an agent sets reference roots")

	var got work.Roots
	e.must(e.rt(func(tx pgx.Tx) (err error) { got, err = work.PrincipalRoots(e.ctx, tx, w.ID); return err }))
	if !reflect.DeepEqual(got.Repos, []string{repoA}) || len(got.References) != 2 {
		t.Fatalf("roots of the member = %+v (the other project's repo must not appear)", got)
	}
}
