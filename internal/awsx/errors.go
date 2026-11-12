package awsx

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/aws/aws-sdk-go-v2/credentials/ssocreds"
	"github.com/aws/smithy-go"
)

// IAMDocsURL documents the IAM actions awsport needs.
const IAMDocsURL = "https://github.com/nhl4000/awsport#iam-permissions"

// CustomDocumentsURL documents binding custom SSM session documents to ports.
const CustomDocumentsURL = "https://github.com/nhl4000/awsport#custom-ssm-documents"

// ErrSSOTokenExpired means the cached SSO access token is missing or expired;
// the user has to run `aws sso login`.
var ErrSSOTokenExpired = errors.New("sso token missing or expired")

// ErrCredentials wraps authentication failures (expired or invalid credentials,
// expired SSO session) so callers can offer a re-login.
var ErrCredentials = errors.New("credentials expired or invalid")

// authErrorCodes are AWS API error codes that mean "log in again".
// AccessDeniedException / UnauthorizedOperation are deliberately absent: they
// are authorization (permissions) failures, not an expired login.
var authErrorCodes = map[string]bool{
	"ExpiredToken":                true,
	"ExpiredTokenException":       true,
	"InvalidClientTokenId":        true,
	"UnrecognizedClientException": true,
	"UnauthorizedException":       true, // SSO portal: access token rejected
	"InvalidGrantException":       true, // SSO OIDC: refresh token rejected
	"AuthFailure":                 true, // EC2: credentials could not be validated
	"InvalidAccessKeyId":          true,
}

// authMessages are lowercase message fragments of SDK/CLI auth failures that
// do not surface as API error codes (token cache and refresh errors).
var authMessages = []string{
	"token has expired",
	"token is expired",
	"sso session",
	"error loading sso token",
	"cached sso token",
	"refresh cached sso token failed",
	"the security token included in the request is invalid",
	"the security token included in the request is expired",
}

// IsAuthError reports whether err means credentials are expired or invalid.
func IsAuthError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrCredentials) || errors.Is(err, ErrSSOTokenExpired) {
		return true
	}
	var invalidToken *ssocreds.InvalidTokenError
	if errors.As(err, &invalidToken) {
		return true
	}
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) && authErrorCodes[apiErr.ErrorCode()] {
		return true
	}
	msg := strings.ToLower(err.Error())
	for _, m := range authMessages {
		if strings.Contains(msg, m) {
			return true
		}
	}
	return false
}

// PermissionError reports an AWS authorization failure for one API action.
// It is NOT an auth/credentials error: IsAuthError returns false for it.
type PermissionError struct {
	Profile   string
	Region    string // empty for global calls
	Action    string // IAM action, e.g. "rds:DescribeDBInstances"
	What      string // human purpose, e.g. "list RDS databases"
	Principal string // ARN from the AWS message if present, else ""
	Err       error  // underlying API error
}

func (e *PermissionError) Error() string {
	var b strings.Builder
	if e.Profile != "" {
		b.WriteString(e.Profile + ": ")
	}
	b.WriteString("not allowed to " + e.What)
	if e.Region != "" {
		b.WriteString(" in " + e.Region)
	}
	b.WriteString(" (missing " + e.Action + ")")
	return b.String()
}

func (e *PermissionError) Unwrap() error { return e.Err }

// ssoRolePrefix prefixes the IAM roles IAM Identity Center creates per
// permission set: AWSReservedSSO_<PermissionSet>_<16 hex digits>.
const ssoRolePrefix = "AWSReservedSSO_"

// roleName returns the IAM role name of an assumed-role or role ARN, or "".
func roleName(principal string) string {
	_, resource, ok := strings.Cut(principal, ":assumed-role/")
	if ok {
		name, _, _ := strings.Cut(resource, "/")
		return name
	}
	if _, resource, ok = strings.Cut(principal, ":role/"); ok {
		return resource[strings.LastIndexByte(resource, '/')+1:]
	}
	return ""
}

// PermissionSet returns the IAM Identity Center permission set name from
// Principal ("AWSReservedSSO_Developer_0123abcd" -> "Developer"), the role
// name for other roles, or "" when unknown.
func (e *PermissionError) PermissionSet() string {
	role := roleName(e.Principal)
	if rest, ok := strings.CutPrefix(role, ssoRolePrefix); ok {
		if i := strings.LastIndexByte(rest, '_'); i > 0 {
			return rest[:i]
		}
		return rest
	}
	return role
}

// Hint is one actionable sentence telling the user whom to ask for what.
func (e *PermissionError) Hint() string {
	return hintSentence([]string{e.Action}, e.hintWhere())
}

// hintWhere names the thing the admin must change: the IAM Identity Center
// permission set, else the role, else the profile's role.
func (e *PermissionError) hintWhere() string {
	switch set := e.PermissionSet(); {
	case set != "" && strings.HasPrefix(roleName(e.Principal), ssoRolePrefix):
		return fmt.Sprintf(" in the %q permission set", set)
	case set != "":
		return fmt.Sprintf(" for the %q role", set)
	case e.Profile != "":
		return fmt.Sprintf(" for the role of profile %q", e.Profile)
	}
	return ""
}

func hintSentence(actions []string, where string) string {
	list := actions[0]
	if n := len(actions); n > 1 {
		list = strings.Join(actions[:n-1], ", ") + " and " + actions[n-1]
	}
	return fmt.Sprintf("Ask your AWS admin to allow %s%s (see %s).", list, where, IAMDocsURL)
}

// Hints merges the hints of perms into one sentence per permission set or
// role, listing each missing action once, in first-seen order.
func Hints(perms []*PermissionError) []string {
	var wheres []string
	actions := map[string][]string{}
	for _, p := range perms {
		w := p.hintWhere()
		if _, ok := actions[w]; !ok {
			wheres = append(wheres, w)
		}
		if !slices.Contains(actions[w], p.Action) {
			actions[w] = append(actions[w], p.Action)
		}
	}
	out := make([]string, 0, len(wheres))
	for _, w := range wheres {
		out = append(out, hintSentence(actions[w], w))
	}
	return out
}

// PermissionErrors returns every *PermissionError inside err (walking
// errors.Join trees and wrapping), deduplicated by Action+Region.
func PermissionErrors(err error) []*PermissionError {
	var out []*PermissionError
	seen := map[[2]string]bool{}
	var walk func(error)
	walk = func(err error) {
		switch e := err.(type) {
		case nil:
			return
		case *PermissionError:
			if key := [2]string{e.Action, e.Region}; !seen[key] {
				seen[key] = true
				out = append(out, e)
			}
			return
		case interface{ Unwrap() []error }:
			for _, inner := range e.Unwrap() {
				walk(inner)
			}
		case interface{ Unwrap() error }:
			walk(e.Unwrap())
		}
	}
	walk(err)
	return out
}

// WithoutPermissionErrors returns err with every *PermissionError removed
// (walking errors.Join trees and wrapping), or nil if err held only
// permission errors. Wrapper messages such as "discover p: " are kept, and
// errors.Is/As still reach the remaining errors.
func WithoutPermissionErrors(err error) error {
	out, _ := withoutPermissionErrors(err)
	return out
}

// withoutPermissionErrors reports whether anything was removed, so unchanged
// errors are returned as is (error values need not be comparable).
func withoutPermissionErrors(err error) (error, bool) {
	switch e := err.(type) {
	case nil:
		return nil, false
	case *PermissionError:
		return nil, true
	case interface{ Unwrap() []error }:
		children := e.Unwrap()
		msg := err.Error()
		joined := isJoinMessage(msg, children)
		var kept []error
		changed := false
		for _, c := range children {
			nc, removed := withoutPermissionErrors(c)
			if nc != nil {
				kept = append(kept, nc)
			}
			if removed {
				changed = true
				if !joined {
					msg = strings.Replace(msg, c.Error(), errorString(nc), 1)
				}
			}
		}
		switch {
		case !changed:
			return err, false
		case len(kept) == 0:
			return nil, true
		case joined:
			return errors.Join(kept...), true
		}
		return &rewrappedError{msg: msg, errs: kept}, true
	case interface{ Unwrap() error }:
		inner := e.Unwrap()
		ni, removed := withoutPermissionErrors(inner)
		switch {
		case !removed:
			return err, false
		case ni == nil:
			return nil, true
		}
		return &rewrappedError{msg: strings.Replace(err.Error(), inner.Error(), ni.Error(), 1), errs: []error{ni}}, true
	}
	return err, false
}

// isJoinMessage reports whether msg is the errors.Join message of children.
func isJoinMessage(msg string, children []error) bool {
	parts := make([]string, 0, len(children))
	for _, c := range children {
		if c != nil {
			parts = append(parts, c.Error())
		}
	}
	return msg == strings.Join(parts, "\n")
}

func errorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// rewrappedError keeps a wrapper's message after some wrapped errors were
// removed.
type rewrappedError struct {
	msg  string
	errs []error
}

func (e *rewrappedError) Error() string   { return e.msg }
func (e *rewrappedError) Unwrap() []error { return e.errs }

var (
	deniedActionRe = regexp.MustCompile(`not authorized to perform:?\s+([A-Za-z0-9-]+:[A-Za-z0-9*]+)`)
	deniedPolicyRe = regexp.MustCompile(`allows the ([A-Za-z0-9-]+:[A-Za-z0-9*]+) action`)
	principalRe    = regexp.MustCompile(`User: (arn:[^\s]+)`)
	resourceRe     = regexp.MustCompile(`on resource:?\s+(arn:[^\s]+)`)
)

// deniedMarkers identify access-denied (authorization) failures in AWS error
// text: error codes and the IAM evaluation message.
var deniedMarkers = []string{
	"AccessDenied", // AccessDenied, AccessDeniedException
	"UnauthorizedOperation",
	"not authorized to perform",
}

// ParseAccessDenied extracts the denied IAM action and the principal ARN from
// AWS error text such as "User: arn:aws:sts::123456789012:assumed-role/X/y is
// not authorized to perform: ssm:StartSession on resource: ...". Either may
// be "" when the message does not name it; ok is false if msg is not an
// access-denied message.
func ParseAccessDenied(msg string) (action, principal string, ok bool) {
	for _, m := range deniedMarkers {
		if strings.Contains(msg, m) {
			ok = true
			break
		}
	}
	if !ok {
		return "", "", false
	}
	if m := deniedActionRe.FindStringSubmatch(msg); m != nil {
		action = m[1]
	} else if m := deniedPolicyRe.FindStringSubmatch(msg); m != nil {
		action = m[1]
	}
	if m := principalRe.FindStringSubmatch(msg); m != nil {
		principal = strings.TrimRight(m[1], ".,;")
	}
	return action, principal, true
}

// DeniedResource returns the resource ARN an access-denied message names
// ("... on resource: arn:aws:ssm:...:document/X because ..."), or "".
func DeniedResource(msg string) string {
	if m := resourceRe.FindStringSubmatch(msg); m != nil {
		return strings.TrimRight(m[1], ".,;")
	}
	return ""
}
