package inboundmedia

import (
	"context"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNativeOversizedMetadataOrder(t *testing.T) {
	body := strings.Repeat("a", 65536)
	for _, kind := range []string{"m.file", "m.image", "m.audio", "m.video"} {
		for _, first := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/kind-first=%t", kind, first), func(t *testing.T) {
				f := newFixture(t, []byte("valid"), 64)
				fields := []string{`"msgtype":"` + kind + `"`, `"body":"` + body + `"`}
				if !first {
					fields[0], fields[1] = fields[1], fields[0]
				}
				ev := nativeEvent(`{` + strings.Join(fields, ",") + `,"url":"mxc://remote.invalid/media-id"}`)
				m := Inspect(ev, selection.RoomID)
				if m == nil || m.Kind != kind || m.Refusal != MetadataInvalid || m.Handle != selection {
					t.Fatalf("oversized native projection: %+v", m)
				}
				_, err := f.fetch(ev, "refused")
				wantRefusal(t, err, MetadataInvalid)
				if f.configs.Load() != 0 || f.downloads.Load() != 0 {
					t.Fatal("oversized metadata reached media preflight/transfer")
				}
				cleanRoot(t, f.root)
				if _, err := f.fetch(validEvent(kind, 5), "valid-neighbour"); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestNativeConfigContextClassification(t *testing.T) {
	for _, name := range []string{"already-cancelled", "already-deadline", "cancel-during-config", "deadline-during-config", "config-failure"} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, []byte("valid"), 64)
			ctx, cancel := context.WithCancel(context.Background())
			want := Cancelled
			switch name {
			case "already-cancelled":
				cancel()
			case "already-deadline", "deadline-during-config":
				cancel()
				duration := 25 * time.Millisecond
				if name == "already-deadline" {
					duration = -time.Second
				}
				ctx, cancel = context.WithTimeout(context.Background(), duration)
				want = Timeout
			case "config-failure":
				want = LimitUnavailable
			}
			defer cancel()
			original := f.c.HTTP
			calls := 0
			f.c.HTTP = &http.Client{Transport: mediaTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				if !strings.HasSuffix(r.URL.Path, "/config") {
					t.Error("unexpected media transfer")
				}
				if name == "config-failure" {
					return nil, fmt.Errorf("synthetic configuration failure")
				}
				if name == "cancel-during-config" {
					cancel()
				}
				<-r.Context().Done()
				return nil, r.Context().Err()
			})}
			_, err := Fetch(ctx, f.c, validEvent("m.file", 5), selection, filepath.Join(f.root, "refused"))
			wantRefusal(t, err, want)
			if strings.HasPrefix(name, "already-") && calls != 0 {
				t.Fatal("ended caller context made a preflight request")
			}
			cleanRoot(t, f.root)
			f.c.HTTP = original
			if _, err := f.fetch(validEvent("m.file", 5), "valid-neighbour"); err != nil {
				t.Fatal(err)
			}
		})
	}
}
