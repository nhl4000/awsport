package awsx

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sts"

	"github.com/nhl4000/awsport/internal/core"
)

// Unreachable reasons reported on resources.
const (
	reasonSSMOffline = "SSM agent not online (missing agent or instance profile)"
	reasonNoEndpoint = "RDS endpoint not available"
)

// stsFallbackRegion is used for the caller-identity check when a profile has
// no region configured.
const stsFallbackRegion = "us-east-1"

// Discoverer lists AWS resources per profile. It caches one aws.Config per
// profile (shared credential cache) and derives per-region copies from it.
type Discoverer struct {
	mu   sync.Mutex
	cfgs map[string]aws.Config
	load func(ctx context.Context, profile string) (aws.Config, error)
}

// NewDiscoverer returns a Discoverer that loads profiles from the shared AWS
// config/credentials files.
func NewDiscoverer() *Discoverer {
	return &Discoverer{
		cfgs: map[string]aws.Config{},
		load: func(ctx context.Context, profile string) (aws.Config, error) {
			return awsconfig.LoadDefaultConfig(ctx, awsconfig.WithSharedConfigProfile(profile))
		},
	}
}

// regionConfig returns the cached config of profile with Region set.
func (d *Discoverer) regionConfig(ctx context.Context, profile, region string) (aws.Config, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	base, ok := d.cfgs[profile]
	if !ok {
		var err error
		base, err = d.load(ctx, profile)
		if err != nil {
			return aws.Config{}, fmt.Errorf("load AWS config for profile %s: %w", profile, err)
		}
		d.cfgs[profile] = base
	}
	cfg := base.Copy()
	if region != "" {
		cfg.Region = region
	}
	return cfg, nil
}

// forget drops the cached config of profile so the next call re-reads
// credentials (after a re-login).
func (d *Discoverer) forget(profile string) {
	d.mu.Lock()
	delete(d.cfgs, profile)
	d.mu.Unlock()
}

// authWrap wraps auth failures with ErrCredentials and drops the cached config.
func (d *Discoverer) authWrap(profile string, err error) error {
	if err == nil || !IsAuthError(err) {
		return err
	}
	d.forget(profile)
	if errors.Is(err, ErrCredentials) {
		return err
	}
	return fmt.Errorf("%w: profile %s: %w", ErrCredentials, profile, err)
}

// Regions returns the regions to scan for a profile: p.Region followed by
// extra, deduplicated with order preserved and empty entries dropped.
func Regions(p core.Profile, extra []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, r := range append([]string{p.Region}, extra...) {
		r = strings.TrimSpace(r)
		if r == "" || seen[r] {
			continue
		}
		seen[r] = true
		out = append(out, r)
	}
	return out
}

// CheckCredentials verifies the profile's credentials with
// sts:GetCallerIdentity. Auth failures are wrapped with ErrCredentials.
func (d *Discoverer) CheckCredentials(ctx context.Context, p core.Profile) error {
	cfg, err := d.regionConfig(ctx, p.Name, p.Region)
	if err != nil {
		return d.authWrap(p.Name, err)
	}
	if cfg.Region == "" {
		cfg.Region = stsFallbackRegion
	}
	if _, err := sts.NewFromConfig(cfg).GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{}); err != nil {
		if IsAuthError(err) {
			return d.authWrap(p.Name, err)
		}
		return fmt.Errorf("check credentials for profile %s: %w", p.Name, err)
	}
	return nil
}

// discoverOptions carries the per-call inputs of resource conversion.
type discoverOptions struct {
	profile      string
	region       string
	presets      []core.PortSpec
	documents    []core.PortDocument
	shellDefault string
	manual       func(profile, region, name string) []core.PortSpec
}

// ec2API, ssmAPI and rdsAPI are the discovery calls (all pages) of one
// region; tests use fakes.
type ec2API interface {
	describeInstances(ctx context.Context) ([]ec2Instance, error)
}

type ssmAPI interface {
	describeOnlineInstances(ctx context.Context) ([]ssmInstanceInfo, error)
}

type rdsAPI interface {
	describeDBInstances(ctx context.Context) ([]dbInstance, error)
	describeDBClusters(ctx context.Context) ([]dbCluster, error)
}

// regionClients are the per-region AWS APIs discovery needs.
type regionClients struct {
	ec2 ec2API
	ssm ssmAPI
	rds rdsAPI
}

func newRegionClients(cfg aws.Config, profile string) regionClients {
	return regionClients{
		ec2: ec2Client{newAPIClient(cfg, profile, "ec2")},
		ssm: ssmClient{newAPIClient(cfg, profile, "ssm")},
		rds: rdsClient{newAPIClient(cfg, profile, "rds")},
	}
}

// Discover lists EC2 instances (excluding terminated, with SSM ping status,
// Name tag and awsport:* tags) and RDS DB instances + clusters (writer
// endpoint) in parallel across regions. EC2 ports merge presets < config
// port documents < awsport:ports tag < manual ports. Per-region errors are
// aggregated and returned alongside whatever was found: access-denied calls
// as *PermissionError, auth errors wrapped with ErrCredentials.
func (d *Discoverer) Discover(ctx context.Context, p core.Profile, regions []string, presets []core.PortSpec,
	documents []core.PortDocument, shellDefault string, manual func(profile, region, name string) []core.PortSpec) ([]core.Resource, error) {
	if len(regions) == 0 {
		return nil, fmt.Errorf("no regions to scan for profile %s: set a region on the profile or add regions to the awsport config", p.Name)
	}
	results := make([][]core.Resource, len(regions))
	errs := make([]error, len(regions))
	var wg sync.WaitGroup
	for i, region := range regions {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cfg, err := d.regionConfig(ctx, p.Name, region)
			if err != nil {
				errs[i] = err
				return
			}
			results[i], errs[i] = discoverRegion(ctx, newRegionClients(cfg, p.Name), discoverOptions{
				profile:      p.Name,
				region:       region,
				presets:      presets,
				documents:    documents,
				shellDefault: shellDefault,
				manual:       manual,
			})
		}()
	}
	wg.Wait()

	var out []core.Resource
	for _, rs := range results {
		out = append(out, rs...)
	}
	return out, d.authWrap(p.Name, errors.Join(errs...))
}

// discoverRegion lists EC2 and RDS resources of one region concurrently.
func discoverRegion(ctx context.Context, c regionClients, o discoverOptions) ([]core.Resource, error) {
	var (
		wg             sync.WaitGroup
		ec2Res, rdsRes []core.Resource
		ec2Err, rdsErr error
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		ec2Res, ec2Err = discoverEC2(ctx, c, o)
	}()
	go func() {
		defer wg.Done()
		rdsRes, rdsErr = discoverRDS(ctx, c.rds, o)
	}()
	wg.Wait()

	out := append(ec2Res, rdsRes...)
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Kind != b.Kind {
			return a.Kind == core.KindEC2
		}
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		return a.ID < b.ID
	})
	return out, errors.Join(ec2Err, rdsErr)
}

// discoverEC2 lists live EC2 instances with their SSM status. If the SSM
// status call fails for another reason no instance is reported (all would
// look unreachable). If it is denied by IAM, running instances are listed as
// connectable — opening a tunnel verifies the agent — and the
// *PermissionError is returned alongside so the missing action is shown.
func discoverEC2(ctx context.Context, c regionClients, o discoverOptions) ([]core.Resource, error) {
	instances, err := c.ec2.describeInstances(ctx)
	if err != nil {
		return nil, err
	}
	instances = slices.DeleteFunc(instances, func(inst ec2Instance) bool { return inst.State == "terminated" })
	if len(instances) == 0 {
		return nil, nil
	}

	online, ssmErr := ssmOnline(ctx, c.ssm)
	var denied *PermissionError
	if ssmErr != nil && !errors.As(ssmErr, &denied) {
		return nil, ssmErr
	}
	out := make([]core.Resource, 0, len(instances))
	for _, inst := range instances {
		if denied != nil {
			online = map[string]bool{inst.ID: inst.State == "running"}
		}
		out = append(out, ec2Resource(inst, online, o))
	}
	return out, ssmErr
}

// ssmOnline returns the IDs of managed instances whose SSM ping status is Online.
func ssmOnline(ctx context.Context, api ssmAPI) (map[string]bool, error) {
	infos, err := api.describeOnlineInstances(ctx)
	if err != nil {
		return nil, err
	}
	online := map[string]bool{}
	for _, info := range infos {
		if info.PingStatus == ssmPingOnline {
			online[info.InstanceID] = true
		}
	}
	return online, nil
}

func ec2Resource(inst ec2Instance, online map[string]bool, o discoverOptions) core.Resource {
	tags := map[string]string{}
	for _, t := range inst.Tags {
		tags[t.Key] = t.Value
	}
	tagName := strings.TrimSpace(tags["Name"])
	name := tagName
	if name == "" {
		name = inst.ID
	}
	shell := strings.TrimSpace(tags[core.TagShell])
	if shell == "" {
		shell = o.shellDefault
	}
	if shell == "" {
		shell = core.DefaultShellCommand
	}
	var manual []core.PortSpec
	if o.manual != nil {
		manual = o.manual(o.profile, o.region, name)
	}

	r := core.Resource{
		Kind:         core.KindEC2,
		Profile:      o.profile,
		Region:       o.region,
		ID:           inst.ID,
		Name:         name,
		State:        inst.State,
		SSMOnline:    online[inst.ID],
		IsBastion:    strings.EqualFold(strings.TrimSpace(tags[core.TagBastion]), "true"),
		Ports:        mergePorts(o.presets, documentPorts(o.documents, o.profile, tagName), parsePortsTag(tags[core.TagPorts]), manual),
		ShellCommand: shell,
	}
	switch {
	case inst.State != "running":
		r.Unreachable = "instance " + inst.State
	case !r.SSMOnline:
		r.Unreachable = reasonSSMOffline
	}
	return r
}

// discoverRDS lists standalone DB instances and DB clusters (writer
// endpoint). Both calls run even if one fails, so a single missing permission
// still shows what the other returns.
func discoverRDS(ctx context.Context, api rdsAPI, o discoverOptions) ([]core.Resource, error) {
	var out []core.Resource
	instances, instErr := api.describeDBInstances(ctx)
	for _, db := range instances {
		if db.ClusterID != "" {
			continue // reached through the cluster's writer endpoint
		}
		out = append(out, rdsResource(o, db.ID, db.Status, db.Address, db.Port, db.Engine))
	}
	clusters, clusterErr := api.describeDBClusters(ctx)
	for _, cl := range clusters {
		out = append(out, rdsResource(o, cl.ID, cl.Status, cl.Endpoint, cl.Port, cl.Engine))
	}
	return out, errors.Join(instErr, clusterErr)
}

func rdsResource(o discoverOptions, id, status, host string, port int, engine string) core.Resource {
	r := core.Resource{
		Kind:    core.KindRDS,
		Profile: o.profile,
		Region:  o.region,
		ID:      id,
		Name:    id,
		State:   status,
		Host:    host,
		Port:    port,
		Engine:  engine,
	}
	switch {
	case status != "available":
		r.Unreachable = "RDS status " + status
	case host == "" || port == 0:
		r.Unreachable = reasonNoEndpoint
	}
	return r
}
