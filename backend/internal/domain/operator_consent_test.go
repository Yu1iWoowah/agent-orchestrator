package domain

import "testing"

func TestOperatorConsentReadinessAlwaysClosed(t *testing.T) {
	for _, purpose := range []OperatorPurpose{ConsentHandoffContext, ConsentHandoffRetryRestore, ConsentSettingsPermissions, ConsentSettingsBatch, ConsentDeliveryDefer, ConsentCoordPolicy, "coord.held_source.disclose", "*"} {
		r := GetAuthorityReadiness([]OperatorPurpose{purpose}, OperatorActionScope{ProjectRef: "p", OwnerRef: "owner"})
		if r.AuthorityGo || r.NativeIssuerDelivered || !r.CoreDelivered || len(r.PurposeSet) != 0 || r.IssuerEpoch != "" || r.ExpiresAt != nil || r.QualificationRef != "" {
			t.Fatalf("authority enabled: %+v", r)
		}
	}
	for _, p := range []OperatorPurpose{ConsentCoordPolicy, "coord.held_source.disclose", "*", "settings.*", "unknown"} {
		if _, active := p.Domain(); active {
			t.Fatalf("optional/unregistered purpose active: %s", p)
		}
	}
}
