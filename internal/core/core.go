// Package core holds the domain types shared by every awsport package.
// It has no dependencies on other awsport packages.
package core

import (
	"fmt"
	"strings"
)

// TagPrefix is the EC2 tag namespace awsport reads (e.g. "awsport:ports").
const TagPrefix = "awsport:"

const (
	TagPorts   = TagPrefix + "ports"   // "mailpit:8025,app:8080"
	TagBastion = TagPrefix + "bastion" // "true"
	TagShell   = TagPrefix + "shell"   // shell command, e.g. "sudo su - ubuntu"
)

// DefaultShellCommand runs on interactive session start unless overridden.
const DefaultShellCommand = "sudo su - ubuntu"

// DefaultBastionName is the Name tag fallback for bastion discovery.
const DefaultBastionName = "management-bastion"

// AuthKind classifies how a profile obtains credentials.
type AuthKind string

const (
	AuthSSO        AuthKind = "sso"
	AuthAssumeRole AuthKind = "assume-role"
	AuthStatic     AuthKind = "static"
	AuthUnknown    AuthKind = "unknown"
)

// Profile is one entry from ~/.aws/config.
type Profile struct {
	Name       string
	Region     string // may be empty; callers fall back to configured regions
	Kind       AuthKind
	AccountID  string // from sso_account_id or role_arn; may be empty
	RoleName   string // sso_role_name or role name from role_arn
	SSOSession string // sso_session name for SSO profiles
	Managed    bool   // inside awsport's managed block
}

// ResourceKind distinguishes connectable resource types.
type ResourceKind string

const (
	KindEC2 ResourceKind = "ec2"
	KindRDS ResourceKind = "rds"
)

// PortSpec is a named remote port.
type PortSpec struct {
	Name   string `json:"name" yaml:"name"`
	Remote int    `json:"remote" yaml:"remote"`
}

func (p PortSpec) String() string {
	if p.Name == "" {
		return fmt.Sprintf("%d", p.Remote)
	}
	return fmt.Sprintf("%s:%d", p.Name, p.Remote)
}

// Resource is a discovered EC2 instance or RDS instance/cluster.
type Resource struct {
	Kind    ResourceKind
	Profile string
	Region  string
	ID      string // EC2 instance ID, or RDS DB instance/cluster identifier
	Name    string // Name tag (EC2) or identifier (RDS)
	State   string // EC2 instance state or RDS status

	// EC2 only.
	SSMOnline    bool
	IsBastion    bool       // tagged awsport:bastion=true
	Ports        []PortSpec // merged presets + tag + manual ports, deduped by Remote
	ShellCommand string     // awsport:shell tag or DefaultShellCommand

	// RDS only.
	Host   string
	Port   int
	Engine string

	// Unreachable is a human reason this resource can't be connected
	// ("stopped", "SSM agent not online", ...). Empty means connectable.
	Unreachable string
}

// Ref uniquely identifies a resource across profiles and regions.
func (r Resource) Ref() string {
	return strings.Join([]string{r.Profile, r.Region, r.ID}, "/")
}

// TunnelKey is the stable key for a (resource, remote port) pair. It is the
// key used for saved ports and for tunnel identity in the manager.
func (r Resource) TunnelKey(remote int) string {
	return fmt.Sprintf("%s/%d", r.Ref(), remote)
}

// TunnelSpec fully describes one SSM port-forwarding session.
type TunnelSpec struct {
	Key        string // Resource.TunnelKey(RemotePort)
	Label      string // human label, e.g. "web-app mailpit"
	Profile    string
	Region     string
	Target     string // instance ID the SSM session attaches to (EC2 or bastion)
	RemoteHost string // set => AWS-StartPortForwardingSessionToRemoteHost
	RemotePort int
	LocalPort  int
	Temporary  bool // LocalPort is a session-only fallback; saved port was busy
}

// SSMDocument returns the SSM document to use for this spec.
func (t TunnelSpec) SSMDocument() string {
	if t.RemoteHost != "" {
		return "AWS-StartPortForwardingSessionToRemoteHost"
	}
	return "AWS-StartPortForwardingSession"
}

// GroupMember references one tunnel inside a saved group.
type GroupMember struct {
	Profile string `json:"profile" yaml:"profile"`
	Region  string `json:"region,omitempty" yaml:"region,omitempty"` // empty = discover across regions
	// Name matches EC2 Name tag or RDS identifier (stable across instance replacement).
	Name   string       `json:"name" yaml:"name"`
	Kind   ResourceKind `json:"kind" yaml:"kind"`
	Remote int          `json:"remote" yaml:"remote"`
}

// Group is a named bundle of tunnels started together.
type Group struct {
	Name    string        `json:"name" yaml:"name"`
	Members []GroupMember `json:"members" yaml:"members"`
}
