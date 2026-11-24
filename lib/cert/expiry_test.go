package cert

import (
	"slices"
	"testing"
	"time"
)

func TestParseWindow(t *testing.T) {
	for in, want := range map[string]time.Duration{
		"30d": 30 * 24 * time.Hour, "0d": 0, "72h": 72 * time.Hour, "90m": 90 * time.Minute,
	} {
		if got, err := ParseWindow(in); err != nil || got != want {
			t.Errorf("ParseWindow(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "d", "-1d", "1.5d", "soon", "-2h"} {
		if _, err := ParseWindow(bad); err == nil {
			t.Errorf("ParseWindow(%q) accepted", bad)
		}
	}
}

func TestExpiring(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	list := []Summary{
		{Name: "expired", NotAfter: now.Add(-time.Hour)},
		{Name: "soon", NotAfter: now.Add(24 * time.Hour)},
		{Name: "later", NotAfter: now.Add(60 * 24 * time.Hour)},
		{Name: "unissued"},
	}
	var got []string
	for _, s := range Expiring(list, 30*24*time.Hour, now) {
		got = append(got, s.Name)
	}
	if !slices.Equal(got, []string{"expired", "soon"}) {
		t.Errorf("Expiring = %v", got)
	}
	if n := len(Expiring(list, 0, now)); n != 1 {
		t.Errorf("window 0 found %d, want only the expired one", n)
	}
}

func TestExpiringNoneIsEmptyNotNil(t *testing.T) {
	if got := Expiring(nil, time.Hour, time.Now()); got == nil {
		t.Error("Expiring returned nil; -o json would print null")
	}
}
