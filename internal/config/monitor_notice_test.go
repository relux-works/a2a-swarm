package config

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestCMNoticeTimestampFence(t *testing.T) {
	p := monitorSeed(t)
	if err := monitorRun(p, MonitorArm, func(g *MonitorGeneration) error {
		before := monitorDisk(t, p)
		if err := g.RecordNotice(strings.Repeat("b", 32), time.Now()); err != ErrMonitorGeneration || !bytes.Equal(before, monitorDisk(t, p)) {
			t.Fatal("stale generation stamped a notice")
		}
		if err := g.Announce(monitorRecord().Generation, func() error { return ErrMonitorOutput }); err != ErrMonitorOutput || !bytes.Equal(before, monitorDisk(t, p)) {
			t.Fatal("failed output stamped a notice")
		}
		if err := g.Announce(monitorRecord().Generation, func() error { return nil }); err != nil {
			return err
		}
		at := time.Now()
		if err := g.RecordNotice(monitorRecord().Generation, at); err != nil {
			return err
		}
		if !monitorLoad(t, p).Monitor.LastNoticeAt.Equal(at) {
			t.Fatal("successful notice timestamp missing")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
