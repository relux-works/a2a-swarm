package cli

import (
	"testing"

	"github.com/relux-works/a2a-swarm/internal/config"
)

func testSetBindControl(t *testing.T, paths config.Paths, name string, target config.BindControlTarget, proof config.BindControlProof) config.BindPartView {
	t.Helper()
	state, err := config.LoadState(paths, name)
	if err != nil {
		t.Fatal(err)
	}
	view, err := config.ReadBindPartView(paths, name, state)
	if err != nil {
		t.Fatal(err)
	}
	updated, err := config.SetBindControl(paths, name,
		config.BindControlExpectation{Revision: view.Revision, Generation: view.Generation, SessionID: view.SessionID},
		target, proof)
	if err != nil {
		t.Fatal(err)
	}
	return updated
}

func testRecordBindNotice(t *testing.T, paths config.Paths, name string, notice config.OwnNoticeObservation, publication string) config.BindPartView {
	t.Helper()
	state, err := config.LoadState(paths, name)
	if err != nil {
		t.Fatal(err)
	}
	view, err := config.ReadBindPartView(paths, name, state)
	if err != nil {
		t.Fatal(err)
	}
	updated, err := config.RecordBindNotice(paths, name, view.Revision, view.Generation, notice, publication)
	if err != nil {
		t.Fatal(err)
	}
	return updated
}
