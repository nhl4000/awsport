package awsx

import (
	"errors"
	"fmt"
	"testing"

	"github.com/aws/aws-sdk-go-v2/credentials/ssocreds"
	"github.com/aws/smithy-go"
)

func TestIsAuthError(t *testing.T) {
	api := func(code string) error { return &smithy.GenericAPIError{Code: code, Message: "request failed"} }
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"ExpiredToken", api("ExpiredToken"), true},
		{"ExpiredTokenException", api("ExpiredTokenException"), true},
		{"InvalidClientTokenId", api("InvalidClientTokenId"), true},
		{"UnrecognizedClientException", api("UnrecognizedClientException"), true},
		{"UnauthorizedException", api("UnauthorizedException"), true},
		{"wrapped code", fmt.Errorf("region x: ec2: %w", api("ExpiredToken")), true},
		{"joined code", errors.Join(errors.New("other"), api("InvalidClientTokenId")), true},
		{"AccessDeniedException", api("AccessDeniedException"), false},
		{"UnauthorizedOperation", api("UnauthorizedOperation"), false},
		{"throttling", api("ThrottlingException"), false},
		{"sso invalid token", &ssocreds.InvalidTokenError{Err: errors.New("x")}, true},
		{"token expired message", errors.New("operation error STS: The SSO session token has expired"), true},
		{"sso session message", errors.New("The SSO session associated with this profile has expired or is otherwise invalid"), true},
		{"cached token message", errors.New("failed to refresh cached credentials, failed to read cached SSO token file"), true},
		{"sentinel credentials", fmt.Errorf("x: %w", ErrCredentials), true},
		{"sentinel sso", fmt.Errorf("x: %w", ErrSSOTokenExpired), true},
		{"plain", errors.New("connection refused"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsAuthError(tt.err); got != tt.want {
				t.Errorf("IsAuthError(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestAuthWrap(t *testing.T) {
	d := NewDiscoverer()
	d.cfgs["example"] = d.cfgs["other"]
	err := d.authWrap("example", &smithy.GenericAPIError{Code: "ExpiredToken"})
	if !errors.Is(err, ErrCredentials) {
		t.Errorf("err = %v, want ErrCredentials", err)
	}
	if _, ok := d.cfgs["example"]; ok {
		t.Error("cached config not dropped after auth error")
	}
	plain := errors.New("boom")
	if got := d.authWrap("example", plain); got != plain {
		t.Errorf("non-auth error changed: %v", got)
	}
}
