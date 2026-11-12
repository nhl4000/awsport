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
		{"mailpit:8025@acme-staging-mailpit-port-forward,9000@acme-port-forward",
			[]core.PortSpec{{Name: "mailpit", Remote: 8025, Document: "acme-staging-mailpit-port-forward"}, {Remote: 9000, Document: "acme-port-forward"}}},
		{"web : 80 @ acme-web-port-forward; app:8080 api:3000@arn:aws:ssm:us-east-1:123456789012:document/acme-api",
			[]core.PortSpec{{Name: "web", Remote: 80, Document: "acme-web-port-forward"}, {Name: "app", Remote: 8080},
				{Name: "api", Remote: 3000, Document: "arn:aws:ssm:us-east-1:123456789012:document/acme-api"}}},
		{"bad:80@,x:81@a b:82@bad!doc,ok:83", []core.PortSpec{{Name: "ok", Remote: 83}}},
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

func TestMergePortsKeepsDocument(t *testing.T) {
	presets := []core.PortSpec{{Name: "mailpit", Remote: 8025}, {Name: "app", Remote: 8080}}
	docs := []core.PortSpec{{Remote: 8025, Document: "acme-mailpit-port-forward"}, {Remote: 9000, Document: "acme-port-forward"}}
	tag := []core.PortSpec{{Name: "mail", Remote: 8025}, {Name: "app", Remote: 8080, Document: "acme-app-port-forward"}, {Remote: 9000, Document: "acme-tag-port-forward"}}
	manual := []core.PortSpec{{Name: "m", Remote: 8025}}
	got := mergePorts(presets, docs, tag, manual)
	want := []core.PortSpec{
		{Name: "m", Remote: 8025, Document: "acme-mailpit-port-forward"},
		{Name: "app", Remote: 8080, Document: "acme-app-port-forward"},
		{Remote: 9000, Document: "acme-tag-port-forward"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("mergePorts = %v, want %v", got, want)
	}
	// A preset listed after a document never erases it.
	if got := mergePorts(docs[:1], presets[:1]); !reflect.DeepEqual(got, []core.PortSpec{{Name: "mailpit", Remote: 8025, Document: "acme-mailpit-port-forward"}}) {
		t.Errorf("preset erased document: %v", got)
	}
}

func TestNameMatches(t *testing.T) {
	tests := []struct {
		pattern, name string
		want          bool
	}{
		{"acme-web", "acme-web", true},
		{"acme-web", "acme-web-2", false},
		{"acme-*", "acme-web", true},
		{"acme-*", "acme-", true},
		{"*-bastion", "acme-bastion", true},
		{"acme-*-web", "acme-staging-web", true},
		{"acme-*-web", "acme-web", false},
		{"*", "anything", true},
		{"*", "", false},
		{"acme-*", "other-web", false},
	}
	for _, tt := range tests {
		if got := nameMatches(tt.pattern, tt.name); got != tt.want {
			t.Errorf("nameMatches(%q, %q) = %v, want %v", tt.pattern, tt.name, got, tt.want)
		}
	}
}

func TestDocumentPorts(t *testing.T) {
	docs := []core.PortDocument{
		{Profile: "acme-prod", Name: "acme-web", Remote: 8025, Document: "scoped-exact"},
		{Name: "acme-web", Remote: 8025, Document: "exact"},
		{Profile: "acme-prod", Name: "acme-*", Remote: 8025, Document: "scoped-wild"},
		{Name: "acme-*", Remote: 8025, Document: "wild"},
		{Name: "acme-*", Remote: 9000, Document: "other-port"},
		{Name: "acme-*", Remote: 9001},
	}
	last := func(ps []core.PortSpec, remote int) string {
		doc := ""
		for _, p := range mergePorts(ps) {
			if p.Remote == remote {
				doc = p.Document
			}
		}
		return doc
	}
	tests := []struct {
		profile, name, want8025, want9000 string
	}{
		{"acme-prod", "acme-web", "scoped-exact", "other-port"},
		{"acme-dev", "acme-web", "exact", "other-port"},
		{"acme-prod", "acme-api", "scoped-wild", "other-port"},
		{"acme-dev", "acme-api", "wild", "other-port"},
		{"acme-dev", "other", "", ""},
		{"acme-dev", "", "", ""},
	}
	for _, tt := range tests {
		got := documentPorts(docs, tt.profile, tt.name)
		if d := last(got, 8025); d != tt.want8025 {
			t.Errorf("%s/%s: 8025 document = %q, want %q", tt.profile, tt.name, d, tt.want8025)
		}
		if d := last(got, 9000); d != tt.want9000 {
			t.Errorf("%s/%s: 9000 document = %q, want %q", tt.profile, tt.name, d, tt.want9000)
		}
		for _, p := range got {
			if p.Remote == 9001 {
				t.Errorf("%s/%s: entry without document applied", tt.profile, tt.name)
			}
		}
	}
}

func TestPickBastion(t *testing.T) {
	ec2 := func(id, name, state string, online, tagged bool) core.Resource {
		return core.Resource{Kind: core.KindEC2, Profile: "example", Region: "ca-central-1", ID: id, Name: name,
			State: state, SSMOnline: online, IsBastion: tagged}
	}
	app := ec2("i-app", "app", "running", true, false)
	named := ec2("i-named", "acme-bastion", "running", true, false)
	wild := ec2("i-wild", "acme-jump-1", "running", true, false)
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
		{"earlier name wins", []core.Resource{wild, named}, "", "i-named", nil},
		{"wildcard name", []core.Resource{app, wild}, "i-app", "i-wild", nil},
		{"saved used", []core.Resource{app, taggedStopped, db}, "i-app", "i-app", nil},
		{"saved must be connectable", []core.Resource{app, taggedOffline}, "i-tagged-off", "", []string{"i-app"}},
		{"other region ignored", []core.Resource{otherRegion, app}, "", "", []string{"i-app"}},
		{"no candidates", []core.Resource{db, taggedStopped}, "", "", []string{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := PickBastion(tt.resources, "example", "ca-central-1", tt.saved, []string{"acme-bastion", "acme-jump-*"})
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

func TestPickBastionNoNamesConfigured(t *testing.T) {
	named := core.Resource{Kind: core.KindEC2, Profile: "example", Region: "ca-central-1", ID: "i-named",
		Name: "acme-bastion", State: "running", SSMOnline: true}
	_, err := PickBastion([]core.Resource{named}, "example", "ca-central-1", "", nil)
	var need *NeedBastionError
	if !errors.As(err, &need) {
		t.Fatalf("err = %v, want *NeedBastionError without bastion_names", err)
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
		profile: "example",
		region:  "ca-central-1",
		presets: []core.PortSpec{{Name: "http", Remote: 80}},
		documents: []core.PortDocument{
			{Name: "web", Remote: 8025, Document: "acme-mailpit-port-forward"},
			{Name: "w*", Remote: 8080, Document: "acme-app-port-forward"},
			{Profile: "other", Name: "web", Remote: 80, Document: "acme-other-port-forward"},
			{Name: "i-stopped", Remote: 81, Document: "acme-id-port-forward"}, // matches Name tags only
		},
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
			Ports: []core.PortSpec{{Name: "http", Remote: 80}, {Name: "api", Remote: 3000}, {Name: "mailpit", Remote: 8025, Document: "acme-mailpit-port-forward"},
				{Remote: 8080, Document: "acme-app-port-forward"}, {Name: "debug", Remote: 9229}}}),
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
	webAndOld := []ec2Instance{instance("i-web", "web", "running", nil), instance("i-old", "old", "stopped", nil)}
	aurora := []dbCluster{{ID: "aurora", Status: "available", Endpoint: "aurora.cluster-example.internal", Port: 3306}}
	tests := []struct {
		name        string
		clients     regionClients
		wantIDs     []string
		wantActions []string
		reachable   map[string]bool // optional: resource ID -> connectable
	}{
		{"ssm denied keeps running EC2 connectable", regionClients{
			ec2: &fakeEC2{instances: webAndOld},
			ssm: &fakeSSM{err: denied("ssm:DescribeInstanceInformation")},
			rds: &fakeRDS{clusters: aurora},
		}, []string{"i-old", "i-web", "aurora"}, []string{"ssm:DescribeInstanceInformation"},
			map[string]bool{"i-web": true, "i-old": false}},
		{"rds instances denied keeps EC2 and clusters", regionClients{
			ec2: &fakeEC2{instances: web},
			ssm: &fakeSSM{infos: []ssmInstanceInfo{{InstanceID: "i-web", PingStatus: "Online"}}},
			rds: &fakeRDS{clusters: aurora, instErr: denied("rds:DescribeDBInstances")},
		}, []string{"i-web", "aurora"}, []string{"rds:DescribeDBInstances"}, nil},
		{"ec2 and rds denied", regionClients{
			ec2: &fakeEC2{err: denied("ec2:DescribeInstances")},
			ssm: &fakeSSM{},
			rds: &fakeRDS{instErr: denied("rds:DescribeDBInstances"), clusterErr: denied("rds:DescribeDBClusters")},
		}, nil, []string{"ec2:DescribeInstances", "rds:DescribeDBInstances", "rds:DescribeDBClusters"}, nil},
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
				if want, ok := tt.reachable[r.ID]; ok && (r.Unreachable == "") != want {
					t.Errorf("%s connectable = %v (reason %q), want %v", r.ID, r.Unreachable == "", r.Unreachable, want)
				}
			}
			if !reflect.DeepEqual(ids, tt.wantIDs) {
				t.Errorf("resources = %v, want %v", ids, tt.wantIDs)
			}
		})
	}
}
