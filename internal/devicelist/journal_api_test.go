package devicelist

import (
	"reflect"
	"testing"
	"time"
)

// Claim: the public journal API has only a closure importer and non-closure queries, with no exported state fields.
func TestJournalHasNoClosureExport(t *testing.T) {
	typ := reflect.TypeOf(NewObserveJournal())
	allowed := map[string]bool{"Close": true, "Head": true, "SeenUnspent": true, "Full": true}
	if _, ok := typ.MethodByName("Close"); !ok {
		t.Fatal("control: external observe-closure importer missing")
	}
	if typ.NumMethod() != len(allowed) {
		t.Fatalf("journal method count %d", typ.NumMethod())
	}
	for i := 0; i < typ.NumMethod(); i++ {
		if !allowed[typ.Method(i).Name] {
			t.Fatalf("unexpected journal export %s", typ.Method(i).Name)
		}
	}
	for i := 0; i < typ.Elem().NumField(); i++ {
		if typ.Elem().Field(i).IsExported() {
			t.Fatal("journal state field is exported")
		}
	}
}

// Claim: a stale higher head and newer seal observation leave an existing journal unchanged, while the young controls advance it.
func TestStaleHeadAndSeenStayUnchanged(t *testing.T) {
	s := newScenario(t)
	wantObservedInstruct(t, s)
	head, _ := s.journal.Head("main", "id/alisa")
	state := s.journal.owners[journalOwner{"main", "id/alisa"}]
	seen := state.seenUnspent[sealE]
	s.next()
	s.useAt(t, replaced(t, `"seq":5`, `"seq":6`), opAt(812310))
	s.edit(t, func(d *snapshotDataValue) { d.Names[0].Op = opAt(812310); d.Tip.Height++ })
	s.at.Now = now.Add(time.Minute)
	wantObservedInstruct(t, s)
	if got, _ := s.journal.Head("main", "id/alisa"); got != head || state.seenUnspent[sealE] != seen {
		t.Fatal("stale view changed a head or seen-unspent tip")
	}
	s.at = at
	s.next()
	wantObservedInstruct(t, s)
	if got, _ := s.journal.Head("main", "id/alisa"); got.Seq != 6 || state.seenUnspent[sealE].Height != seen.Height+1 {
		t.Fatalf("young control did not advance: %+v", got)
	}
}
