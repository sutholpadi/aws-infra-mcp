# AWS Inventory and Cost MCP Server

A read-only Go MCP server for querying AWS account identity, selected resource inventory, and AWS Cost Explorer.

## Requirements

- Go 1.25 or newer
- AWS credentials configured through the standard AWS SDK for Go v2 credential chain
- A default AWS region, for example `AWS_REGION=us-west-2`

The server does not accept credentials as tool arguments. It resolves AWS identity with STS when it starts and writes logs to stderr so stdout remains available for MCP stdio transport.

## Build and run

```sh
go test ./...
go build -o ./bin/aws-infra-mcp ./cmd/aws-infra-mcp
AWS_PROFILE=<aws_profile> AWS_REGION=us-west-2 ./bin/aws-infra-mcp
```

The default transport is stdio. To run the server with Streamable HTTP instead:

```sh
AWS_PROFILE=<aws_profile> AWS_REGION=us-west-2 \
  ./bin/aws-infra-mcp -transport http -http-addr 127.0.0.1:8080
```

The MCP endpoint is `http://127.0.0.1:8080/mcp`. Use `-http-addr` to choose another listen address. Keep the process running while clients connect. HTTP is unauthenticated by default; keep it bound to localhost unless you put it behind an appropriately secured TLS/authentication layer.

You can optionally restrict regional tools to an allowlist:

```sh
AWS_ALLOWED_REGIONS=us-west-2,us-east-1
```

Regional inventory tools use the server's default region unless a tool call specifies another region. When `AWS_ALLOWED_REGIONS` is set, both the default and requested regions must be on that list. S3 bucket listing is account-wide and has no region input.

## MCP tools

| Tool | Description |
|---|---|
| `aws_account_info` | Return the active account ID and default region |
| `list_ec2_instances` | List EC2 instance IDs, state, type, zone, and tags; supports state and exact-match tag filters |
| `list_s3_buckets` | List bucket names and creation dates; never lists object contents |
| `list_rds_instances` | List RDS identifiers, engine, status, class, endpoint, and zone; supports engine and status filters |
| `list_lambda_functions` | List function names, ARNs, runtime, memory, timeout, and last modified; supports runtime filtering |
| `get_cost_summary` | Return daily or monthly unblended cost; defaults to monthly |
| `get_cost_by_service` | Return monthly unblended cost grouped by service |
| `list_sqs_queues` | List SQS queue URLs in a region |

Inventory calls default to 100 results and accept a `limit` from 1 to 500. A `truncated` field indicates more matching results were available. Lambda runtime filtering is applied while paging through the regional function inventory. Cost Explorer requests use its `us-east-1` endpoint; the inventory default region does not change cost-report scope. Cost dates use `YYYY-MM-DD`; the start is inclusive and the end is exclusive. Cost queries are limited to 366 days for daily granularity and 3,660 days for monthly granularity. Cost Explorer data availability can lag and AWS may charge for use of billing APIs.

## MCP client configuration

For stdio clients, configure the client to launch the binary:

```json
{
  "mcpServers": {
    "aws-infra": {
      "command": "/absolute/path/to/aws-infra-mcp",
      "args": [],
      "env": {
        "AWS_PROFILE": "<aws_profile>",
        "AWS_REGION": "us-west-2",
        "AWS_ALLOWED_REGIONS": "us-west-2,us-east-1"
      }
    }
  }
}
```

Use the credential mechanism supported by your environment (for example, an AWS profile, environment credentials, or workload identity). Do not put long-lived credentials into this configuration.

For HTTP clients, start the server separately using the HTTP command above, then configure the client to connect to its endpoint:

```json
{
  "mcpServers": {
    "aws-infra-http": {
      "type": "http",
      "url": "http://127.0.0.1:8080/mcp"
    }
  }
}
```

The HTTP client configuration connects to the running server; it does not launch it. Startup errors and the listening endpoint are logged to the terminal where the server process is running.

## IAM permissions

The following policy is a starting point for the tools included here. It uses `Resource: "*"` because the listed read APIs generally do not support resource-level permissions. Review the policy against your organization’s IAM standards before deployment.

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": [
        "sts:GetCallerIdentity",
        "ec2:DescribeInstances",
        "s3:ListAllMyBuckets",
        "rds:DescribeDBInstances",
        "lambda:ListFunctions",
        "sqs:ListQueues",
        "ce:GetCostAndUsage"
      ],
      "Resource": "*"
    }
  ]
}
```

If an operation returns `AccessDenied`, check that the corresponding action is granted and that any permissions boundary, session policy, SCP, or billing-access setting also permits it.

## Development

```sh
go test ./...
go vet ./...
```

Default tests do not contact AWS or require credentials. For an integration smoke test, use a dedicated account or role with only the permissions above.
