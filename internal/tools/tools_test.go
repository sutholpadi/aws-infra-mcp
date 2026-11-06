package tools

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	lambdatypes "github.com/aws/aws-sdk-go-v2/service/lambda/types"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestRegister(t *testing.T) {
	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "test"}, nil)
	Register(server, Config{})
}

func TestNormalizeLimit(t *testing.T) {
	tests := []struct {
		name    string
		input   int32
		want    int
		wantErr bool
	}{
		{name: "default", input: 0, want: defaultLimit},
		{name: "minimum", input: 1, want: 1},
		{name: "maximum", input: maxLimit, want: maxLimit},
		{name: "negative", input: -1, wantErr: true},
		{name: "too large", input: maxLimit + 1, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := normalizeLimit(test.input)
			if (err != nil) != test.wantErr {
				t.Fatalf("normalizeLimit(%d) error = %v, wantErr %v", test.input, err, test.wantErr)
			}
			if err == nil && got != test.want {
				t.Fatalf("normalizeLimit(%d) = %d, want %d", test.input, got, test.want)
			}
		})
	}
}

func TestResolveRegion(t *testing.T) {
	service := Service{config: Config{
		AWS:            aws.Config{Region: "us-west-2"},
		AllowedRegions: map[string]bool{"us-west-2": true, "us-east-1": true},
	}}
	tests := []struct {
		name      string
		requested string
		want      string
		wantErr   bool
	}{
		{name: "configured default", want: "us-west-2"},
		{name: "allowed override", requested: "us-east-1", want: "us-east-1"},
		{name: "disallowed override", requested: "eu-west-1", wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := service.resolveRegion(test.requested)
			if (err != nil) != test.wantErr {
				t.Fatalf("resolveRegion(%q) error = %v, wantErr %v", test.requested, err, test.wantErr)
			}
			if err == nil && got != test.want {
				t.Fatalf("resolveRegion(%q) = %q, want %q", test.requested, got, test.want)
			}
		})
	}
}

func TestValidateDateRange(t *testing.T) {
	today := time.Now().UTC().Truncate(24 * time.Hour).Format("2006-01-02")
	tests := []struct {
		name    string
		start   string
		end     string
		wantErr bool
	}{
		{name: "valid", start: "2025-01-01", end: "2025-02-01"},
		{name: "invalid date", start: "2025-02-30", end: "2025-03-01", wantErr: true},
		{name: "not increasing", start: "2025-02-01", end: "2025-02-01", wantErr: true},
		{name: "future end", start: "2025-01-01", end: time.Now().UTC().AddDate(0, 0, 2).Format("2006-01-02"), wantErr: true},
		{name: "over maximum range", start: "2010-01-01", end: "2025-01-01", wantErr: true},
		{name: "today as exclusive end", start: "2025-01-01", end: today},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, _, err := validateDateRange(test.start, test.end)
			if (err != nil) != test.wantErr {
				t.Fatalf("validateDateRange(%q, %q) error = %v, wantErr %v", test.start, test.end, err, test.wantErr)
			}
		})
	}
}

func TestAddDecimalStrings(t *testing.T) {
	tests := []struct {
		left, right, want string
	}{
		{left: "0.1", right: "0.2", want: "0.3"},
		{left: "12.345", right: "0.655", want: "13"},
		{left: "-1.25", right: "2.5", want: "1.25"},
		{left: "", right: "0.75", want: "0.75"},
		{left: "1e-3", right: "2e-3", want: "0.003"},
	}
	for _, test := range tests {
		got, err := addDecimalStrings(test.left, test.right)
		if err != nil {
			t.Fatalf("addDecimalStrings(%q, %q) error = %v", test.left, test.right, err)
		}
		if got != test.want {
			t.Errorf("addDecimalStrings(%q, %q) = %q, want %q", test.left, test.right, got, test.want)
		}
	}
}

func TestAccountInfoAndMappingHelpers(t *testing.T) {
	service := Service{config: Config{AWS: aws.Config{Region: "us-west-2"}, AccountID: "123456789012"}}
	result, data, err := service.accountInfo(context.Background(), nil, EmptyInput{})
	if err != nil {
		t.Fatalf("accountInfo() err = %v", err)
	}
	if result != nil {
		t.Fatalf("accountInfo() result = %#v, want nil", result)
	}
	if data.AccountID != "123456789012" || data.DefaultRegion != "us-west-2" {
		t.Fatalf("accountInfo() = %#v, want account_id 123456789012 and default_region us-west-2", data)
	}

	instance := mapEC2Instance(ec2typesInstance())
	if instance.InstanceID != "i-123" || instance.State != "running" || instance.InstanceType != "t3.micro" || instance.AvailabilityZone != "us-east-1a" {
		t.Fatalf("mapEC2Instance() = %#v", instance)
	}
	if instance.Tags["env"] != "prod" {
		t.Fatalf("mapEC2Instance() tags = %#v, want env=prod", instance.Tags)
	}

	function := mapLambdaFunction(lambdaFunctionConfig())
	if function.Name != "demo" || function.Runtime != "nodejs20.x" || function.MemoryMB != 128 || function.TimeoutSecs != 30 {
		t.Fatalf("mapLambdaFunction() = %#v", function)
	}
	if function.LastModified == "" {
		t.Fatal("mapLambdaFunction() LastModified was empty")
	}
}

func TestListS3BucketsWithMockServer(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/" {
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/xml")
		_, _ = io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?>
		<ListAllMyBucketsResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/">
		  <Buckets>
		    <Bucket>
		      <Name>demo-bucket</Name>
		      <CreationDate>2024-01-02T03:04:05.000Z</CreationDate>
		    </Bucket>
		  </Buckets>
		</ListAllMyBucketsResult>`)
	}))
	defer server.Close()

	service := Service{config: Config{AccountID: "111122223333", AWS: aws.Config{Region: "us-east-1", Credentials: credentials.NewStaticCredentialsProvider("AKID", "SECRET", ""), BaseEndpoint: aws.String(server.URL), HTTPClient: server.Client()}}}
	_, output, err := service.listS3Buckets(context.Background(), nil, LimitInput{Limit: 10})
	if err != nil {
		t.Fatalf("listS3Buckets() err = %v", err)
	}
	if output.AccountID != "111122223333" || len(output.Buckets) != 1 || output.Buckets[0].Name != "demo-bucket" {
		t.Fatalf("listS3Buckets() = %#v", output)
	}
}

func TestListSQSQueuesWithMockServer(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "unexpected method", http.StatusBadRequest)
			return
		}
		if r.Header.Get("X-Amz-Target") != "AmazonSQS.ListQueues" {
			http.Error(w, "unexpected action", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/x-amz-json-1.0")
		_, _ = io.WriteString(w, `{"QueueUrls":["https://sqs.us-west-2.amazonaws.com/111122223333/demo-queue"]}`)
	}))
	defer server.Close()

	service := Service{config: Config{
		AccountID:      "111122223333",
		AWS:            aws.Config{Region: "us-east-1", Credentials: credentials.NewStaticCredentialsProvider("AKID", "SECRET", ""), BaseEndpoint: aws.String(server.URL), HTTPClient: server.Client()},
		AllowedRegions: map[string]bool{"us-east-1": true, "us-west-2": true},
	}}
	_, output, err := service.listSQSQueues(context.Background(), nil, SQSInput{Region: "us-west-2", Limit: 10})
	if err != nil {
		t.Fatalf("listSQSQueues() err = %v", err)
	}
	if output.AccountID != "111122223333" || output.Region != "us-west-2" || len(output.Queues) != 1 || output.Queues[0] != "https://sqs.us-west-2.amazonaws.com/111122223333/demo-queue" {
		t.Fatalf("listSQSQueues() = %#v", output)
	}
}

func TestListEC2InstancesAndLambdaFunctionsWithMockServers(t *testing.T) {
	t.Run("ec2", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				http.Error(w, "unexpected method", http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "text/xml")
			_, _ = io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?>
			<DescribeInstancesResponse xmlns="http://ec2.amazonaws.com/doc/2016-11-15/">
			  <requestId>req-1</requestId>
			  <reservationSet>
			    <item>
			      <instancesSet>
			        <item>
			          <instanceId>i-123</instanceId>
			          <instanceType>t3.micro</instanceType>
			          <state><name>running</name></state>
			          <placement><availabilityZone>us-east-1a</availabilityZone></placement>
			          <tagSet>
			            <item><key>env</key><value>prod</value></item>
			          </tagSet>
			        </item>
			      </instancesSet>
			    </item>
			  </reservationSet>
			</DescribeInstancesResponse>`)
		}))
		defer server.Close()

		service := Service{config: Config{AccountID: "111122223333", AWS: aws.Config{Region: "us-east-1", Credentials: credentials.NewStaticCredentialsProvider("AKID", "SECRET", ""), BaseEndpoint: aws.String(server.URL), HTTPClient: server.Client()}}}
		_, output, err := service.listEC2Instances(context.Background(), nil, EC2Input{Limit: 10})
		if err != nil {
			t.Fatalf("listEC2Instances() err = %v", err)
		}
		if len(output.Instances) != 1 || output.Instances[0].InstanceID != "i-123" || output.Instances[0].Tags["env"] != "prod" {
			t.Fatalf("listEC2Instances() = %#v", output)
		}
	})

	t.Run("lambda", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet || r.URL.Path != "/2015-03-31/functions" {
				http.Error(w, "unexpected request", http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"Functions":[{"FunctionName":"demo","FunctionArn":"arn:aws:lambda:us-east-1:111122223333:function:demo","Runtime":"nodejs20.x","MemorySize":128,"Timeout":30,"LastModified":"2024-01-02T03:04:05.000+0000"}]}`)
		}))
		defer server.Close()

		service := Service{config: Config{AccountID: "111122223333", AWS: aws.Config{Region: "us-east-1", Credentials: credentials.NewStaticCredentialsProvider("AKID", "SECRET", ""), BaseEndpoint: aws.String(server.URL), HTTPClient: server.Client()}}}
		_, output, err := service.listLambdaFunctions(context.Background(), nil, LambdaInput{Limit: 25})
		if err != nil {
			t.Fatalf("listLambdaFunctions() err = %v", err)
		}
		if len(output.Functions) != 1 || output.Functions[0].Name != "demo" || output.Functions[0].Runtime != "nodejs20.x" {
			t.Fatalf("listLambdaFunctions() = %#v", output)
		}
	})
}

func TestListRDSAndCostSummaryWithMockServers(t *testing.T) {
	t.Run("rds", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				http.Error(w, "unexpected method", http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "text/xml")
			_, _ = io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?>
			<DescribeDBInstancesResponse xmlns="http://rds.amazonaws.com/doc/2014-10-31/">
			  <DescribeDBInstancesResult>
			    <DBInstances>
			      <DBInstance>
			        <DBInstanceIdentifier>demo-db</DBInstanceIdentifier>
			        <Engine>postgres</Engine>
			        <EngineVersion>16.1</EngineVersion>
			        <DBInstanceStatus>available</DBInstanceStatus>
			        <DBInstanceClass>db.t3.micro</DBInstanceClass>
			        <AvailabilityZone>us-east-1a</AvailabilityZone>
			        <Endpoint>
			          <Address>demo-db.abc.us-east-1.rds.amazonaws.com</Address>
			          <Port>5432</Port>
			        </Endpoint>
			      </DBInstance>
			    </DBInstances>
			  </DescribeDBInstancesResult>
			</DescribeDBInstancesResponse>`)
		}))
		defer server.Close()

		service := Service{config: Config{AccountID: "111122223333", AWS: aws.Config{Region: "us-east-1", Credentials: credentials.NewStaticCredentialsProvider("AKID", "SECRET", ""), BaseEndpoint: aws.String(server.URL), HTTPClient: server.Client()}}}
		_, output, err := service.listRDSInstances(context.Background(), nil, RDSInput{Limit: 10})
		if err != nil {
			t.Fatalf("listRDSInstances() err = %v", err)
		}
		if len(output.Instances) != 1 || output.Instances[0].Identifier != "demo-db" || output.Instances[0].Port != 5432 {
			t.Fatalf("listRDSInstances() = %#v", output)
		}
	})

	t.Run("cost summary", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				http.Error(w, "unexpected method", http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "application/x-amz-json-1.1")
			_, _ = io.WriteString(w, `{"ResultsByTime":[{"TimePeriod":{"Start":"2024-01-01","End":"2024-02-01"},"Total":{"UnblendedCost":{"Amount":"2.50","Unit":"USD"}},"Groups":[]}]}`)
		}))
		defer server.Close()

		service := Service{config: Config{AccountID: "111122223333", AWS: aws.Config{Region: "us-east-1", Credentials: credentials.NewStaticCredentialsProvider("AKID", "SECRET", ""), BaseEndpoint: aws.String(server.URL), HTTPClient: server.Client()}}}
		_, output, err := service.getCostSummary(context.Background(), nil, CostInput{Start: "2024-01-01", End: "2024-01-15"})
		if err != nil {
			t.Fatalf("getCostSummary() err = %v", err)
		}
		if output.Granularity != "MONTHLY" || len(output.Results) != 1 || output.Results[0].Cost.Amount != "2.50" {
			t.Fatalf("getCostSummary() = %#v", output)
		}
	})
}

func TestCostByServiceAndHelpers(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-amz-json-1.1")
		_, _ = io.WriteString(w, `{"ResultsByTime":[{"TimePeriod":{"Start":"2024-01-01","End":"2024-02-01"},"Total":{"UnblendedCost":{"Amount":"4.00","Unit":"USD"}},"Groups":[{"Keys":["AmazonS3"],"Metrics":{"UnblendedCost":{"Amount":"1.50","Unit":"USD"}}},{"Keys":["AmazonEC2"],"Metrics":{"UnblendedCost":{"Amount":"2.50","Unit":"USD"}}}]}]}`)
	}))
	defer server.Close()

	service := Service{config: Config{AccountID: "111122223333", AWS: aws.Config{Region: "us-east-1", Credentials: credentials.NewStaticCredentialsProvider("AKID", "SECRET", ""), BaseEndpoint: aws.String(server.URL), HTTPClient: server.Client()}}}
	_, output, err := service.getCostByService(context.Background(), nil, CostServiceInput{Start: "2024-01-01", End: "2024-01-31", Limit: 1})
	if err != nil {
		t.Fatalf("getCostByService() err = %v", err)
	}
	if len(output.Services) != 1 || output.Services[0].Service != "AmazonEC2" || output.Services[0].Cost.Amount != "2.50" {
		t.Fatalf("getCostByService() = %#v", output)
	}

	if awsError("Service", context.DeadlineExceeded) == nil {
		t.Fatal("awsError() returned nil for a real error")
	}
	if got, err := decimalScale("1.23e2"); err != nil || got != 0 {
		t.Fatalf("decimalScale(\"1.23e2\") = %d, %v, want 0, nil", got, err)
	}
}

func ec2typesInstance() ec2types.Instance {
	return ec2types.Instance{
		InstanceId:   aws.String("i-123"),
		InstanceType: "t3.micro",
		State:        &ec2types.InstanceState{Name: ec2types.InstanceStateNameRunning},
		Placement:    &ec2types.Placement{AvailabilityZone: aws.String("us-east-1a")},
		Tags:         []ec2types.Tag{{Key: aws.String("env"), Value: aws.String("prod")}},
	}
}

func lambdaFunctionConfig() lambdatypes.FunctionConfiguration {
	return lambdatypes.FunctionConfiguration{
		FunctionName: aws.String("demo"),
		FunctionArn:  aws.String("arn:aws:lambda:us-east-1:111122223333:function:demo"),
		Runtime:      lambdatypes.RuntimeNodejs20x,
		MemorySize:   aws.Int32(128),
		Timeout:      aws.Int32(30),
		LastModified: aws.String("2024-01-02T03:04:05.000+0000"),
	}
}
