package config

import "testing"

func TestMailboxPendingViewPreservesUnread(t *testing.T) {
	p := testPaths(t)
	mb, err := OpenMailbox(p, "a")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = mb.Append([]MailEntry{ev("$held"), ev("$visible")}); err != nil {
		t.Fatal(err)
	}
	mb.SetPendingFilter(func(e MailEntry) bool { return e.ID == "$visible" })
	if got := mb.Pending(); len(got) != 1 || got[0].ID != "$visible" {
		t.Fatal("view did not restrict delivery", got)
	}
	if !mb.Has("$held") || mb.Received("$held") {
		t.Fatal("view changed identity/receipt state")
	}
	if _, err = mb.Receive([]string{"$visible"}); err != nil {
		t.Fatal(err)
	}
	if err = mb.compact(); err != nil {
		t.Fatal(err)
	}
	mb, err = OpenMailbox(p, "a")
	if err != nil {
		t.Fatal(err)
	}
	if got := mb.Pending(); len(got) != 1 || got[0].ID != "$held" || mb.Received("$held") {
		t.Fatal("filtered unread lost during compaction/reload", got)
	}
}
