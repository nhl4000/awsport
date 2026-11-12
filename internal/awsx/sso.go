package awsx

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	ssoapi "github.com/aws/aws-sdk-go-v2/service/sso"

	"github.com/nhl4000/awsport/internal/config"
)

// tokenSkew is the minimum remaining validity for a cached token to be used.
const tokenSkew = time.Minute

// ssoRoleWorkers bounds concurrent ListAccountRoles calls (the SSO portal
// API throttles aggressively).
const ssoRoleWorkers = 4

// ssoAPI is the subset of the SSO portal client used by ListSSOAccess.
type ssoAPI interface {
	ssoapi.ListAccountsAPIClient
	ssoapi.ListAccountRolesAPIClient
}

// ListSSOAccess reads the cached SSO access token (~/.aws/sso/cache/*.json
// whose startUrl matches) and lists every account and role via the SSO API,
// sorted by profile name. Returns an error wrapping ErrSSOTokenExpired if the
// token is missing, expired or rejected.
func ListSSOAccess(ctx context.Context, sso config.SSO) ([]SSOProfile, error) {
	if strings.TrimSpace(sso.StartURL) == "" || strings.TrimSpace(sso.Region) == "" {
		return nil, errors.New("list SSO access: SSO start URL and region are required")
	}
	token, _, err := cachedSSOToken(ssoCacheDir(), sso.StartURL, time.Now())
	if err != nil {
		return nil, err
	}
	region := strings.TrimSpace(sso.Region)
	// Load shared config so AWS_ENDPOINT_URL, AWS_CA_BUNDLE, retry and proxy
	// settings apply. The SSO portal API is bearer-token authenticated, so no
	// profile credentials are needed here.
	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(region))
	if err != nil {
		return nil, fmt.Errorf("list SSO access: load AWS config: %w", err)
	}
	return listSSOAccess(ctx, ssoapi.NewFromConfig(cfg), token, region)
}

// SSOCacheDir is the AWS CLI's SSO token cache (~/.aws/sso/cache).
func SSOCacheDir() string {
	return ssoCacheDir()
}

func ssoCacheDir() string {
	return filepath.Join(homeDir(), ".aws", "sso", "cache")
}

type cachedToken struct {
	StartURL    string `json:"startUrl"`
	AccessToken string `json:"accessToken"`
	ExpiresAt   string `json:"expiresAt"`
}

// SSOTokenExpiry returns the expiry of the longest-lived cached access token
// in dir whose startUrl matches startURL, even when it has passed. The error
// wraps ErrSSOTokenExpired when there is no such token or it is not valid for
// at least another minute (the margin ListSSOAccess requires); the expiry is
// zero when no token was found.
func SSOTokenExpiry(dir, startURL string, now time.Time) (time.Time, error) {
	_, exp, err := cachedSSOToken(dir, startURL, now)
	return exp, err
}

// cachedSSOToken returns the longest-lived access token in dir whose startUrl
// matches startURL and its expiry. The error wraps ErrSSOTokenExpired when
// there is none or it expires within tokenSkew of now.
func cachedSSOToken(dir, startURL string, now time.Time) (string, time.Time, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return "", time.Time{}, fmt.Errorf("%w: no SSO token cache at %s", ErrSSOTokenExpired, dir)
	}
	if err != nil {
		return "", time.Time{}, fmt.Errorf("read SSO token cache: %w", err)
	}
	want := normalizeStartURL(startURL)
	var best string
	var bestExp time.Time
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		var t cachedToken
		if json.Unmarshal(data, &t) != nil || t.AccessToken == "" || normalizeStartURL(t.StartURL) != want {
			continue
		}
		exp, ok := parseExpiry(t.ExpiresAt)
		if !ok {
			continue
		}
		if best == "" || exp.After(bestExp) {
			best, bestExp = t.AccessToken, exp
		}
	}
	if best == "" {
		return "", time.Time{}, fmt.Errorf("%w for %s", ErrSSOTokenExpired, startURL)
	}
	if !bestExp.After(now.Add(tokenSkew)) {
		return "", bestExp, fmt.Errorf("%w for %s (valid until %s)", ErrSSOTokenExpired, startURL, bestExp.Format(time.RFC3339))
	}
	return best, bestExp, nil
}

func normalizeStartURL(u string) string {
	return strings.TrimRight(strings.TrimSpace(u), "/#")
}

// parseExpiry accepts RFC 3339 (AWS CLI v2) and the legacy "...UTC" form.
func parseExpiry(s string) (time.Time, bool) {
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05UTC", "2006-01-02T15:04:05Z0700"} {
		if t, err := time.Parse(layout, strings.TrimSpace(s)); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

func listSSOAccess(ctx context.Context, api ssoAPI, token, region string) ([]SSOProfile, error) {
	wrap := func(what string, err error) error {
		if IsAuthError(err) {
			return fmt.Errorf("%w: %s: %w", ErrSSOTokenExpired, what, err)
		}
		return fmt.Errorf("%s: %w", what, err)
	}

	type account struct{ id, name string }
	var accounts []account
	pager := ssoapi.NewListAccountsPaginator(api, &ssoapi.ListAccountsInput{AccessToken: aws.String(token)})
	for pager.HasMorePages() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, wrap("list SSO accounts", err)
		}
		for _, a := range page.AccountList {
			accounts = append(accounts, account{aws.ToString(a.AccountId), aws.ToString(a.AccountName)})
		}
	}

	roles := make([][]string, len(accounts))
	errs := make([]error, len(accounts))
	jobs := make(chan int)
	var wg sync.WaitGroup
	for range min(ssoRoleWorkers, len(accounts)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				roles[i], errs[i] = listRoles(ctx, api, token, accounts[i].id)
			}
		}()
	}
	for i := range accounts {
		jobs <- i
	}
	close(jobs)
	wg.Wait()

	var out []SSOProfile
	for i, a := range accounts {
		if errs[i] != nil {
			return nil, wrap(fmt.Sprintf("list roles for account %s", a.id), errs[i])
		}
		name := a.name
		if name == "" {
			name = a.id
		}
		for _, r := range roles[i] {
			out = append(out, SSOProfile{
				Name:        ProfileName(name, r),
				AccountID:   a.id,
				AccountName: a.name,
				RoleName:    r,
				Region:      region,
			})
		}
	}
	disambiguate(out)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func listRoles(ctx context.Context, api ssoAPI, token, accountID string) ([]string, error) {
	var roles []string
	pager := ssoapi.NewListAccountRolesPaginator(api, &ssoapi.ListAccountRolesInput{
		AccessToken: aws.String(token),
		AccountId:   aws.String(accountID),
	})
	for pager.HasMorePages() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, r := range page.RoleList {
			roles = append(roles, aws.ToString(r.RoleName))
		}
	}
	return roles, nil
}

// disambiguate renames profiles whose generated names collide (accounts with
// the same or slug-equivalent names) by inserting the account ID.
func disambiguate(ps []SSOProfile) {
	count := map[string]int{}
	for _, p := range ps {
		count[p.Name]++
	}
	for i, p := range ps {
		if count[p.Name] > 1 {
			ps[i].Name = ProfileName(p.AccountName+" "+p.AccountID, p.RoleName)
		}
	}
}
