package awsx

import (
	"context"
	"errors"
	"reflect"
	"strconv"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/rds"
	rdstypes "github.com/aws/aws-sdk-go-v2/service/rds/types"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"
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

// Fakes serve pre-built pages, keyed by NextToken = page index.

type fakeEC2 struct {
	pages []*ec2.DescribeInstancesOutput
}

func (f *fakeEC2) DescribeInstances(_ context.Context, in *ec2.DescribeInstancesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	i := pageIndex(in.NextToken)
	out := *f.pages[i]
	out.NextToken = nextToken(i, len(f.pages))
	return &out, nil
}

type fakeSSM struct {
	pages []*ssm.DescribeInstanceInformationOutput
	err   error
}

func (f *fakeSSM) DescribeInstanceInformation(_ context.Context, in *ssm.DescribeInstanceInformationInput, _ ...func(*ssm.Options)) (*ssm.DescribeInstanceInformationOutput, error) {
	if f.err != nil {
		return nil, f.err
	}
	i := pageIndex(in.NextToken)
	out := *f.pages[i]
	out.NextToken = nextToken(i, len(f.pages))
	return &out, nil
}

type fakeRDS struct {
	instances []rdstypes.DBInstance
	clusters  []rdstypes.DBCluster
	err       error
}

func (f *fakeRDS) DescribeDBInstances(context.Context, *rds.DescribeDBInstancesInput, ...func(*rds.Options)) (*rds.DescribeDBInstancesOutput, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &rds.DescribeDBInstancesOutput{DBInstances: f.instances}, nil
}

func (f *fakeRDS) DescribeDBClusters(context.Context, *rds.DescribeDBClustersInput, ...func(*rds.Options)) (*rds.DescribeDBClustersOutput, error) {
	return &rds.DescribeDBClustersOutput{DBClusters: f.clusters}, nil
}

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

func instance(id, name string, state ec2types.InstanceStateName, tags map[string]string) ec2types.Instance {
	inst := ec2types.Instance{InstanceId: aws.String(id), State: &ec2types.InstanceState{Name: state}}
	if name != "" {
		inst.Tags = append(inst.Tags, ec2types.Tag{Key: aws.String("Name"), Value: aws.String(name)})
	}
	for k, v := range tags {
		inst.Tags = append(inst.Tags, ec2types.Tag{Key: aws.String(k), Value: aws.String(v)})
	}
	return inst
}

func TestDiscoverRegion(t *testing.T) {
	clients := regionClients{
		ec2: &fakeEC2{pages: []*ec2.DescribeInstancesOutput{
			{Reservations: []ec2types.Reservation{{Instances: []ec2types.Instance{
				instance("i-web", "web", ec2types.InstanceStateNameRunning, map[string]string{
					core.TagPorts: "mailpit:8025, api:3000",
					core.TagShell: "bash -l",
				}),
				instance("i-gone", "gone", ec2types.InstanceStateNameTerminated, nil),
			}}}},
			{Reservations: []ec2types.Reservation{{Instances: []ec2types.Instance{
				instance("i-jump", "jump", ec2types.InstanceStateNameRunning, map[string]string{core.TagBastion: "TRUE"}),
				instance("i-stopped", "", ec2types.InstanceStateNameStopped, nil),
			}}}},
		}},
		ssm: &fakeSSM{pages: []*ssm.DescribeInstanceInformationOutput{
			{InstanceInformationList: []ssmtypes.InstanceInformation{{InstanceId: aws.String("i-web"), PingStatus: ssmtypes.PingStatusOnline}}},
			{InstanceInformationList: []ssmtypes.InstanceInformation{{InstanceId: aws.String("i-stopped"), PingStatus: ssmtypes.PingStatusConnectionLost}}},
		}},
		rds: &fakeRDS{
			instances: []rdstypes.DBInstance{
				{DBInstanceIdentifier: aws.String("standalone"), DBInstanceStatus: aws.String("available"), Engine: aws.String("postgres"),
					Endpoint: &rdstypes.Endpoint{Address: aws.String("standalone.example.internal"), Port: aws.Int32(5432)}},
				{DBInstanceIdentifier: aws.String("aurora-1"), DBClusterIdentifier: aws.String("aurora"), DBInstanceStatus: aws.String("available")},
				{DBInstanceIdentifier: aws.String("paused"), DBInstanceStatus: aws.String("stopped"), Engine: aws.String("mysql")},
			},
			clusters: []rdstypes.DBCluster{
				{DBClusterIdentifier: aws.String("aurora"), Status: aws.String("available"), Engine: aws.String("aurora-postgresql"),
					Endpoint: aws.String("aurora.cluster-example.internal"), Port: aws.Int32(5432)},
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
	denied := &smithy.GenericAPIError{Code: "AccessDeniedException", Message: "not allowed"}
	clients := regionClients{
		ec2: &fakeEC2{pages: []*ec2.DescribeInstancesOutput{{Reservations: []ec2types.Reservation{{Instances: []ec2types.Instance{
			instance("i-web", "web", ec2types.InstanceStateNameRunning, nil),
		}}}}}},
		ssm: &fakeSSM{err: denied},
		rds: &fakeRDS{clusters: []rdstypes.DBCluster{{DBClusterIdentifier: aws.String("aurora"), Status: aws.String("available"),
			Endpoint: aws.String("aurora.cluster-example.internal"), Port: aws.Int32(3306)}}},
	}
	got, err := discoverRegion(context.Background(), clients, discoverOptions{profile: "example", region: "ca-central-1"})
	if err == nil || !errors.Is(err, denied) {
		t.Fatalf("err = %v, want wrapping AccessDenied", err)
	}
	if IsAuthError(err) {
		t.Error("AccessDenied must not be an auth error")
	}
	if len(got) != 1 || got[0].ID != "aurora" {
		t.Errorf("got %+v, want only the RDS cluster", got)
	}
}
