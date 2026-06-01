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
