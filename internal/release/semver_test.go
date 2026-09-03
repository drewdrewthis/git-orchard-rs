package release_test

import (
	"testing"

	"github.com/drewdrewthis/orchardist/internal/release"
)

func TestCompare_OrdersVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"1.2.3", "1.2.3", 0},
		{"v1.2.3", "1.2.3", 0}, // the leading v is optional on either side
		{"1.2.4", "1.2.3", 1},
		{"1.2.3", "1.3.0", -1},
		{"1.10.0", "1.9.0", 1}, // numeric, not lexical
		{"2.0.0", "10.0.0", -1},
		{"1.0.0", "1.0.0-rc1", 1}, // a release outranks its own prerelease
	}
	for _, c := range cases {
		if got := release.Compare(c.a, c.b); got != c.want {
			t.Errorf("Compare(%q,%q) = %d; want %d", c.a, c.b, got, c.want)
		}
	}
}

// @scenario A dev build is always treated as older than any published release
//
// A dev build must always be offered the upgrade — never told it is current.
func TestCompare_DevIsOlderThanEveryRelease(t *testing.T) {
	if got := release.Compare(release.DevVersion, "0.0.1"); got != -1 {
		t.Errorf("Compare(dev, 0.0.1) = %d; want -1", got)
	}
	if got := release.Compare("0.0.1", release.DevVersion); got != 1 {
		t.Errorf("Compare(0.0.1, dev) = %d; want 1", got)
	}
	if got := release.Compare(release.DevVersion, release.DevVersion); got != 0 {
		t.Errorf("Compare(dev, dev) = %d; want 0", got)
	}
	if !release.IsNewer("1.0.0", release.DevVersion) {
		t.Error("IsNewer(1.0.0, dev) = false; a dev build must always see an update")
	}
}

func TestCompare_GarbageSortsLikeDev(t *testing.T) {
	for _, junk := range []string{"", "   ", "not-a-version", "1.2.3.4.5-x y"} {
		if got := release.Compare(junk, "1.0.0"); got != -1 {
			t.Errorf("Compare(%q, 1.0.0) = %d; want -1", junk, got)
		}
	}
}

func TestIsSemver(t *testing.T) {
	cases := []struct {
		v    string
		want bool
	}{
		{"1.2.3", true},
		{"v1.2.3", true},
		{"dev", false},
		{"abc1234-dirty", false},
		{"", false},
	}
	for _, c := range cases {
		if got := release.IsSemver(c.v); got != c.want {
			t.Errorf("IsSemver(%q) = %v; want %v", c.v, got, c.want)
		}
	}
}

func TestIsNewer_EqualIsNotNewer(t *testing.T) {
	if release.IsNewer("1.2.3", "1.2.3") {
		t.Error("IsNewer on equal versions = true; an up-to-date install must report no update")
	}
	if release.IsNewer("1.2.2", "1.2.3") {
		t.Error("IsNewer(older, newer) = true")
	}
}
