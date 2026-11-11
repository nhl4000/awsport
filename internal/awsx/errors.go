package awsx

import (
	"errors"
	"strings"

	"github.com/aws/aws-sdk-go-v2/credentials/ssocreds"
	"github.com/aws/smithy-go"
)

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
