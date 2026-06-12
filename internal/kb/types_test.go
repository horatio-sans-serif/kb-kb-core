package kb

import "testing"

func TestPrivacyTier_ValidAndAllowsCloud(t *testing.T) {
	for _, tier := range []PrivacyTier{Public, Personal, Sensitive, Restricted} {
		if !tier.Valid() {
			t.Errorf("%q should be valid", tier)
		}
	}
	if PrivacyTier("bogus").Valid() {
		t.Error("unknown tier should be invalid")
	}

	if Sensitive.AllowsCloud() || Restricted.AllowsCloud() {
		t.Error("sensitive/restricted must not be processable by cloud services")
	}
	if !Public.AllowsCloud() || !Personal.AllowsCloud() {
		t.Error("public/personal should be cloud-allowed")
	}
}

func TestPrivacyTier_Ordering(t *testing.T) {
	// Rank is the sensitivity order the export filter (§8.14) and privacy_max
	// depend on: public < personal < sensitive < restricted.
	order := []PrivacyTier{Public, Personal, Sensitive, Restricted}
	for i := 1; i < len(order); i++ {
		if order[i-1].Rank() >= order[i].Rank() {
			t.Errorf("%q should rank below %q", order[i-1], order[i])
		}
	}

	// AtMost is the per-artifact export-filter predicate: an artifact is included
	// when its tier is no more sensitive than the ceiling.
	if !Personal.AtMost(Sensitive) {
		t.Error("personal should be allowed under a sensitive ceiling")
	}
	if Restricted.AtMost(Personal) {
		t.Error("restricted must be excluded under a personal ceiling")
	}
	if !Restricted.AtMost(Restricted) {
		t.Error("a tier is at most itself")
	}
}

func TestMaxTier(t *testing.T) {
	if got := MaxTier(Public, Restricted, Personal); got != Restricted {
		t.Errorf("MaxTier = %q, want restricted", got)
	}
	// privacy_max of an empty render defaults to the least-sensitive tier.
	if got := MaxTier(); got != Public {
		t.Errorf("MaxTier() of nothing = %q, want public", got)
	}
}

func TestTiersAtMost(t *testing.T) {
	cases := map[PrivacyTier]int{
		Public:     1, // only public
		Personal:   2,
		Sensitive:  3,
		Restricted: 4, // all
	}
	for ceiling, want := range cases {
		got := TiersAtMost(ceiling)
		if len(got) != want {
			t.Errorf("TiersAtMost(%s) = %v (%d), want %d tiers", ceiling, got, len(got), want)
		}
		for _, tier := range got {
			if !tier.AtMost(ceiling) {
				t.Errorf("TiersAtMost(%s) included %s, which is above the ceiling", ceiling, tier)
			}
		}
	}
	// Public ceiling sees only public.
	if got := TiersAtMost(Public); len(got) != 1 || got[0] != Public {
		t.Errorf("TiersAtMost(Public) = %v, want [public]", got)
	}
	// An unknown ceiling fails closed (sees nothing).
	if got := TiersAtMost(PrivacyTier("bogus")); got != nil {
		t.Errorf("TiersAtMost(bogus) = %v, want nil (fail closed)", got)
	}
}
