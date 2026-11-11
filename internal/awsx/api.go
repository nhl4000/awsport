package awsx

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/smithy-go"
)

// The few read-only EC2, SSM and RDS calls discovery needs are made with this
// small SigV4 client instead of the generated SDK service clients, which are
// huge (the EC2 one alone dominated build time and binary size).

const (
	apiUserAgent    = "awsport"
	apiMaxAttempts  = 3
	apiRetryBase    = 300 * time.Millisecond
	apiErrBodyLimit = 1 << 20
)

// throttleCodes are error codes worth retrying after a backoff.
var throttleCodes = map[string]bool{
	"Throttling":           true,
	"ThrottlingException":  true,
	"RequestLimitExceeded": true,
}

// deniedCodes are error codes of authorization (IAM permission) failures.
var deniedCodes = map[string]bool{
	"AccessDenied":          true,
	"AccessDeniedException": true,
	"UnauthorizedOperation": true,
}

// apiClient calls one AWS service in one region with SigV4-signed requests.
type apiClient struct {
	service   string // endpoint prefix and SigV4 signing name, e.g. "ec2"
	profile   string // reported in *PermissionError
	region    string
	endpoint  string
	creds     aws.CredentialsProvider
	http      aws.HTTPClient
	signer    *v4.Signer
	retryBase time.Duration
}

func newAPIClient(cfg aws.Config, profile, service string) *apiClient {
	var hc aws.HTTPClient = http.DefaultClient
	if cfg.HTTPClient != nil {
		hc = cfg.HTTPClient
	}
	return &apiClient{
		service:   service,
		profile:   profile,
		region:    cfg.Region,
		endpoint:  resolveEndpoint(service, cfg.Region, aws.ToString(cfg.BaseEndpoint)),
		creds:     cfg.Credentials,
		http:      hc,
		signer:    v4.NewSigner(),
		retryBase: apiRetryBase,
	}
}

// resolveEndpoint returns the service URL: $AWS_ENDPOINT_URL_<SERVICE>, then
// base (the config's AWS_ENDPOINT_URL / endpoint_url), then the public
// regional endpoint.
func resolveEndpoint(service, region, base string) string {
	if os.Getenv("AWS_IGNORE_CONFIGURED_ENDPOINT_URLS") != "true" {
		if v := os.Getenv("AWS_ENDPOINT_URL_" + strings.ToUpper(service)); v != "" {
			return v
		}
	}
	if base != "" {
		return base
	}
	domain := "amazonaws.com"
	if strings.HasPrefix(region, "cn-") {
		domain = "amazonaws.com.cn"
	}
	return "https://" + service + "." + region + "." + domain
}

// query calls an AWS query-protocol operation (EC2, RDS) and decodes the XML
// response into out. what describes the call for *PermissionError.
func (c *apiClient) query(ctx context.Context, op, version, what string, params url.Values, out any) error {
	body := "Action=" + url.QueryEscape(op) + "&Version=" + url.QueryEscape(version)
	if len(params) > 0 {
		body += "&" + params.Encode()
	}
	resp, err := c.send(ctx, op, what, []byte(body), func(h http.Header) {
		h.Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8")
	})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if err := xml.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("%s: %s %s: decode response: %w", c.region, c.service, op, err)
	}
	return nil
}

// jsonCall calls an AWS JSON 1.1 operation (target "<prefix>.<op>") and
// decodes the response into out.
func (c *apiClient) jsonCall(ctx context.Context, targetPrefix, op, what string, in, out any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return fmt.Errorf("%s: %s %s: encode request: %w", c.region, c.service, op, err)
	}
	resp, err := c.send(ctx, op, what, body, func(h http.Header) {
		h.Set("Content-Type", "application/x-amz-json-1.1")
		h.Set("X-Amz-Target", targetPrefix+"."+op)
	})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("%s: %s %s: decode response: %w", c.region, c.service, op, err)
	}
	return nil
}

// send POSTs a signed request, retrying throttling and 5xx responses. On
// success the caller owns (and closes) the response body.
func (c *apiClient) send(ctx context.Context, op, what string, body []byte, headers func(http.Header)) (*http.Response, error) {
	fail := func(err error) error { return fmt.Errorf("%s: %s %s: %w", c.region, c.service, op, err) }
	if c.creds == nil {
		return nil, fail(errors.New("no AWS credentials configured"))
	}
	creds, err := c.creds.Retrieve(ctx)
	if err != nil {
		return nil, fail(err)
	}
	sum := sha256.Sum256(body)
	payloadHash := hex.EncodeToString(sum[:])
	for attempt := 1; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
		if err != nil {
			return nil, fail(err)
		}
		headers(req.Header)
		req.Header.Set("User-Agent", apiUserAgent)
		if err := c.signer.SignHTTP(ctx, creds, req, payloadHash, c.service, c.region, time.Now()); err != nil {
			return nil, fail(fmt.Errorf("sign request: %w", err))
		}
		resp, err := c.http.Do(req)
		if err != nil {
			return nil, fail(err)
		}
		if resp.StatusCode < 300 {
			return resp, nil
		}
		apiErr := readAPIError(resp)
		var ge *smithy.GenericAPIError
		retry := resp.StatusCode >= 500 || (errors.As(apiErr, &ge) && throttleCodes[ge.Code])
		if !retry || attempt == apiMaxAttempts {
			if pe := c.permissionError(op, what, apiErr); pe != nil {
				return nil, pe
			}
			return nil, fail(apiErr)
		}
		if err := c.sleep(ctx, attempt); err != nil {
			return nil, fail(err)
		}
	}
}

// sleep waits a jittered exponential backoff before retry attempt+1.
func (c *apiClient) sleep(ctx context.Context, attempt int) error {
	d := c.retryBase << (attempt - 1)
	d = d/2 + rand.N(d/2+1)
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// permissionError converts an access-denied API error into *PermissionError
// for action <service>:<op>; it returns nil for other errors.
func (c *apiClient) permissionError(op, what string, err error) *PermissionError {
	var ge *smithy.GenericAPIError
	if !errors.As(err, &ge) || authErrorCodes[ge.Code] {
		return nil
	}
	_, principal, denied := ParseAccessDenied(ge.Message)
	if !deniedCodes[ge.Code] && !denied {
		return nil
	}
	return &PermissionError{
		Profile:   c.profile,
		Region:    c.region,
		Action:    c.service + ":" + op,
		What:      what,
		Principal: principal,
		Err:       ge,
	}
}

// readAPIError parses (and closes) an error response: EC2
// <Response><Errors><Error>, query <ErrorResponse><Error> or JSON
// {"__type", "message"}. Unparseable bodies yield a plain HTTP status error.
func readAPIError(resp *http.Response) error {
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, apiErrBodyLimit))
	code, msg := parseAPIError(data, resp.Header.Get("X-Amzn-Errortype"))
	if code == "" {
		snippet := strings.TrimSpace(string(data))
		if len(snippet) > 200 {
			snippet = snippet[:200] + "…"
		}
		if snippet == "" {
			return fmt.Errorf("HTTP %s", resp.Status)
		}
		return fmt.Errorf("HTTP %s: %s", resp.Status, snippet)
	}
	return &smithy.GenericAPIError{Code: code, Message: msg}
}

type xmlAPIError struct {
	Code    string `xml:"Code"`
	Message string `xml:"Message"`
}

func parseAPIError(data []byte, typeHeader string) (code, msg string) {
	data = bytes.TrimSpace(data)
	if bytes.HasPrefix(data, []byte("<")) {
		var e struct {
			EC2   []xmlAPIError `xml:"Errors>Error"` // <Response> (EC2)
			Query xmlAPIError   `xml:"Error"`        // <ErrorResponse> (RDS, STS)
		}
		if xml.Unmarshal(data, &e) != nil {
			return "", ""
		}
		if len(e.EC2) > 0 {
			return e.EC2[0].Code, e.EC2[0].Message
		}
		return e.Query.Code, e.Query.Message
	}
	var e struct {
		Type    string `json:"__type"`
		Code    string `json:"code"`
		Message string `json:"message"` // also matches "Message"
	}
	_ = json.Unmarshal(data, &e)
	code = e.Type
	if code == "" {
		code = e.Code
	}
	if code == "" {
		code, _, _ = strings.Cut(typeHeader, ":")
	}
	code = code[strings.LastIndexByte(code, '#')+1:]
	return strings.TrimSpace(code), e.Message
}

// --- EC2 -------------------------------------------------------------------

const ec2APIVersion = "2016-11-15"

// liveInstanceStates are the instance-state-name filter values (all but
// terminated).
var liveInstanceStates = []string{"pending", "running", "shutting-down", "stopping", "stopped"}

type ec2Tag struct {
	Key   string `xml:"key"`
	Value string `xml:"value"`
}

// ec2Instance holds the DescribeInstances fields discovery uses.
type ec2Instance struct {
	ID    string   `xml:"instanceId"`
	State string   `xml:"instanceState>name"`
	Tags  []ec2Tag `xml:"tagSet>item"`
}

type ec2Client struct{ *apiClient }

func (c ec2Client) describeInstances(ctx context.Context) ([]ec2Instance, error) {
	params := url.Values{
		"Filter.1.Name": {"instance-state-name"},
		"MaxResults":    {"1000"},
	}
	for i, s := range liveInstanceStates {
		params.Set(fmt.Sprintf("Filter.1.Value.%d", i+1), s)
	}
	var out []ec2Instance
	for {
		var page struct {
			Reservations []struct {
				Instances []ec2Instance `xml:"instancesSet>item"`
			} `xml:"reservationSet>item"`
			NextToken string `xml:"nextToken"`
		}
		if err := c.query(ctx, "DescribeInstances", ec2APIVersion, "list EC2 instances", params, &page); err != nil {
			return nil, err
		}
		for _, r := range page.Reservations {
			out = append(out, r.Instances...)
		}
		if page.NextToken == "" || page.NextToken == params.Get("NextToken") {
			return out, nil
		}
		params.Set("NextToken", page.NextToken)
	}
}

// --- SSM -------------------------------------------------------------------

const ssmPingOnline = "Online"

// ssmInstanceInfo holds the DescribeInstanceInformation fields discovery uses.
type ssmInstanceInfo struct {
	InstanceID string `json:"InstanceId"`
	PingStatus string `json:"PingStatus"`
}

type ssmClient struct{ *apiClient }

// describeOnlineInstances lists managed instances whose ping status is Online.
func (c ssmClient) describeOnlineInstances(ctx context.Context) ([]ssmInstanceInfo, error) {
	type filter struct {
		Key    string
		Values []string
	}
	req := struct {
		Filters    []filter
		MaxResults int
		NextToken  string `json:",omitempty"`
	}{Filters: []filter{{Key: "PingStatus", Values: []string{ssmPingOnline}}}, MaxResults: 50}
	var out []ssmInstanceInfo
	for {
		var page struct {
			InstanceInformationList []ssmInstanceInfo
			NextToken               string
		}
		if err := c.jsonCall(ctx, "AmazonSSM", "DescribeInstanceInformation", "check SSM agent status", req, &page); err != nil {
			return nil, err
		}
		out = append(out, page.InstanceInformationList...)
		if page.NextToken == "" || page.NextToken == req.NextToken {
			return out, nil
		}
		req.NextToken = page.NextToken
	}
}

// --- RDS -------------------------------------------------------------------

const rdsAPIVersion = "2014-10-31"

// dbInstance holds the DescribeDBInstances fields discovery uses.
type dbInstance struct {
	ID        string `xml:"DBInstanceIdentifier"`
	ClusterID string `xml:"DBClusterIdentifier"`
	Status    string `xml:"DBInstanceStatus"`
	Engine    string `xml:"Engine"`
	Address   string `xml:"Endpoint>Address"`
	Port      int    `xml:"Endpoint>Port"`
}

// dbCluster holds the DescribeDBClusters fields discovery uses (Endpoint is
// the writer endpoint).
type dbCluster struct {
	ID       string `xml:"DBClusterIdentifier"`
	Status   string `xml:"Status"`
	Engine   string `xml:"Engine"`
	Endpoint string `xml:"Endpoint"`
	Port     int    `xml:"Port"`
}

type rdsClient struct{ *apiClient }

func (c rdsClient) describeDBInstances(ctx context.Context) ([]dbInstance, error) {
	var out []dbInstance
	params := url.Values{"MaxRecords": {"100"}}
	for {
		var page struct {
			Items  []dbInstance `xml:"DescribeDBInstancesResult>DBInstances>DBInstance"`
			Marker string       `xml:"DescribeDBInstancesResult>Marker"`
		}
		if err := c.query(ctx, "DescribeDBInstances", rdsAPIVersion, "list RDS databases", params, &page); err != nil {
			return nil, err
		}
		out = append(out, page.Items...)
		if page.Marker == "" || page.Marker == params.Get("Marker") {
			return out, nil
		}
		params.Set("Marker", page.Marker)
	}
}

func (c rdsClient) describeDBClusters(ctx context.Context) ([]dbCluster, error) {
	var out []dbCluster
	params := url.Values{"MaxRecords": {"100"}}
	for {
		var page struct {
			Items  []dbCluster `xml:"DescribeDBClustersResult>DBClusters>DBCluster"`
			Marker string      `xml:"DescribeDBClustersResult>Marker"`
		}
		if err := c.query(ctx, "DescribeDBClusters", rdsAPIVersion, "list Aurora clusters", params, &page); err != nil {
			return nil, err
		}
		out = append(out, page.Items...)
		if page.Marker == "" || page.Marker == params.Get("Marker") {
			return out, nil
		}
		params.Set("Marker", page.Marker)
	}
}
