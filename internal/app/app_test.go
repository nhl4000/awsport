package app

import (
	"strings"
	"testing"

	"github.com/nhl4000/awsport/internal/core"
)

func TestMatchMember(t *testing.T) {
	resources := []core.Resource{
		{Kind: core.KindEC2, Profile: "example-dev", Region: "us-east-1", ID: "i-0b", Name: "app"},
		{Kind: core.KindEC2, Profile: "example-dev", Region: "us-east-1", ID: "i-0a", Name: "app"},
		{Kind: core.KindEC2, Profile: "example-dev", Region: "us-east-1", ID: "i-0c", Name: "old", Unreachable: "instance stopped"},
		{Kind: core.KindEC2, Profile: "example-dev", Region: "us-west-2", ID: "i-0d", Name: "multi"},
		{Kind: core.KindEC2, Profile: "example-dev", Region: "us-east-1", ID: "i-0e", Name: "multi"},
		{Kind: core.KindRDS, Profile: "example-dev", Region: "us-east-1", ID: "db", Name: "db"},
		{Kind: core.KindEC2, Profile: "example-dev", Region: "us-east-1", ID: "i-0f", Name: "db"},
		{Kind: core.KindEC2, Profile: "example-prod", Region: "us-east-1", ID: "i-0g", Name: "app"},
		{Kind: core.KindEC2, Profile: "example-dev", Region: "us-east-1", ID: "i-0h", Name: "replaced", Unreachable: "instance terminated"},
		{Kind: core.KindEC2, Profile: "example-dev", Region: "us-east-1", ID: "i-0i", Name: "replaced"},
	}
	tests := []struct {
		name    string
		member  core.GroupMember
		wantID  string
		wantErr string
	}{
		{"lowest id wins within region", core.GroupMember{Profile: "example-dev", Name: "app", Kind: core.KindEC2}, "i-0a", ""},
		{"profile scoped", core.GroupMember{Profile: "example-prod", Name: "app"}, "i-0g", ""},
		{"unreachable reported", core.GroupMember{Profile: "example-dev", Name: "old"}, "", "instance stopped"},
		{"missing", core.GroupMember{Profile: "example-dev", Name: "nope"}, "", "no resource named"},
		{"ambiguous regions", core.GroupMember{Profile: "example-dev", Name: "multi"}, "", "set a region"},
		{"region disambiguates", core.GroupMember{Profile: "example-dev", Name: "multi", Region: "us-west-2"}, "i-0d", ""},
		{"kind filters", core.GroupMember{Profile: "example-dev", Name: "db", Kind: core.KindRDS}, "db", ""},
		{"ambiguous kinds", core.GroupMember{Profile: "example-dev", Name: "db"}, "", "set a kind"},
		{"wrong kind", core.GroupMember{Profile: "example-dev", Name: "app", Kind: core.KindRDS}, "", "no resource named"},
		{"reachable preferred over replaced", core.GroupMember{Profile: "example-dev", Name: "replaced"}, "i-0i", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := MatchMember(resources, tt.member)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.ID != tt.wantID {
				t.Fatalf("ID = %q, want %q", got.ID, tt.wantID)
			}
		})
	}
}

func TestMergeGroups(t *testing.T) {
	cfg := []core.Group{
		{Name: "b", Members: []core.GroupMember{{Name: "from-config"}}},
		{Name: "a", Members: []core.GroupMember{{Name: "a1"}}},
	}
	saved := []core.Group{
		{Name: "b", Members: []core.GroupMember{{Name: "from-state"}}},
		{Name: "c", Members: []core.GroupMember{{Name: "c1"}}},
	}
	got := mergeGroups(cfg, saved)
	var names []string
	for _, g := range got {
		names = append(names, g.Name)
	}
	if strings.Join(names, ",") != "a,b,c" {
		t.Fatalf("names = %v", names)
	}
	if got[1].Members[0].Name != "from-state" {
		t.Fatalf("state should win, got %q", got[1].Members[0].Name)
	}
}
