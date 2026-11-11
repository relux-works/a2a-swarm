package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
)

// The configured bot, not the fixture's default literal, controls refusal.
// Matching state/replies and a former-default positive prevent a hardcoded bot
// implementation from satisfying these file/fake-Keychain controls. A case
// variant is a distinct principal and remains admitted under exact equality.
func TestAdminSubscriptionConfiguredPrincipal(t *testing.T) {
	const alternate = "@alternate:test.invalid"
	for _, backend := range []string{"file", "keychain"} {
		for _, tc := range []struct {
			name, bot, from string
			expired, ok     bool
		}{
			{"alternate-exact", alternate, alternate, false, false},
			{"alternate-kind-expired", alternate, alternate, true, false},
			{"former-default", alternate, admin198Bot, false, true},
			{"owner", alternate, admin198Owner, false, true},
			{"default-exact", admin198Bot, admin198Bot, false, false},
			{"case-variant", admin198Bot, "@SERVER:test.invalid", false, true},
		} {
			t.Run(backend+"/"+tc.name, func(t *testing.T) {
				f := newAdmin198Backend(t, "positive", backend)
				f.c.AdminRegistration.ServerBotUserID = tc.bot
				f.c.AdminRegistration.ExpectedMembers = []string{admin198User, tc.bot}
				f.save()
				selector := SubSelector{From: tc.from}
				if tc.expired {
					selector.Kinds = []string{"question"}
					selector.Until = "2000-01-01T00:00:00Z"
				}
				f.subscriptions = &SubList{V: 1, Subs: []Sub{{ID: "principal", SubSelector: selector, CreatedAt: f.now.Add(-time.Minute).Format(createdAtLayout)}}}
				f.seedExistingMute()
				before := f.subscriptionSnapshot()
				var out, stderr bytes.Buffer
				a, _ := memberRootApp(f, &out, &stderr)
				code := a.Run(context.Background(), []string{"--as", "admin", "connection", "register", "newbie", "--via-admin-room"})
				p, err := f.c.Paths(config.Paths{Base: f.root}, "newbie", false)
				if err != nil {
					t.Fatal("principal control paths unavailable")
				}
				if tc.ok {
					if code != 0 || f.issues != 1 || f.auths != 1 || f.registrations != 2 || f.redactions != 2 || !strings.Contains(out.String(), "registered newbie;") {
						t.Fatal("distinct principal did not complete valid registration")
					}
					creds, err := config.LoadCredentialMetadata(p, "newbie", false)
					binding, e := f.c.Binding("newbie", false)
					if err != nil || e != nil || config.ValidateBoundCredentials(creds, binding) != nil {
						t.Fatal("principal positive lacks bound credentials")
					}
				} else {
					want := "a2a: admin registration stage=room-subscriptions errcode=LOCAL_REFUSAL HTTP=0; no automatic retry\n"
					if code == 0 || stderr.String() != want || out.Len() != 0 || f.issues != 0 || f.auths != 0 || f.registrations != 0 || f.sends != 0 || f.redactions != 0 || len(f.store.creates) != 0 {
						t.Fatal("configured principal did not refuse before effects")
					}
					for _, path := range []string{p.AgentFile("newbie"), filepath.Join(p.Base, "registration.pending.json")} {
						if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
							t.Fatal("principal refusal published credentials or journal")
						}
					}
				}
				f.assertSubscriptionsUnchanged(before)
				f.assertNoExports(out.Bytes(), stderr.Bytes())
			})
		}
	}
}

// At the selector matcher, the exact bot row matches only the exact sender and
// the case-variant row only its own distinct sender. This is not a delivery-path
// proof; the independent protected-room delivery/history tests remain in place.
func TestAdminSubscriptionExactSenderMatching(t *testing.T) {
	f := newAdmin198(t, "positive")
	ev := f.reply()
	created := f.now.Add(-time.Minute).Format(createdAtLayout)
	exact := Sub{ID: "exact", SubSelector: SubSelector{From: admin198Bot}, CreatedAt: created}
	variant := Sub{ID: "variant", SubSelector: SubSelector{From: "@SERVER:test.invalid"}, CreatedAt: created}
	if !exact.matches(ev, admin198Room, Header{}, false) || variant.matches(ev, admin198Room, Header{}, false) {
		t.Fatal("bot event lost exact/nonmatching case controls")
	}
	ev.Sender = variant.From
	if exact.matches(ev, admin198Room, Header{}, false) || !variant.matches(ev, admin198Room, Header{}, false) {
		t.Fatal("variant event lost exact/nonmatching case controls")
	}
}
