package v1alpha1

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestStampOwnerLabel pins the write path's anti-spoof validator at the API
// layer (PR #427 review P9.1): the owner label is the security boundary every
// read path filters by, so the validator's edges — empty, oversized, invalid
// characters, leading/trailing punctuation — are exercised here, where a
// regex widening cannot slip through unseen.
func TestStampOwnerLabel(t *testing.T) {
	tests := []struct {
		name    string
		owner   string
		wantErr bool
	}{
		{"plain username", "alice", false},
		{"single char", "a", false},
		{"dots underscores hyphens", "alice.smith_ops-2", false},
		{"max length", strings.Repeat("a", 63), false},
		{"empty", "", true},
		{"too long", strings.Repeat("a", 64), true},
		{"at sign (email-style identity)", "alice@example.com", true},
		{"leading dot", ".alice", true},
		{"trailing dot", "alice.", true},
		{"leading hyphen", "-alice", true},
		{"space", "al ice", true},
		{"colon (label-value illegal)", "dev:alice", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			wf := &Workflow{ObjectMeta: metav1.ObjectMeta{Name: "probe", Namespace: "ns"}}
			err := StampOwnerLabel(wf, tc.owner)
			if tc.wantErr && err == nil {
				t.Errorf("owner %q accepted — want invalid-label-value error", tc.owner)
			}
			if !tc.wantErr {
				if err != nil {
					t.Fatalf("owner %q rejected: %v", tc.owner, err)
				}
				if got := wf.Labels[OwnerLabel]; got != tc.owner {
					t.Errorf("stamped label = %q, want %q", got, tc.owner)
				}
			}
		})
	}
}
