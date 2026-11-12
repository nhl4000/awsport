package awsx

import (
	"fmt"

	"github.com/nhl4000/awsport/internal/core"
)

// NeedBastionError is returned by PickBastion when no bastion can be resolved
// automatically; the user must pick one of Candidates (running, SSM-online
// EC2 instances in the same profile and region).
type NeedBastionError struct {
	Profile, Region string
	Candidates      []core.Resource
}

func (e *NeedBastionError) Error() string {
	if len(e.Candidates) == 0 {
		return fmt.Sprintf("no bastion for profile %s in %s: no running EC2 instance with SSM online", e.Profile, e.Region)
	}
	return fmt.Sprintf("no bastion for profile %s in %s: choose one of %d SSM-online instances (or tag one %s=true)",
		e.Profile, e.Region, len(e.Candidates), core.TagBastion)
}

// PickBastion resolves the bastion used to reach RDS in profile/region:
// an instance tagged awsport:bastion=true, then one whose Name tag matches
// one of names (exact, or with one '*' wildcard; earlier names first), then
// the saved instance ID. Only running, SSM-online EC2 instances qualify.
// Otherwise it returns *NeedBastionError.
func PickBastion(resources []core.Resource, profile, region, saved string, names []string) (core.Resource, error) {
	var candidates []core.Resource
	for _, r := range resources {
		if r.Kind == core.KindEC2 && r.Profile == profile && r.Region == region &&
			r.State == "running" && r.SSMOnline {
			candidates = append(candidates, r)
		}
	}
	matchers := []func(core.Resource) bool{func(r core.Resource) bool { return r.IsBastion }}
	for _, n := range names {
		matchers = append(matchers, func(r core.Resource) bool { return nameMatches(n, r.Name) })
	}
	matchers = append(matchers, func(r core.Resource) bool { return saved != "" && r.ID == saved })
	for _, match := range matchers {
		for _, r := range candidates {
			if match(r) {
				return r, nil
			}
		}
	}
	return core.Resource{}, &NeedBastionError{Profile: profile, Region: region, Candidates: candidates}
}
