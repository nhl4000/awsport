package awsx

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/smithy-go"

	"github.com/nhl4000/awsport/internal/core"
)

const testPrincipal = "arn:aws:sts::123456789012:assumed-role/AWSReservedSSO_Developer_0123456789abcdef/jane"

// testConfig points every service at srv with static session credentials.
func testConfig(t *testing.T, srv *httptest.Server) aws.Config {
	t.Helper()
	for _, svc := range []string{"EC2", "RDS", "SSM"} {
		t.Setenv("AWS_ENDPOINT_URL_"+svc, "")
	}
	return aws.Config{
		Region: "ca-central-1",
		Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
			return aws.Credentials{AccessKeyID: "AKIDEXAMPLE", SecretAccessKey: "secret", SessionToken: "session-token"}, nil
		}),
		BaseEndpoint: aws.String(srv.URL),
		HTTPClient:   srv.Client(),
	}
}

func testClient(t *testing.T, srv *httptest.Server, service string) *apiClient {
	c := newAPIClient(testConfig(t, srv), "example", service)
	c.retryBase = time.Millisecond
	return c
}

// discoveryServer serves two pages of every discovery API and records the
// Authorization header per service.
type discoveryServer struct {
	t    *testing.T
	mu   sync.Mutex
	auth map[string]string // service -> Authorization
}

func (s *discoveryServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	record := func(service string) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.auth[service] = r.Header.Get("Authorization")
		if r.Header.Get("X-Amz-Security-Token") != "session-token" || r.Header.Get("X-Amz-Date") == "" {
			s.t.Errorf("%s: missing session token or date headers: %v", service, r.Header)
		}
	}
	if target := r.Header.Get("X-Amz-Target"); target != "" {
		record("ssm")
		var in struct {
			Filters []struct {
				Key    string
				Values []string
			}
			NextToken string
		}
		if target != "AmazonSSM.DescribeInstanceInformation" || r.Header.Get("Content-Type") != "application/x-amz-json-1.1" {
			s.t.Errorf("ssm: target %q content type %q", target, r.Header.Get("Content-Type"))
		}
		if err := json.Unmarshal(body, &in); err != nil || len(in.Filters) != 1 || in.Filters[0].Key != "PingStatus" ||
			!reflect.DeepEqual(in.Filters[0].Values, []string{"Online"}) {
			s.t.Errorf("ssm: unexpected request %s (%v)", body, err)
		}
		switch in.NextToken {
		case "":
			fmt.Fprint(w, `{"InstanceInformationList":[{"InstanceId":"i-web","PingStatus":"Online","PlatformType":"Linux"}],"NextToken":"ssm-2"}`)
		case "ssm-2":
			fmt.Fprint(w, `{"InstanceInformationList":[{"InstanceId":"i-jump","PingStatus":"Online"}]}`)
		default:
			s.t.Errorf("ssm: unexpected token %q", in.NextToken)
		}
		return
	}
	form, err := url.ParseQuery(string(body))
	if err != nil {
		s.t.Errorf("parse form: %v", err)
	}
	switch action := form.Get("Action"); action {
	case "DescribeInstances":
		record("ec2")
		want := url.Values{
			"Action": {"DescribeInstances"}, "Version": {"2016-11-15"}, "MaxResults": {"1000"},
			"Filter.1.Name": {"instance-state-name"}, "Filter.1.Value.1": {"pending"}, "Filter.1.Value.2": {"running"},
			"Filter.1.Value.3": {"shutting-down"}, "Filter.1.Value.4": {"stopping"}, "Filter.1.Value.5": {"stopped"},
		}
		token := form.Get("NextToken")
		form.Del("NextToken")
		if !reflect.DeepEqual(form, want) {
			s.t.Errorf("ec2: params %v, want %v", form, want)
		}
		switch token {
		case "":
			fmt.Fprint(w, `<?xml version="1.0" encoding="UTF-8"?>
<DescribeInstancesResponse xmlns="http://ec2.amazonaws.com/doc/2016-11-15/">
  <requestId>r1</requestId>
  <reservationSet><item><reservationId>r-1</reservationId><instancesSet>
    <item>
      <instanceId>i-web</instanceId>
      <instanceState><code>16</code><name>running</name></instanceState>
      <networkInterfaceSet><item><tagSet><item><key>Name</key><value>eni-tag</value></item></tagSet></item></networkInterfaceSet>
      <tagSet><item><key>Name</key><value>web</value></item><item><key>awsport:ports</key><value>app:8080</value></item></tagSet>
    </item>
    <item><instanceId>i-gone</instanceId><instanceState><code>48</code><name>terminated</name></instanceState></item>
  </instancesSet></item></reservationSet>
  <nextToken>ec2-2</nextToken>
</DescribeInstancesResponse>`)
		case "ec2-2":
			fmt.Fprint(w, `<DescribeInstancesResponse xmlns="http://ec2.amazonaws.com/doc/2016-11-15/">
  <reservationSet><item><instancesSet><item>
    <instanceId>i-jump</instanceId>
    <instanceState><code>16</code><name>running</name></instanceState>
    <tagSet><item><key>Name</key><value>jump</value></item><item><key>awsport:bastion</key><value>true</value></item></tagSet>
  </item></instancesSet></item></reservationSet>
</DescribeInstancesResponse>`)
		default:
			s.t.Errorf("ec2: unexpected token %q", token)
		}
	case "DescribeDBInstances":
		record("rds")
		if form.Get("Version") != "2014-10-31" {
			s.t.Errorf("rds: version %q", form.Get("Version"))
		}
		switch form.Get("Marker") {
		case "":
			fmt.Fprint(w, `<DescribeDBInstancesResponse xmlns="http://rds.amazonaws.com/doc/2014-10-31/"><DescribeDBInstancesResult>
  <Marker>db-2</Marker>
  <DBInstances><DBInstance>
    <DBInstanceIdentifier>app-db</DBInstanceIdentifier><DBInstanceStatus>available</DBInstanceStatus><Engine>postgres</Engine>
    <Endpoint><Address>app-db.example.internal</Address><Port>5432</Port><HostedZoneId>Z1</HostedZoneId></Endpoint>
    <DbInstancePort>0</DbInstancePort>
  </DBInstance></DBInstances>
</DescribeDBInstancesResult></DescribeDBInstancesResponse>`)
		case "db-2":
			fmt.Fprint(w, `<DescribeDBInstancesResponse><DescribeDBInstancesResult><DBInstances>
  <DBInstance><DBInstanceIdentifier>aurora-1</DBInstanceIdentifier><DBClusterIdentifier>aurora</DBClusterIdentifier><DBInstanceStatus>available</DBInstanceStatus></DBInstance>
  <DBInstance><DBInstanceIdentifier>creating</DBInstanceIdentifier><DBInstanceStatus>creating</DBInstanceStatus><Engine>mysql</Engine></DBInstance>
</DBInstances></DescribeDBInstancesResult></DescribeDBInstancesResponse>`)
		default:
			s.t.Errorf("rds: unexpected marker %q", form.Get("Marker"))
		}
	case "DescribeDBClusters":
		record("rds")
		switch form.Get("Marker") {
		case "":
			fmt.Fprint(w, `<DescribeDBClustersResponse><DescribeDBClustersResult><Marker>cl-2</Marker><DBClusters><DBCluster>
  <DBClusterIdentifier>aurora</DBClusterIdentifier><Status>available</Status><Engine>aurora-postgresql</Engine>
  <Endpoint>aurora.cluster-example.internal</Endpoint><ReaderEndpoint>aurora.cluster-ro-example.internal</ReaderEndpoint><Port>5432</Port>
  <DBClusterMembers><DBClusterMember><DBInstanceIdentifier>aurora-1</DBInstanceIdentifier></DBClusterMember></DBClusterMembers>
  <VpcSecurityGroups><VpcSecurityGroupMembership><Status>active</Status></VpcSecurityGroupMembership></VpcSecurityGroups>
</DBCluster></DBClusters></DescribeDBClustersResult></DescribeDBClustersResponse>`)
		case "cl-2":
			fmt.Fprint(w, `<DescribeDBClustersResponse><DescribeDBClustersResult><DBClusters><DBCluster>
  <DBClusterIdentifier>reporting</DBClusterIdentifier><Status>available</Status><Engine>aurora-mysql</Engine>
  <Endpoint>reporting.cluster-example.internal</Endpoint><Port>3306</Port>
</DBCluster></DBClusters></DescribeDBClustersResult></DescribeDBClustersResponse>`)
		default:
			s.t.Errorf("rds clusters: unexpected marker %q", form.Get("Marker"))
		}
	default:
		s.t.Errorf("unexpected action %q", action)
		http.Error(w, "unsupported", http.StatusBadRequest)
	}
}

func TestDiscoverRegionHTTP(t *testing.T) {
	fake := &discoveryServer{t: t, auth: map[string]string{}}
	srv := httptest.NewServer(fake)
	defer srv.Close()

	before := time.Now().UTC().Format("20060102")
	got, err := discoverRegion(context.Background(), newRegionClients(testConfig(t, srv), "example"),
		discoverOptions{profile: "example", region: "ca-central-1"})
	after := time.Now().UTC().Format("20060102")
	if err != nil {
		t.Fatal(err)
	}
	type row struct {
		ID, Name, State, Host string
		Port                  int
		Online, Bastion       bool
		Ports                 []core.PortSpec
	}
	var rows []row
	for _, r := range got {
		rows = append(rows, row{r.ID, r.Name, r.State, r.Host, r.Port, r.SSMOnline, r.IsBastion, r.Ports})
	}
	want := []row{
		{ID: "i-jump", Name: "jump", State: "running", Online: true, Bastion: true, Ports: []core.PortSpec{}},
		{ID: "i-web", Name: "web", State: "running", Online: true, Ports: []core.PortSpec{{Name: "app", Remote: 8080}}},
		{ID: "app-db", Name: "app-db", State: "available", Host: "app-db.example.internal", Port: 5432},
		{ID: "aurora", Name: "aurora", State: "available", Host: "aurora.cluster-example.internal", Port: 5432},
		{ID: "creating", Name: "creating", State: "creating"},
		{ID: "reporting", Name: "reporting", State: "available", Host: "reporting.cluster-example.internal", Port: 3306},
	}
	if !reflect.DeepEqual(rows, want) {
		t.Errorf("resources\n got: %+v\nwant: %+v", rows, want)
	}

	for _, svc := range []string{"ec2", "ssm", "rds"} {
		auth := fake.auth[svc]
		scope := "/ca-central-1/" + svc + "/aws4_request, SignedHeaders="
		if !strings.HasPrefix(auth, "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/") || !strings.Contains(auth, scope) ||
			!strings.Contains(auth, ", Signature=") {
			t.Errorf("%s Authorization = %q, want SigV4 with scope %s", svc, auth, scope)
		}
		if !strings.Contains(auth, "/"+before+"/") && !strings.Contains(auth, "/"+after+"/") {
			t.Errorf("%s Authorization = %q, want date %s", svc, auth, before)
		}
	}
}

func TestAPIErrors(t *testing.T) {
	deniedMsg := func(action string) string {
		return "User: " + testPrincipal + " is not authorized to perform: " + action +
			" because no identity-based policy allows the " + action + " action"
	}
	tests := []struct {
		name       string
		service    string
		status     int
		header     map[string]string
		body       string
		wantCode   string
		wantAction string // non-empty: *PermissionError expected
		wantAuth   bool
	}{
		{name: "ec2 unauthorized operation", service: "ec2", status: 403,
			body:     `<?xml version="1.0" encoding="UTF-8"?><Response><Errors><Error><Code>UnauthorizedOperation</Code><Message>You are not authorized to perform this operation. ` + deniedMsg("ec2:DescribeInstances") + `. Encoded authorization failure message: abc</Message></Error></Errors><RequestID>r1</RequestID></Response>`,
			wantCode: "UnauthorizedOperation", wantAction: "ec2:DescribeInstances"},
		{name: "ec2 auth failure", service: "ec2", status: 401,
			body:     `<Response><Errors><Error><Code>AuthFailure</Code><Message>AWS was not able to validate the provided access credentials</Message></Error></Errors></Response>`,
			wantCode: "AuthFailure", wantAuth: true},
		{name: "rds access denied", service: "rds", status: 403,
			body:     `<ErrorResponse xmlns="http://rds.amazonaws.com/doc/2014-10-31/"><Error><Type>Sender</Type><Code>AccessDenied</Code><Message>` + deniedMsg("rds:DescribeDBInstances") + `</Message></Error><RequestId>r2</RequestId></ErrorResponse>`,
			wantCode: "AccessDenied", wantAction: "rds:DescribeDBInstances"},
		{name: "rds expired token", service: "rds", status: 403,
			body:     `<ErrorResponse><Error><Type>Sender</Type><Code>ExpiredToken</Code><Message>The security token included in the request is expired</Message></Error></ErrorResponse>`,
			wantCode: "ExpiredToken", wantAuth: true},
		{name: "ssm access denied", service: "ssm", status: 400,
			body:     `{"__type":"com.amazon.coral.service#AccessDeniedException","message":"` + deniedMsg("ssm:DescribeInstanceInformation") + `"}`,
			wantCode: "AccessDeniedException", wantAction: "ssm:DescribeInstanceInformation"},
		{name: "ssm code from header", service: "ssm", status: 400,
			header:   map[string]string{"X-Amzn-Errortype": "UnrecognizedClientException:http://internal.amazon.com/coral/com.amazon.coral.service/"},
			body:     `{"Message":"The security token included in the request is invalid."}`,
			wantCode: "UnrecognizedClientException", wantAuth: true},
		{name: "ssm validation", service: "ssm", status: 400,
			body:     `{"__type":"InvalidFilterKey","message":"bad filter"}`,
			wantCode: "InvalidFilterKey"},
		{name: "unparseable", service: "rds", status: 400, body: `<html>bad gateway</html>`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				for k, v := range tt.header {
					w.Header().Set(k, v)
				}
				w.WriteHeader(tt.status)
				fmt.Fprint(w, tt.body)
			}))
			defer srv.Close()
			c := testClient(t, srv, tt.service)
			var err error
			switch tt.service {
			case "ec2":
				_, err = ec2Client{c}.describeInstances(context.Background())
			case "rds":
				_, err = rdsClient{c}.describeDBInstances(context.Background())
			case "ssm":
				_, err = ssmClient{c}.describeOnlineInstances(context.Background())
			}
			if err == nil {
				t.Fatal("want error")
			}
			var apiErr smithy.APIError
			if tt.wantCode == "" {
				if errors.As(err, &apiErr) || !strings.Contains(err.Error(), "HTTP 400") {
					t.Errorf("err = %v, want plain HTTP 400 error", err)
				}
			} else if !errors.As(err, &apiErr) || apiErr.ErrorCode() != tt.wantCode {
				t.Errorf("err = %v, want API error code %s", err, tt.wantCode)
			}
			if got := IsAuthError(err); got != tt.wantAuth {
				t.Errorf("IsAuthError(%v) = %v, want %v", err, got, tt.wantAuth)
			}
			var pe *PermissionError
			if !errors.As(err, &pe) {
				if tt.wantAction != "" {
					t.Fatalf("err = %v, want *PermissionError", err)
				}
				return
			}
			if tt.wantAction == "" {
				t.Fatalf("unexpected *PermissionError %v", pe)
			}
			if pe.Action != tt.wantAction || pe.Principal != testPrincipal || pe.Profile != "example" ||
				pe.Region != "ca-central-1" || pe.What == "" || pe.PermissionSet() != "Developer" {
				t.Errorf("PermissionError = %+v", pe)
			}
		})
	}
}

func TestAPIRetry(t *testing.T) {
	throttle := func(w http.ResponseWriter) {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `<ErrorResponse><Error><Code>Throttling</Code><Message>Rate exceeded</Message></Error></ErrorResponse>`)
	}
	ok := func(w http.ResponseWriter) {
		fmt.Fprint(w, `<DescribeDBClustersResponse><DescribeDBClustersResult><DBClusters><DBCluster><DBClusterIdentifier>aurora</DBClusterIdentifier></DBCluster></DBClusters></DescribeDBClustersResult></DescribeDBClustersResponse>`)
	}
	unavailable := func(w http.ResponseWriter) { http.Error(w, "unavailable", http.StatusServiceUnavailable) }
	tests := []struct {
		name      string
		responses []func(http.ResponseWriter)
		wantCalls int32
		wantErr   string
	}{
		{"throttled then success", []func(http.ResponseWriter){throttle, throttle, ok}, 3, ""},
		{"5xx then success", []func(http.ResponseWriter){unavailable, ok}, 2, ""},
		{"gives up after 3 attempts", []func(http.ResponseWriter){throttle}, 3, "Throttling"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				i := int(calls.Add(1)) - 1
				tt.responses[min(i, len(tt.responses)-1)](w)
			}))
			defer srv.Close()
			got, err := rdsClient{testClient(t, srv, "rds")}.describeDBClusters(context.Background())
			if n := calls.Load(); n != tt.wantCalls {
				t.Errorf("calls = %d, want %d", n, tt.wantCalls)
			}
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %s", err, tt.wantErr)
				}
				return
			}
			if err != nil || len(got) != 1 || got[0].ID != "aurora" {
				t.Fatalf("got (%+v, %v), want aurora", got, err)
			}
		})
	}
}

func TestAPIRetryRespectsContext(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"__type":"ThrottlingException"}`, http.StatusBadRequest)
	}))
	defer srv.Close()
	c := testClient(t, srv, "ssm")
	c.retryBase = time.Hour
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := ssmClient{c}.describeOnlineInstances(ctx)
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 10*time.Second {
		t.Fatalf("err = %v after %v, want deadline exceeded promptly", err, time.Since(start))
	}
}

func TestResolveEndpoint(t *testing.T) {
	t.Setenv("AWS_ENDPOINT_URL_EC2", "")
	t.Setenv("AWS_IGNORE_CONFIGURED_ENDPOINT_URLS", "")
	tests := []struct {
		service, region, base, env, want string
	}{
		{"ec2", "ca-central-1", "", "", "https://ec2.ca-central-1.amazonaws.com"},
		{"rds", "cn-north-1", "", "", "https://rds.cn-north-1.amazonaws.com.cn"},
		{"ssm", "us-east-1", "http://127.0.0.1:4599", "", "http://127.0.0.1:4599"},
		{"ec2", "us-east-1", "http://127.0.0.1:4599", "http://localhost:9000", "http://localhost:9000"},
	}
	for _, tt := range tests {
		t.Setenv("AWS_ENDPOINT_URL_"+strings.ToUpper(tt.service), tt.env)
		if got := resolveEndpoint(tt.service, tt.region, tt.base); got != tt.want {
			t.Errorf("resolveEndpoint(%s, %s, %q) with env %q = %s, want %s", tt.service, tt.region, tt.base, tt.env, got, tt.want)
		}
	}
}
