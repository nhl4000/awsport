package awsx

import (
	"context"
	"errors"
	"reflect"
	"strconv"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/smithy-go"

	"github.com/nhl4000/awsport/internal/core"
)

func TestRegions(t *testing.T) {
	tests := []struct {
		name   string
		region string
		extra  []string
		want   []string
	}{
		{"profile only", "ca-central-1", nil, []string{"ca-central-1"}},
		{"dedupe keeps order", "us-east-2", []string{"ca-central-1", "us-east-2", " ca-central-1 ", ""}, []string{"us-east-2", "ca-central-1"}},
		{"no profile region", "", []string{"eu-west-1"}, []string{"eu-west-1"}},
		{"nothing", "", nil, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Regions(core.Profile{Region: tt.region}, tt.extra)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Regions = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestParsePortsTag(t *testing.T) {
	tests := []struct {
		in   string
		want []core.PortSpec
	}{
		{"mailpit:8025,app:8080", []core.PortSpec{{Name: "mailpit", Remote: 8025}, {Name: "app", Remote: 8080}}},
		{" mailpit : 8025 , 9000 ,, app:8080 ", []core.PortSpec{{Name: "mailpit", Remote: 8025}, {Name: "", Remote: 9000}, {Name: "app", Remote: 8080}}},
		{"web:80 api:3000", []core.PortSpec{{Name: "web", Remote: 80}, {Name: "api", Remote: 3000}}},
		{"bad:x,0,70000,ok:22", []core.PortSpec{{Name: "ok", Remote: 22}}},
		{"", nil},
	}
	for _, tt := range tests {
		if got := parsePortsTag(tt.in); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("parsePortsTag(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

func TestMergePorts(t *testing.T) {
	presets := []core.PortSpec{{Name: "https", Remote: 443}, {Name: "http", Remote: 80}, {Name: "app", Remote: 8080}}
	tag := []core.PortSpec{{Name: "api", Remote: 8080}, {Name: "", Remote: 443}, {Name: "mailpit", Remote: 8025}}
	manual := []core.PortSpec{{Name: "debug", Remote: 9229}, {Name: "admin", Remote: 80}, {Name: "bogus", Remote: 0}}
	got := mergePorts(presets, tag, manual)
	want := []core.PortSpec{{Name: "admin", Remote: 80}, {Name: "https", Remote: 443}, {Name: "mailpit", Remote: 8025}, {Name: "api", Remote: 8080}, {Name: "debug", Remote: 9229}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("mergePorts = %v, want %v", got, want)
	}
	if got := mergePorts(); len(got) != 0 {
		t.Errorf("mergePorts() = %v, want empty", got)
	}
}

func TestPickBastion(t *testing.T) {
	ec2 := func(id, name, state string, online, tagged bool) core.Resource {
		return core.Resource{Kind: core.KindEC2, Profile: "example", Region: "ca-central-1", ID: id, Name: name,
			State: state, SSMOnline: online, IsBastion: tagged}
	}
	app := ec2("i-app", "app", "running", true, false)
	named := ec2("i-named", core.DefaultBastionName, "running", true, false)
	tagged := ec2("i-tagged", "jump", "running", true, true)
	taggedOffline := ec2("i-tagged-off", "jump-old", "running", false, true)
	taggedStopped := ec2("i-tagged-stop", "jump-stopped", "stopped", true, true)
	otherRegion := tagged
	otherRegion.ID, otherRegion.Region = "i-elsewhere", "us-east-2"
	db := core.Resource{Kind: core.KindRDS, Profile: "example", Region: "ca-central-1", ID: "db", State: "available"}

	tests := []struct {
		name       string
		resources  []core.Resource
		saved      string
		wantID     string
		candidates []string // when NeedBastionError expected
	}{
		{"tag wins over name and saved", []core.Resource{app, named, tagged}, "i-app", "i-tagged", nil},
		{"name wins over saved", []core.Resource{app, named, taggedOffline}, "i-app", "i-named", nil},
		{"saved used", []core.Resource{app, taggedStopped, db}, "i-app", "i-app", nil},
		{"saved must be connectable", []core.Resource{app, taggedOffline}, "i-tagged-off", "", []string{"i-app"}},
		{"other region ignored", []core.Resource{otherRegion, app}, "", "", []string{"i-app"}},
		{"no candidates", []core.Resource{db, taggedStopped}, "", "", []string{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := PickBastion(tt.resources, "example", "ca-central-1", tt.saved)
			if tt.candidates == nil {
				if err != nil || got.ID != tt.wantID {
					t.Fatalf("got (%s, %v), want %s", got.ID, err, tt.wantID)
				}
				return
			}
			var need *NeedBastionError
			if !errors.As(err, &need) {
				t.Fatalf("err = %v, want *NeedBastionError", err)
			}
			ids := []string{}
			for _, c := range need.Candidates {
				ids = append(ids, c.ID)
			}
			if !reflect.DeepEqual(ids, tt.candidates) || need.Profile != "example" || need.Region != "ca-central-1" {
				t.Errorf("NeedBastionError = %+v, want candidates %v", need, tt.candidates)
			}
			if need.Error() == "" {
				t.Error("empty error message")
			}
		})
	}
}

// pageIndex and nextToken page fake SDK outputs: NextToken = page index.
func pageIndex(token *string) int {
	if token == nil {
		return 0
	}
	i, _ := strconv.Atoi(*token)
	return i
}

func nextToken(i, n int) *string {
	if i+1 >= n {
		return nil
	}
	return aws.String(strconv.Itoa(i + 1))
}

// Fakes return the already-paginated results of one region.

type fakeEC2 struct {
	instances []ec2Instance
	err       error
}

func (f *fakeEC2) describeInstances(context.Context) ([]ec2Instance, error) {
	return f.instances, f.err
}

type fakeSSM struct {
	infos []ssmInstanceInfo
	err   error
}

func (f *fakeSSM) describeOnlineInstances(context.Context) ([]ssmInstanceInfo, error) {
	return f.infos, f.err
}

type fakeRDS struct {
	instances  []dbInstance
	clusters   []dbCluster
	instErr    error
	clusterErr error
}

func (f *fakeRDS) describeDBInstances(context.Context) ([]dbInstance, error) {
	return f.instances, f.instErr
}

func (f *fakeRDS) describeDBClusters(context.Context) ([]dbCluster, error) {
	return f.clusters, f.clusterErr
}

func instance(id, name, state string, tags map[string]string) ec2Instance {
	inst := ec2Instance{ID: id, State: state}
	if name != "" {
		inst.Tags = append(inst.Tags, ec2Tag{Key: "Name", Value: name})
	}
	for k, v := range tags {
		inst.Tags = append(inst.Tags, ec2Tag{Key: k, Value: v})
	}
	return inst
}

func TestDiscoverRegion(t *testing.T) {
	clients := regionClients{
		ec2: &fakeEC2{instances: []ec2Instance{
			instance("i-web", "web", "running", map[string]string{
				core.TagPorts: "mailpit:8025, api:3000",
				core.TagShell: "bash -l",
			}),
			instance("i-gone", "gone", "terminated", nil),
			instance("i-jump", "jump", "running", map[string]string{core.TagBastion: "TRUE"}),
			instance("i-stopped", "", "stopped", nil),
		}},
		ssm: &fakeSSM{infos: []ssmInstanceInfo{
			{InstanceID: "i-web", PingStatus: "Online"},
			{InstanceID: "i-stopped", PingStatus: "ConnectionLost"},
		}},
		rds: &fakeRDS{
			instances: []dbInstance{
				{ID: "standalone", Status: "available", Engine: "postgres", Address: "standalone.example.internal", Port: 5432},
				{ID: "aurora-1", ClusterID: "aurora", Status: "available"},
				{ID: "paused", Status: "stopped", Engine: "mysql"},
			},
			clusters: []dbCluster{
				{ID: "aurora", Status: "available", Engine: "aurora-postgresql", Endpoint: "aurora.cluster-example.internal", Port: 5432},
			},
		},
	}
	var manualCalls []string
	opts := discoverOptions{
		profile:      "example",
		region:       "ca-central-1",
		presets:      []core.PortSpec{{Name: "http", Remote: 80}},
		shellDefault: "sh",
		manual: func(profile, region, name string) []core.PortSpec {
			manualCalls = append(manualCalls, profile+"/"+region+"/"+name)
			if name == "web" {
				return []core.PortSpec{{Name: "debug", Remote: 9229}}
			}
			return nil
		},
	}

	got, err := discoverRegion(context.Background(), clients, opts)
	if err != nil {
		t.Fatal(err)
	}
	base := core.Resource{Profile: "example", Region: "ca-central-1"}
	ec2r := func(r core.Resource) core.Resource {
		r.Kind, r.Profile, r.Region = core.KindEC2, base.Profile, base.Region
		return r
	}
	rdsr := func(r core.Resource) core.Resource {
		r.Kind, r.Profile, r.Region, r.Name = core.KindRDS, base.Profile, base.Region, r.ID
		return r
	}
	want := []core.Resource{
		ec2r(core.Resource{ID: "i-stopped", Name: "i-stopped", State: "stopped", Ports: []core.PortSpec{{Name: "http", Remote: 80}},
			ShellCommand: "sh", Unreachable: "instance stopped"}),
		ec2r(core.Resource{ID: "i-jump", Name: "jump", State: "running", IsBastion: true, Ports: []core.PortSpec{{Name: "http", Remote: 80}},
			ShellCommand: "sh", Unreachable: reasonSSMOffline}),
		ec2r(core.Resource{ID: "i-web", Name: "web", State: "running", SSMOnline: true, ShellCommand: "bash -l",
			Ports: []core.PortSpec{{Name: "http", Remote: 80}, {Name: "api", Remote: 3000}, {Name: "mailpit", Remote: 8025}, {Name: "debug", Remote: 9229}}}),
		rdsr(core.Resource{ID: "aurora", State: "available", Host: "aurora.cluster-example.internal", Port: 5432, Engine: "aurora-postgresql"}),
		rdsr(core.Resource{ID: "paused", State: "stopped", Engine: "mysql", Unreachable: "RDS status stopped"}),
		rdsr(core.Resource{ID: "standalone", State: "available", Host: "standalone.example.internal", Port: 5432, Engine: "postgres"}),
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("discoverRegion mismatch\n got: %+v\nwant: %+v", got, want)
	}
	if len(manualCalls) != 3 {
		t.Errorf("manual called %d times (%v), want 3", len(manualCalls), manualCalls)
	}
}

func TestDiscoverRegionPartialErrors(t *testing.T) {
	denied := func(action string) *PermissionError {
		return &PermissionError{Profile: "example", Region: "ca-central-1", Action: action, What: "x",
			Err: &smithy.GenericAPIError{Code: "AccessDeniedException", Message: "not allowed"}}
	}
	web := []ec2Instance{instance("i-web", "web", "running", nil)}
	aurora := []dbCluster{{ID: "aurora", Status: "available", Endpoint: "aurora.cluster-example.internal", Port: 3306}}
	tests := []struct {
		name        string
		clients     regionClients
		wantIDs     []string
		wantActions []string
	}{
		{"ssm denied hides EC2", regionClients{
			ec2: &fakeEC2{instances: web},
			ssm: &fakeSSM{err: denied("ssm:DescribeInstanceInformation")},
			rds: &fakeRDS{clusters: aurora},
		}, []string{"aurora"}, []string{"ssm:DescribeInstanceInformation"}},
		{"rds instances denied keeps EC2 and clusters", regionClients{
			ec2: &fakeEC2{instances: web},
			ssm: &fakeSSM{infos: []ssmInstanceInfo{{InstanceID: "i-web", PingStatus: "Online"}}},
			rds: &fakeRDS{clusters: aurora, instErr: denied("rds:DescribeDBInstances")},
		}, []string{"i-web", "aurora"}, []string{"rds:DescribeDBInstances"}},
		{"ec2 and rds denied", regionClients{
			ec2: &fakeEC2{err: denied("ec2:DescribeInstances")},
			ssm: &fakeSSM{},
			rds: &fakeRDS{instErr: denied("rds:DescribeDBInstances"), clusterErr: denied("rds:DescribeDBClusters")},
		}, nil, []string{"ec2:DescribeInstances", "rds:DescribeDBInstances", "rds:DescribeDBClusters"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := discoverRegion(context.Background(), tt.clients, discoverOptions{profile: "example", region: "ca-central-1"})
			if err == nil {
				t.Fatal("want error")
			}
			if IsAuthError(err) {
				t.Error("access denied must not be an auth error")
			}
			var actions []string
			for _, pe := range PermissionErrors(err) {
				actions = append(actions, pe.Action)
			}
			if !reflect.DeepEqual(actions, tt.wantActions) {
				t.Errorf("permission errors = %v, want %v", actions, tt.wantActions)
			}
			var ids []string
			for _, r := range got {
				ids = append(ids, r.ID)
			}
			if !reflect.DeepEqual(ids, tt.wantIDs) {
				t.Errorf("resources = %v, want %v", ids, tt.wantIDs)
			}
		})
	}
}
