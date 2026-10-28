package main

import (
	"strings"
	"testing"
	"time"

	"github.com/chux0519/memspace/internal/adapter"
	"github.com/chux0519/memspace/internal/upgrade"
)

func TestUpgradeNotice(t *testing.T) {
	markers := t.TempDir()
	failed := upgrade.State{Target: "v0.2.0", Error: "download: HTTP 404", AttemptedAt: time.Now()}
	if n := noticeFor(failed, "v0.1.0", markers); !strings.Contains(n, "failed (download: HTTP 404)") || !strings.Contains(n, "memspace upgrade") {
		t.Fatalf("failure notice = %q", n)
	}
	if n := noticeFor(failed, "v0.1.0", markers); n != "" {
		t.Errorf("failure repeated within a day: %q", n)
	}
	if n := noticeFor(failed, "v0.2.0", markers); n != "" {
		t.Errorf("failure shown after upgrading by hand: %q", n)
	}

	done := upgrade.State{UpgradedFrom: "v0.1.0", UpgradedTo: "v0.2.0", UpgradedAt: time.Now()}
	if n := noticeFor(done, "v0.2.0", markers); !strings.Contains(n, "from v0.1.0 to v0.2.0") {
		t.Fatalf("success notice = %q", n)
	}
	if n := noticeFor(done, "v0.2.0", markers); n != "" {
		t.Errorf("success shown twice: %q", n)
	}
	done.UpgradedAt = time.Now().Add(-8 * 24 * time.Hour)
	if n := noticeFor(done, "v0.2.0", t.TempDir()); n != "" {
		t.Errorf("old upgrade announced: %q", n)
	}
}

func TestVersionCheck(t *testing.T) {
	for _, c := range []struct {
		client, server string
		auto           bool
		st             upgrade.State
		ok             bool
		want           string
	}{
		{"v0.2.0", "v0.2.0", true, upgrade.State{}, true, ""},
		{"devel-abc", "v0.2.0", true, upgrade.State{}, true, ""},
		{"v0.1.0", "v0.2.0", true, upgrade.State{}, false, "upgrades itself"},
		{"v0.1.0", "v0.2.0", false, upgrade.State{}, false, "auto_upgrade is off"},
		{"v0.1.0", "v0.2.0", true, upgrade.State{Target: "v0.2.0", Error: "HTTP 404"}, false, "failed"},
	} {
		got := versionCheck(c.client, c.server, c.auto, c.st)
		if (got.Status == adapter.OK) != c.ok || !strings.Contains(got.Detail, c.want) {
			t.Errorf("%s/%s: %+v", c.client, c.server, got)
		}
	}
}
