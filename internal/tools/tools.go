package tools

import (
	"context"
	"fmt"
	"math/big"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/costexplorer"
	cetypes "github.com/aws/aws-sdk-go-v2/service/costexplorer/types"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
	lambdatypes "github.com/aws/aws-sdk-go-v2/service/lambda/types"
	"github.com/aws/aws-sdk-go-v2/service/rds"
	rdstypes "github.com/aws/aws-sdk-go-v2/service/rds/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	defaultLimit = 100
	maxLimit     = 500
)

type Config struct {
	AWS            aws.Config
	AccountID      string
	AllowedRegions map[string]bool
}

type Service struct {
	config Config
}

func Register(server *mcp.Server, cfg Config) {
	service := &Service{config: cfg}

	mcp.AddTool(server, &mcp.Tool{
		Name:        "aws_account_info",
		Description: "Return the AWS account and configured default region used by this server.",
	}, service.accountInfo)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "list_ec2_instances",
		Description: "List EC2 instances in a region, with optional state and exact-match tag filters. Results are limited to 500.",
	}, service.listEC2Instances)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "list_s3_buckets",
		Description: "List S3 bucket names and creation dates. This account-wide operation does not enumerate objects.",
	}, service.listS3Buckets)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "list_rds_instances",
		Description: "List RDS DB instances in a region, with optional engine and status filters. Results are limited to 500.",
	}, service.listRDSInstances)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "list_lambda_functions",
		Description: "List Lambda functions in a region, optionally filtered by runtime. Results are limited to 500.",
	}, service.listLambdaFunctions)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_cost_summary",
		Description: "Return unblended cost for an AWS Cost Explorer date range. Start is inclusive and end is exclusive (YYYY-MM-DD); DAILY ranges are limited to 366 days.",
	}, service.getCostSummary)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_cost_by_service",
		Description: "Break down unblended cost by AWS service for a date range. Start is inclusive and end is exclusive (YYYY-MM-DD).",
	}, service.getCostByService)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "list_sqs_queues",
		Description: "List SQS queue URLs in a region. Results are limited to 500.",
	}, service.listSQSQueues)
}

type EmptyInput struct{}

type AccountInfo struct {
	AccountID     string `json:"account_id"`
	DefaultRegion string `json:"default_region"`
}

func (s *Service) accountInfo(context.Context, *mcp.CallToolRequest, EmptyInput) (*mcp.CallToolResult, AccountInfo, error) {
	return nil, AccountInfo{AccountID: s.config.AccountID, DefaultRegion: s.config.AWS.Region}, nil
}

type EC2Input struct {
	Region string            `json:"region,omitempty" jsonschema:"AWS region; defaults to the server's configured region"`
	Limit  int32             `json:"limit,omitempty" jsonschema:"Maximum number of results (default 100, maximum 500)"`
	State  string            `json:"state,omitempty" jsonschema:"Optional EC2 instance state filter"`
	Tags   map[string]string `json:"tags,omitempty" jsonschema:"Optional exact-match tag filters"`
}

type Instance struct {
	InstanceID       string            `json:"instance_id"`
	State            string            `json:"state"`
	InstanceType     string            `json:"instance_type"`
	AvailabilityZone string            `json:"availability_zone,omitempty"`
	Tags             map[string]string `json:"tags,omitempty"`
}

type EC2InstancesOutput struct {
	AccountID string     `json:"account_id"`
	Region    string     `json:"region"`
	Instances []Instance `json:"instances"`
	Truncated bool       `json:"truncated"`
}

func (s *Service) listEC2Instances(ctx context.Context, _ *mcp.CallToolRequest, input EC2Input) (*mcp.CallToolResult, EC2InstancesOutput, error) {
	region, err := s.resolveRegion(input.Region)
	if err != nil {
		return nil, EC2InstancesOutput{}, err
	}
	limit, err := normalizeLimit(input.Limit)
	if err != nil {
		return nil, EC2InstancesOutput{}, err
	}
	client := ec2.NewFromConfig(s.config.AWS, func(options *ec2.Options) { options.Region = region })
	filters := make([]ec2types.Filter, 0, len(input.Tags)+1)
	if input.State != "" {
		filters = append(filters, ec2types.Filter{
			Name:   aws.String("instance-state-name"),
			Values: []string{input.State},
		})
	}
	for key, value := range input.Tags {
		if key == "" {
			return nil, EC2InstancesOutput{}, fmt.Errorf("tag filter keys cannot be empty")
		}
		filters = append(filters, ec2types.Filter{
			Name:   aws.String("tag:" + key),
			Values: []string{value},
		})
	}
	paginator := ec2.NewDescribeInstancesPaginator(client, &ec2.DescribeInstancesInput{
		MaxResults: aws.Int32(1000),
		Filters:    filters,
	})
	result := EC2InstancesOutput{AccountID: s.config.AccountID, Region: region, Instances: make([]Instance, 0)}
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, EC2InstancesOutput{}, awsError("EC2", err)
		}
		for reservationIndex, reservation := range page.Reservations {
			for instanceIndex, item := range reservation.Instances {
				result.Instances = append(result.Instances, mapEC2Instance(item))
				if len(result.Instances) == limit {
					result.Truncated = paginator.HasMorePages() ||
						instanceIndex < len(reservation.Instances)-1 ||
						reservationIndex < len(page.Reservations)-1
					return nil, result, nil
				}
			}
		}
	}
	return nil, result, nil
}

func mapEC2Instance(item ec2types.Instance) Instance {
	out := Instance{
		InstanceID:   aws.ToString(item.InstanceId),
		InstanceType: string(item.InstanceType),
	}
	if item.State != nil {
		out.State = string(item.State.Name)
	}
	if item.Placement != nil {
		out.AvailabilityZone = aws.ToString(item.Placement.AvailabilityZone)
	}
	if len(item.Tags) > 0 {
		out.Tags = make(map[string]string, len(item.Tags))
		for _, tag := range item.Tags {
			out.Tags[aws.ToString(tag.Key)] = aws.ToString(tag.Value)
		}
	}
	return out
}

type Bucket struct {
	Name      string `json:"name"`
	CreatedAt string `json:"created_at,omitempty"`
}

type S3BucketsOutput struct {
	AccountID string   `json:"account_id"`
	Buckets   []Bucket `json:"buckets"`
	Truncated bool     `json:"truncated"`
}

type LimitInput struct {
	Limit int32 `json:"limit,omitempty" jsonschema:"Maximum number of results (default 100, maximum 500)"`
}

func (s *Service) listS3Buckets(ctx context.Context, _ *mcp.CallToolRequest, input LimitInput) (*mcp.CallToolResult, S3BucketsOutput, error) {
	limit, err := normalizeLimit(input.Limit)
	if err != nil {
		return nil, S3BucketsOutput{}, err
	}
	client := s3.NewFromConfig(s.config.AWS)
	paginator := s3.NewListBucketsPaginator(client, &s3.ListBucketsInput{
		MaxBuckets: aws.Int32(int32(limit)),
	}, func(options *s3.ListBucketsPaginatorOptions) { options.Limit = int32(limit) })
	result := S3BucketsOutput{AccountID: s.config.AccountID, Buckets: make([]Bucket, 0)}
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, S3BucketsOutput{}, awsError("S3", err)
		}
		for bucketPosition, item := range page.Buckets {
			bucket := Bucket{Name: aws.ToString(item.Name)}
			if item.CreationDate != nil {
				bucket.CreatedAt = item.CreationDate.UTC().Format(time.RFC3339)
			}
			result.Buckets = append(result.Buckets, bucket)
			if len(result.Buckets) == limit {
				result.Truncated = paginator.HasMorePages() || bucketPosition < len(page.Buckets)-1
				return nil, result, nil
			}
		}
	}
	return nil, result, nil
}

type RDSInput struct {
	Region string `json:"region,omitempty" jsonschema:"AWS region; defaults to the server's configured region"`
	Limit  int32  `json:"limit,omitempty" jsonschema:"Maximum number of results (default 100, maximum 500)"`
	Engine string `json:"engine,omitempty" jsonschema:"Optional exact-match database engine filter"`
	Status string `json:"status,omitempty" jsonschema:"Optional exact-match DB instance status filter"`
}

type RDSInstance struct {
	Identifier       string `json:"identifier"`
	Engine           string `json:"engine"`
	EngineVersion    string `json:"engine_version"`
	Status           string `json:"status"`
	Class            string `json:"class"`
	Endpoint         string `json:"endpoint,omitempty"`
	Port             int32  `json:"port,omitempty"`
	AvailabilityZone string `json:"availability_zone,omitempty"`
}

type RDSInstancesOutput struct {
	AccountID string        `json:"account_id"`
	Region    string        `json:"region"`
	Instances []RDSInstance `json:"instances"`
	Truncated bool          `json:"truncated"`
}

func (s *Service) listRDSInstances(ctx context.Context, _ *mcp.CallToolRequest, input RDSInput) (*mcp.CallToolResult, RDSInstancesOutput, error) {
	region, err := s.resolveRegion(input.Region)
	if err != nil {
		return nil, RDSInstancesOutput{}, err
	}
	limit, err := normalizeLimit(input.Limit)
	if err != nil {
		return nil, RDSInstancesOutput{}, err
	}
	client := rds.NewFromConfig(s.config.AWS, func(options *rds.Options) { options.Region = region })
	filters := make([]rdstypes.Filter, 0, 2)
	if input.Engine != "" {
		filters = append(filters, rdstypes.Filter{Name: aws.String("engine"), Values: []string{input.Engine}})
	}
	if input.Status != "" {
		filters = append(filters, rdstypes.Filter{Name: aws.String("db-instance-status"), Values: []string{input.Status}})
	}
	paginator := rds.NewDescribeDBInstancesPaginator(client, &rds.DescribeDBInstancesInput{
		MaxRecords: aws.Int32(100),
		Filters:    filters,
	})
	result := RDSInstancesOutput{AccountID: s.config.AccountID, Region: region, Instances: make([]RDSInstance, 0)}
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, RDSInstancesOutput{}, awsError("RDS", err)
		}
		for position, item := range page.DBInstances {
			instance := RDSInstance{
				Identifier:       aws.ToString(item.DBInstanceIdentifier),
				Engine:           aws.ToString(item.Engine),
				EngineVersion:    aws.ToString(item.EngineVersion),
				Status:           aws.ToString(item.DBInstanceStatus),
				Class:            aws.ToString(item.DBInstanceClass),
				AvailabilityZone: aws.ToString(item.AvailabilityZone),
			}
			if item.Endpoint != nil {
				instance.Endpoint = aws.ToString(item.Endpoint.Address)
				instance.Port = aws.ToInt32(item.Endpoint.Port)
			}
			result.Instances = append(result.Instances, instance)
			if len(result.Instances) == limit {
				result.Truncated = paginator.HasMorePages() || position < len(page.DBInstances)-1
				return nil, result, nil
			}
		}
	}
	return nil, result, nil
}

type LambdaFunction struct {
	Name         string `json:"name"`
	ARN          string `json:"arn"`
	Runtime      string `json:"runtime"`
	MemoryMB     int32  `json:"memory_mb"`
	TimeoutSecs  int32  `json:"timeout_seconds"`
	LastModified string `json:"last_modified,omitempty"`
}

type LambdaInput struct {
	Region  string `json:"region,omitempty" jsonschema:"AWS region; defaults to the server's configured region"`
	Limit   int32  `json:"limit,omitempty" jsonschema:"Maximum number of results (default 100, maximum 500)"`
	Runtime string `json:"runtime,omitempty" jsonschema:"Optional exact-match Lambda runtime filter"`
}

type LambdaFunctionsOutput struct {
	AccountID string           `json:"account_id"`
	Region    string           `json:"region"`
	Functions []LambdaFunction `json:"functions"`
	Truncated bool             `json:"truncated"`
}

func (s *Service) listLambdaFunctions(ctx context.Context, _ *mcp.CallToolRequest, input LambdaInput) (*mcp.CallToolResult, LambdaFunctionsOutput, error) {
	region, err := s.resolveRegion(input.Region)
	if err != nil {
		return nil, LambdaFunctionsOutput{}, err
	}
	limit, err := normalizeLimit(input.Limit)
	if err != nil {
		return nil, LambdaFunctionsOutput{}, err
	}
	client := lambda.NewFromConfig(s.config.AWS, func(options *lambda.Options) { options.Region = region })
	paginator := lambda.NewListFunctionsPaginator(client, &lambda.ListFunctionsInput{
		// AWS permits at most 50 functions per page.
		//
		// The output limit is enforced separately so callers can request a smaller response.
		MaxItems: aws.Int32(50),
	}, func(options *lambda.ListFunctionsPaginatorOptions) { options.Limit = 50 })
	result := LambdaFunctionsOutput{AccountID: s.config.AccountID, Region: region, Functions: make([]LambdaFunction, 0)}
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, LambdaFunctionsOutput{}, awsError("Lambda", err)
		}
		for _, item := range page.Functions {
			if input.Runtime != "" && string(item.Runtime) != input.Runtime {
				continue
			}
			result.Functions = append(result.Functions, mapLambdaFunction(item))
			if len(result.Functions) > limit {
				result.Functions = result.Functions[:limit]
				result.Truncated = true
				return nil, result, nil
			}
		}
	}
	return nil, result, nil
}

func mapLambdaFunction(item lambdatypes.FunctionConfiguration) LambdaFunction {
	out := LambdaFunction{
		Name:        aws.ToString(item.FunctionName),
		ARN:         aws.ToString(item.FunctionArn),
		Runtime:     string(item.Runtime),
		MemoryMB:    aws.ToInt32(item.MemorySize),
		TimeoutSecs: aws.ToInt32(item.Timeout),
	}
	if item.LastModified != nil {
		out.LastModified = aws.ToString(item.LastModified)
	}
	return out
}

type CostInput struct {
	Start       string `json:"start" jsonschema:"Inclusive start date in YYYY-MM-DD format"`
	End         string `json:"end" jsonschema:"Exclusive end date in YYYY-MM-DD format"`
	Granularity string `json:"granularity,omitempty" jsonschema:"DAILY or MONTHLY; defaults to MONTHLY"`
}

type CostServiceInput struct {
	Start string `json:"start" jsonschema:"Inclusive start date in YYYY-MM-DD format"`
	End   string `json:"end" jsonschema:"Exclusive end date in YYYY-MM-DD format"`
	Limit int32  `json:"limit,omitempty" jsonschema:"Maximum number of service groups (default 100, maximum 500)"`
}

type CostAmount struct {
	Amount string `json:"amount"`
	Unit   string `json:"unit"`
}

type CostPeriod struct {
	Start string     `json:"start"`
	End   string     `json:"end"`
	Cost  CostAmount `json:"cost"`
}

type CostSummaryOutput struct {
	AccountID   string       `json:"account_id"`
	Metric      string       `json:"metric"`
	Granularity string       `json:"granularity"`
	Results     []CostPeriod `json:"results"`
}

type ServiceCost struct {
	Service string     `json:"service"`
	Cost    CostAmount `json:"cost"`
}

type CostByServiceOutput struct {
	AccountID string        `json:"account_id"`
	Start     string        `json:"start"`
	End       string        `json:"end"`
	Services  []ServiceCost `json:"services"`
	Truncated bool          `json:"truncated"`
}

func (s *Service) getCostSummary(ctx context.Context, _ *mcp.CallToolRequest, input CostInput) (*mcp.CallToolResult, CostSummaryOutput, error) {
	start, end, err := validateDateRange(input.Start, input.End)
	if err != nil {
		return nil, CostSummaryOutput{}, err
	}
	granularity := input.Granularity
	if granularity == "" {
		granularity = "MONTHLY"
	}
	if granularity != "DAILY" && granularity != "MONTHLY" {
		return nil, CostSummaryOutput{}, fmt.Errorf("granularity must be DAILY or MONTHLY")
	}
	if granularity == "DAILY" {
		startDate, _ := time.Parse("2006-01-02", start)
		endDate, _ := time.Parse("2006-01-02", end)
		if endDate.Sub(startDate) > 366*24*time.Hour {
			return nil, CostSummaryOutput{}, fmt.Errorf("DAILY cost queries cannot exceed 366 days")
		}
	}
	output, err := s.queryCosts(ctx, start, end, granularity, nil)
	if err != nil {
		return nil, CostSummaryOutput{}, err
	}
	return nil, CostSummaryOutput{
		AccountID: s.config.AccountID, Metric: "UnblendedCost", Granularity: granularity, Results: output,
	}, nil
}

func (s *Service) getCostByService(ctx context.Context, _ *mcp.CallToolRequest, input CostServiceInput) (*mcp.CallToolResult, CostByServiceOutput, error) {
	start, end, err := validateDateRange(input.Start, input.End)
	if err != nil {
		return nil, CostByServiceOutput{}, err
	}
	limit, err := normalizeLimit(input.Limit)
	if err != nil {
		return nil, CostByServiceOutput{}, err
	}
	groupBy := []cetypes.GroupDefinition{{Type: cetypes.GroupDefinitionTypeDimension, Key: aws.String("SERVICE")}}
	periods, err := s.queryCostPages(ctx, start, end, "MONTHLY", groupBy)
	if err != nil {
		return nil, CostByServiceOutput{}, err
	}
	totals := make(map[string]CostAmount)
	for _, period := range periods {
		for _, group := range period.groups {
			if _, ok := new(big.Rat).SetString(group.cost.Amount); !ok {
				return nil, CostByServiceOutput{}, fmt.Errorf("Cost Explorer returned invalid cost amount %q", group.cost.Amount)
			}
			amount, exists := totals[group.name]
			if !exists {
				amount = CostAmount{Unit: group.cost.Unit}
			}
			amount.Amount, err = addDecimalStrings(amount.Amount, group.cost.Amount)
			if err != nil {
				return nil, CostByServiceOutput{}, fmt.Errorf("invalid cost amount returned by Cost Explorer: %w", err)
			}
			totals[group.name] = amount
		}
	}
	services := make([]ServiceCost, 0, len(totals))
	for name, cost := range totals {
		services = append(services, ServiceCost{Service: name, Cost: cost})
	}
	sort.Slice(services, func(i, j int) bool {
		left, _ := new(big.Rat).SetString(services[i].Cost.Amount)
		right, _ := new(big.Rat).SetString(services[j].Cost.Amount)
		if comparison := left.Cmp(right); comparison != 0 {
			return comparison > 0
		}
		return services[i].Service < services[j].Service
	})
	truncated := len(services) > limit
	if truncated {
		services = services[:limit]
	}
	return nil, CostByServiceOutput{
		AccountID: s.config.AccountID, Start: start, End: end, Services: services, Truncated: truncated,
	}, nil
}

type costGroup struct {
	name string
	cost CostAmount
}

type costPeriodWithGroups struct {
	start  string
	end    string
	cost   CostAmount
	groups []costGroup
}

func (s *Service) queryCosts(ctx context.Context, start, end, granularity string, groupBy []cetypes.GroupDefinition) ([]CostPeriod, error) {
	periods, err := s.queryCostPages(ctx, start, end, granularity, groupBy)
	if err != nil {
		return nil, err
	}
	result := make([]CostPeriod, 0, len(periods))
	for _, item := range periods {
		result = append(result, CostPeriod{Start: item.start, End: item.end, Cost: item.cost})
	}
	return result, nil
}

type SQSInput struct {
	Region string `json:"region,omitempty" jsonschema:"AWS region; defaults to the server's configured region"`
	Limit  int32  `json:"limit,omitempty" jsonschema:"Maximum number of results (default 100, maximum 500)"`
}

type SQSQueuesOutput struct {
	AccountID string   `json:"account_id"`
	Region    string   `json:"region"`
	Queues    []string `json:"queues"`
	Truncated bool     `json:"truncated"`
}

func (s *Service) listSQSQueues(ctx context.Context, _ *mcp.CallToolRequest, input SQSInput) (*mcp.CallToolResult, SQSQueuesOutput, error) {
	region, err := s.resolveRegion(input.Region)
	if err != nil {
		return nil, SQSQueuesOutput{}, err
	}
	limit, err := normalizeLimit(input.Limit)
	if err != nil {
		return nil, SQSQueuesOutput{}, err
	}
	client := sqs.NewFromConfig(s.config.AWS, func(options *sqs.Options) { options.Region = region })
	paginator := sqs.NewListQueuesPaginator(client, &sqs.ListQueuesInput{
		MaxResults: aws.Int32(int32(limit)),
	}, func(options *sqs.ListQueuesPaginatorOptions) { options.Limit = int32(limit) })
	result := SQSQueuesOutput{
		AccountID: s.config.AccountID,
		Region:    region,
		Queues:    make([]string, 0),
	}
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, SQSQueuesOutput{}, awsError("SQS", err)
		}
		for position, queueURL := range page.QueueUrls {
			result.Queues = append(result.Queues, queueURL)
			if len(result.Queues) == limit {
				result.Truncated = paginator.HasMorePages() || position < len(page.QueueUrls)-1
				return nil, result, nil
			}
		}
	}
	return nil, result, nil
}

func (s *Service) queryCostPages(ctx context.Context, start, end, granularity string, groupBy []cetypes.GroupDefinition) ([]costPeriodWithGroups, error) {
	costConfig := s.config.AWS
	costConfig.Region = "us-east-1"
	client := costexplorer.NewFromConfig(costConfig)
	input := &costexplorer.GetCostAndUsageInput{
		TimePeriod:  &cetypes.DateInterval{Start: aws.String(start), End: aws.String(end)},
		Granularity: cetypes.Granularity(granularity),
		Metrics:     []string{"UnblendedCost"},
		GroupBy:     groupBy,
	}
	periods := make([]costPeriodWithGroups, 0)
	for {
		page, err := client.GetCostAndUsage(ctx, input)
		if err != nil {
			return nil, awsError("Cost Explorer", err)
		}
		for _, item := range page.ResultsByTime {
			period := costPeriodWithGroups{
				start: aws.ToString(item.TimePeriod.Start),
				end:   aws.ToString(item.TimePeriod.End),
				cost:  costAmount(item.Total["UnblendedCost"]),
			}
			for _, group := range item.Groups {
				name := ""
				if len(group.Keys) > 0 {
					name = group.Keys[0]
				}
				period.groups = append(period.groups, costGroup{
					name: name,
					cost: costAmount(group.Metrics["UnblendedCost"]),
				})
			}
			periods = append(periods, period)
		}
		if page.NextPageToken == nil || *page.NextPageToken == "" {
			break
		}
		input.NextPageToken = page.NextPageToken
	}
	return periods, nil
}

func costAmount(metric cetypes.MetricValue) CostAmount {
	return CostAmount{Amount: aws.ToString(metric.Amount), Unit: aws.ToString(metric.Unit)}
}

func addDecimalStrings(left, right string) (string, error) {
	if left == "" {
		return right, nil
	}
	leftValue, ok := new(big.Rat).SetString(left)
	if !ok {
		return "", fmt.Errorf("cannot parse %q", left)
	}
	rightValue, ok := new(big.Rat).SetString(right)
	if !ok {
		return "", fmt.Errorf("cannot parse %q", right)
	}
	leftScale, err := decimalScale(left)
	if err != nil {
		return "", err
	}
	rightScale, err := decimalScale(right)
	if err != nil {
		return "", err
	}
	scale := leftScale
	if rightScale > scale {
		scale = rightScale
	}
	sum := new(big.Rat).Add(leftValue, rightValue)
	result := sum.FloatString(scale)
	if strings.Contains(result, ".") {
		result = strings.TrimRight(strings.TrimRight(result, "0"), ".")
	}
	if result == "" || result == "-0" {
		result = "0"
	}
	return result, nil
}

func decimalScale(value string) (int, error) {
	exponent := 0
	if exponentIndex := strings.IndexAny(value, "eE"); exponentIndex >= 0 {
		parsedExponent, err := strconv.Atoi(value[exponentIndex+1:])
		if err != nil {
			return 0, fmt.Errorf("invalid decimal exponent in %q", value)
		}
		exponent = parsedExponent
		value = value[:exponentIndex]
	}
	scale := 0
	if dot := strings.IndexByte(value, '.'); dot >= 0 {
		scale = len(value) - dot - 1
	}
	scale -= exponent
	if scale < 0 {
		return 0, nil
	}
	return scale, nil
}

func (s *Service) resolveRegion(requested string) (string, error) {
	if requested == "" {
		requested = s.config.AWS.Region
	}
	if requested == "" {
		return "", fmt.Errorf("region is required and no default region is configured")
	}
	if len(s.config.AllowedRegions) > 0 && !s.config.AllowedRegions[requested] {
		return "", fmt.Errorf("region %q is not allowed by server configuration", requested)
	}
	return requested, nil
}

func normalizeLimit(limit int32) (int, error) {
	if limit == 0 {
		return defaultLimit, nil
	}
	if limit < 1 || limit > maxLimit {
		return 0, fmt.Errorf("limit must be between 1 and %d", maxLimit)
	}
	return int(limit), nil
}

func validateDateRange(start, end string) (string, string, error) {
	startDate, err := time.Parse("2006-01-02", start)
	if err != nil {
		return "", "", fmt.Errorf("start must be a valid YYYY-MM-DD date")
	}
	endDate, err := time.Parse("2006-01-02", end)
	if err != nil {
		return "", "", fmt.Errorf("end must be a valid YYYY-MM-DD date")
	}
	if !startDate.Before(endDate) {
		return "", "", fmt.Errorf("start must be earlier than end")
	}
	if endDate.After(time.Now().UTC().Truncate(24 * time.Hour)) {
		return "", "", fmt.Errorf("end cannot be in the future")
	}
	maxDays := 3660
	if startDate.AddDate(0, 0, maxDays).Before(endDate) {
		return "", "", fmt.Errorf("date range cannot exceed %d days", maxDays)
	}
	return start, end, nil
}

func awsError(service string, err error) error {
	return fmt.Errorf("%s request failed: %w", service, err)
}
