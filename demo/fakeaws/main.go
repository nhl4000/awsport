// Command fakeaws is a tiny stand-in for the handful of AWS APIs awsport calls
// (SSO GetRoleCredentials/ListAccounts/ListAccountRoles, STS
// GetCallerIdentity, EC2 DescribeInstances, SSM DescribeInstanceInformation,
// RDS DescribeDBInstances/DescribeDBClusters). It serves fictional "acme"
// accounts so the README demo can be recorded without touching real AWS.
//
// Point the SDK at it with AWS_ENDPOINT_URL=http://127.0.0.1:<port>.
package main

import (
	"encoding/json"
	"encoding/xml"
	"flag"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strings"
	"time"
)

type instance struct {
	ID, Name, State string
	Online          bool
	Tags            map[string]string
}

type database struct {
	ID, Engine, Host string
	Port             int
	Cluster          bool
}

type account struct {
	ID, Name, Role string
	Instances      []instance
	Databases      []database
}

const region = "us-east-1"

var accounts = []account{
	{ID: "111111111111", Name: "acme-dev", Role: "developer",
		Instances: []instance{
			{ID: "i-0d3v000000000001", Name: "web-app", State: "running", Online: true, Tags: map[string]string{"awsport:ports": "mailpit:8025 app:8080"}},
			{ID: "i-0d3v000000000002", Name: "bastion", State: "running", Online: true, Tags: map[string]string{"awsport:bastion": "true"}},
		},
		Databases: []database{{ID: "app-db", Engine: "postgres", Host: "app-db.c0ffee000001.us-east-1.rds.amazonaws.com", Port: 5432}},
	},
	{ID: "222222222222", Name: "acme-staging", Role: "developer",
		Instances: []instance{
			{ID: "i-0s7a000000000001", Name: "web-app", State: "running", Online: true, Tags: map[string]string{"awsport:ports": "mailpit:8025 app:8080"}},
			{ID: "i-0s7a000000000002", Name: "worker", State: "running", Online: true, Tags: map[string]string{"awsport:ports": "metrics:9090"}},
			{ID: "i-0s7a000000000003", Name: "bastion", State: "running", Online: true, Tags: map[string]string{"awsport:bastion": "true"}},
			{ID: "i-0s7a000000000004", Name: "legacy-cron", State: "stopped"},
		},
		Databases: []database{
			{ID: "app-db", Engine: "postgres", Host: "app-db.c0ffee000002.us-east-1.rds.amazonaws.com", Port: 5432},
			{ID: "analytics", Engine: "aurora-mysql", Host: "analytics.cluster-c0ffee000002.us-east-1.rds.amazonaws.com", Port: 3306, Cluster: true},
		},
	},
	{ID: "333333333333", Name: "acme-prod", Role: "readonly",
		Instances: []instance{
			{ID: "i-0p40000000000001", Name: "web-app", State: "running", Online: true, Tags: map[string]string{"awsport:ports": "app:8080"}},
			{ID: "i-0p40000000000002", Name: "bastion", State: "running", Online: true, Tags: map[string]string{"awsport:bastion": "true"}},
		},
		Databases: []database{{ID: "app-db", Engine: "postgres", Host: "app-db.c0ffee000003.us-east-1.rds.amazonaws.com", Port: 5432}},
	},
}

// Access keys are "ASIADEMO" + account ID, so SigV4 requests identify their account.
const akidPrefix = "ASIADEMO"

var credRe = regexp.MustCompile(`Credential=([A-Z0-9]+)/\d+/([a-z0-9-]+)/`)

// denied holds IAM actions (e.g. "rds:DescribeDBInstances") answered with an
// access-denied error, to preview awsport's permission messages.
var denied = map[string]bool{}

func main() {
	addr := flag.String("addr", "127.0.0.1:4599", "listen address")
	deny := flag.String("deny", "", "comma-separated IAM actions to deny, e.g. rds:DescribeDBInstances,ssm:DescribeInstanceInformation")
	flag.Parse()
	for _, a := range strings.Split(*deny, ",") {
		if a = strings.TrimSpace(a); a != "" {
			denied[a] = true
		}
	}
	log.Printf("fakeaws listening on http://%s", *addr)
	log.Fatal(http.ListenAndServe(*addr, http.HandlerFunc(handle)))
}

func handle(w http.ResponseWriter, r *http.Request) {
	switch {
	case strings.HasPrefix(r.URL.Path, "/federation/credentials"):
		roleCredentials(w, r)
		return
	case strings.HasPrefix(r.URL.Path, "/assignment/roles"):
		listRoles(w, r)
		return
	case strings.HasPrefix(r.URL.Path, "/assignment/accounts"):
		listAccounts(w)
		return
	}
	acct, reg := caller(r)
	if target := r.Header.Get("X-Amz-Target"); target != "" {
		if target == "AmazonSSM.DescribeInstanceInformation" {
			if denyIfConfigured(w, acct, "ssm", "DescribeInstanceInformation") {
				return
			}
			ssmInfo(w, acct, reg)
			return
		}
		http.Error(w, "unsupported target "+target, http.StatusBadRequest)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	action := r.PostForm.Get("Action")
	service := map[string]string{"DescribeInstances": "ec2", "DescribeDBInstances": "rds", "DescribeDBClusters": "rds"}[action]
	if service != "" && denyIfConfigured(w, acct, service, action) {
		return
	}
	switch action {
	case "GetCallerIdentity":
		writeXML(w, stsIdentity(acct))
	case "DescribeInstances":
		writeXML(w, ec2Instances(acct, reg))
	case "DescribeDBInstances":
		writeXML(w, rdsInstances(acct, reg))
	case "DescribeDBClusters":
		writeXML(w, rdsClusters(acct, reg))
	default:
		http.Error(w, "unsupported action "+action, http.StatusBadRequest)
	}
}

// denyIfConfigured writes the service's real access-denied error shape if
// service:action is in the -deny list.
func denyIfConfigured(w http.ResponseWriter, a *account, service, action string) bool {
	iam := service + ":" + action
	if !denied[iam] {
		return false
	}
	id, role := "000000000000", "developer"
	if a != nil {
		id, role = a.ID, a.Role
	}
	msg := fmt.Sprintf("User: arn:aws:sts::%s:assumed-role/AWSReservedSSO_%s_0123abcd/demo is not authorized to perform: %s because no identity-based policy allows the %s action", id, strings.ToUpper(role[:1])+role[1:], iam, iam)
	switch service {
	case "ssm":
		w.Header().Set("Content-Type", "application/x-amz-json-1.1")
		w.WriteHeader(http.StatusBadRequest)
		writeJSON(w, map[string]string{"__type": "AccessDeniedException", "message": msg})
	case "ec2":
		w.Header().Set("Content-Type", "text/xml")
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?><Response><Errors><Error><Code>UnauthorizedOperation</Code><Message>You are not authorized to perform this operation. %s</Message></Error></Errors><RequestID>demo</RequestID></Response>`, msg)
	default:
		w.Header().Set("Content-Type", "text/xml")
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?><ErrorResponse xmlns="http://rds.amazonaws.com/doc/2014-10-31/"><Error><Type>Sender</Type><Code>AccessDenied</Code><Message>%s</Message></Error><RequestId>demo</RequestId></ErrorResponse>`, msg)
	}
	return true
}

func caller(r *http.Request) (*account, string) {
	m := credRe.FindStringSubmatch(r.Header.Get("Authorization"))
	if m == nil {
		return nil, ""
	}
	id := strings.TrimPrefix(m[1], akidPrefix)
	for i := range accounts {
		if accounts[i].ID == id {
			return &accounts[i], m[2]
		}
	}
	return nil, m[2]
}

func roleCredentials(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("account_id")
	writeJSON(w, map[string]any{"roleCredentials": map[string]any{
		"accessKeyId":     akidPrefix + id,
		"secretAccessKey": "demo-secret",
		"sessionToken":    "demo-session-token",
		"expiration":      time.Now().Add(8 * time.Hour).UnixMilli(),
	}})
}

func listAccounts(w http.ResponseWriter) {
	list := make([]map[string]string, 0, len(accounts))
	for _, a := range accounts {
		list = append(list, map[string]string{"accountId": a.ID, "accountName": a.Name, "emailAddress": a.Name + "@example.com"})
	}
	writeJSON(w, map[string]any{"accountList": list})
}

func listRoles(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("account_id")
	var roles []map[string]string
	for _, a := range accounts {
		if a.ID == id {
			roles = append(roles, map[string]string{"accountId": a.ID, "roleName": a.Role})
		}
	}
	writeJSON(w, map[string]any{"roleList": roles})
}

func ssmInfo(w http.ResponseWriter, a *account, reg string) {
	list := []map[string]string{}
	if a != nil && reg == region {
		for _, in := range a.Instances {
			if in.Online {
				list = append(list, map[string]string{"InstanceId": in.ID, "PingStatus": "Online", "PlatformType": "Linux"})
			}
		}
	}
	w.Header().Set("Content-Type", "application/x-amz-json-1.1")
	writeJSON(w, map[string]any{"InstanceInformationList": list})
}

// --- XML responses -------------------------------------------------------

type stsResp struct {
	XMLName xml.Name `xml:"https://sts.amazonaws.com/doc/2011-06-15/ GetCallerIdentityResponse"`
	Arn     string   `xml:"GetCallerIdentityResult>Arn"`
	UserID  string   `xml:"GetCallerIdentityResult>UserId"`
	Account string   `xml:"GetCallerIdentityResult>Account"`
	ReqID   string   `xml:"ResponseMetadata>RequestId"`
}

func stsIdentity(a *account) any {
	if a == nil {
		return stsResp{}
	}
	return stsResp{
		Arn:     fmt.Sprintf("arn:aws:sts::%s:assumed-role/AWSReservedSSO_%s/demo", a.ID, a.Role),
		UserID:  "AROADEMO:demo",
		Account: a.ID,
		ReqID:   "demo",
	}
}

type ec2Tag struct {
	Key   string `xml:"key"`
	Value string `xml:"value"`
}
type ec2Instance struct {
	ID    string   `xml:"instanceId"`
	Code  int      `xml:"instanceState>code"`
	State string   `xml:"instanceState>name"`
	Type  string   `xml:"instanceType"`
	Tags  []ec2Tag `xml:"tagSet>item"`
}
type ec2Reservation struct {
	ID        string        `xml:"reservationId"`
	Instances []ec2Instance `xml:"instancesSet>item"`
}
type ec2Resp struct {
	XMLName      xml.Name         `xml:"http://ec2.amazonaws.com/doc/2016-11-15/ DescribeInstancesResponse"`
	ReqID        string           `xml:"requestId"`
	Reservations []ec2Reservation `xml:"reservationSet>item"`
}

func ec2Instances(a *account, reg string) any {
	resp := ec2Resp{ReqID: "demo"}
	if a == nil || reg != region {
		return resp
	}
	for _, in := range a.Instances {
		code := 16
		if in.State == "stopped" {
			code = 80
		}
		tags := []ec2Tag{{Key: "Name", Value: in.Name}}
		for k, v := range in.Tags {
			tags = append(tags, ec2Tag{Key: k, Value: v})
		}
		resp.Reservations = append(resp.Reservations, ec2Reservation{
			ID:        "r-" + in.ID[2:],
			Instances: []ec2Instance{{ID: in.ID, Code: code, State: in.State, Type: "t3.small", Tags: tags}},
		})
	}
	return resp
}

type rdsInstance struct {
	ID      string `xml:"DBInstanceIdentifier"`
	Status  string `xml:"DBInstanceStatus"`
	Engine  string `xml:"Engine"`
	Address string `xml:"Endpoint>Address"`
	Port    int    `xml:"Endpoint>Port"`
}
type rdsInstancesResp struct {
	XMLName   xml.Name      `xml:"http://rds.amazonaws.com/doc/2014-10-31/ DescribeDBInstancesResponse"`
	Instances []rdsInstance `xml:"DescribeDBInstancesResult>DBInstances>DBInstance"`
	ReqID     string        `xml:"ResponseMetadata>RequestId"`
}
type rdsCluster struct {
	ID       string `xml:"DBClusterIdentifier"`
	Status   string `xml:"Status"`
	Engine   string `xml:"Engine"`
	Endpoint string `xml:"Endpoint"`
	Port     int    `xml:"Port"`
}
type rdsClustersResp struct {
	XMLName  xml.Name     `xml:"http://rds.amazonaws.com/doc/2014-10-31/ DescribeDBClustersResponse"`
	Clusters []rdsCluster `xml:"DescribeDBClustersResult>DBClusters>DBCluster"`
	ReqID    string       `xml:"ResponseMetadata>RequestId"`
}

func rdsInstances(a *account, reg string) any {
	resp := rdsInstancesResp{ReqID: "demo"}
	if a == nil || reg != region {
		return resp
	}
	for _, d := range a.Databases {
		if !d.Cluster {
			resp.Instances = append(resp.Instances, rdsInstance{ID: d.ID, Status: "available", Engine: d.Engine, Address: d.Host, Port: d.Port})
		}
	}
	return resp
}

func rdsClusters(a *account, reg string) any {
	resp := rdsClustersResp{ReqID: "demo"}
	if a == nil || reg != region {
		return resp
	}
	for _, d := range a.Databases {
		if d.Cluster {
			resp.Clusters = append(resp.Clusters, rdsCluster{ID: d.ID, Status: "available", Engine: d.Engine, Endpoint: d.Host, Port: d.Port})
		}
	}
	return resp
}

func writeXML(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "text/xml")
	_, _ = w.Write([]byte(xml.Header))
	if err := xml.NewEncoder(w).Encode(v); err != nil {
		log.Printf("encode xml: %v", err)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	if w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", "application/json")
	}
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("encode json: %v", err)
	}
}
