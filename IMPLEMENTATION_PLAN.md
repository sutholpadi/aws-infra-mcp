# AWS Inventory and Cost MCP Server — Implementation Plan

## Goal

Build a Go MCP server that lets a user inspect AWS resource inventory and analyze account costs through natural-language requests. The first release should be read-only, scoped to the AWS credentials and account the user configures, and useful without requiring a database or long-running service.

## MVP scope

- Run as an MCP server over **stdio** for local MCP clients.
- Use the AWS SDK for Go v2 and its standard credential provider chain (environment, shared config/profile, and supported workload credentials). Never accept or log raw access keys through MCP tool arguments.
- Make AWS account and region scope explicit. Support a configured default region and optional region selection where a service is regional. Show the resolved account ID and region in tool results.
- Provide read-only inventory for:
  - EC2 instances
  - S3 buckets (bucket metadata only; do not enumerate object contents)
  - RDS DB instances
  - Lambda functions
  - SQS queues
- Provide cost analysis using AWS Cost Explorer, including total cost for a date range and breakdowns by service and/or linked account where available.
- Return structured, bounded results with pagination and clear errors. Do not silently truncate results.
- Do not create, modify, or delete AWS resources.

## Proposed architecture

1. **MCP transport and server**
   - Initialize the Go MCP SDK server and register named tools with descriptions, input schemas, and structured outputs.
   - Keep stdio protocol output free of application logs; send diagnostic logs to stderr.
2. **AWS client setup**
   - Load SDK configuration once at startup using the standard credential chain.
   - Create narrowly scoped service clients from that configuration.
   - Resolve and retain the caller account ID for result context and account validation.
3. **Tool handlers**
   - Validate arguments and apply safe defaults and maximum page/result limits.
   - Call AWS APIs with request contexts, handle pagination, and map SDK responses into stable response types.
   - Return actionable, non-sensitive errors, distinguishing invalid input, missing permissions, throttling, and service failures.
4. **Configuration**
   - Keep configuration small: default region, optional allowed regions, and result limits.
   - Prefer environment/config-file settings over tool arguments for server policy. Do not add a credential store.

## Initial MCP tool set

| Tool | Purpose | Key inputs |
|---|---|---|
| `aws_account_info` | Identify the active AWS account and configured scope | None |
| `list_ec2_instances` | List EC2 instances and selected metadata | Region; optional state and tag filters |
| `list_s3_buckets` | List bucket names and bucket-level metadata | Optional region/filter if supported by the selected API |
| `list_rds_instances` | List RDS DB instances and selected metadata | Region; optional engine/status filters |
| `list_lambda_functions` | List Lambda functions and selected metadata | Region; optional runtime filter |
| `list_sqs_queues` | List SQS queue URLs in a region | Region |
| `get_cost_summary` | Query total unblended cost for a period | Start date, end date, granularity, optional group-by |
| `get_cost_by_service` | Compare cost across AWS services for a period | Start date, end date, optional top-N limit |

Tool schemas should make date formats, inclusive/exclusive boundaries, supported granularity, regional scope, and maximum result counts explicit. Confirm Cost Explorer's date-boundary semantics in implementation and document them in tool descriptions.

## Delivery milestones

### 1. Bootstrap and protocol

- Initialize the Go module and select maintained MCP and AWS SDK for Go v2 dependencies.
- Add server startup, stdio transport, signal/context handling, and structured logging to stderr.
- Register a health/account identity tool and verify a local MCP client can discover and invoke it.

**Exit criteria:** server builds and starts; protocol discovery succeeds; startup failures are reported clearly.

### 2. AWS identity and inventory

- Load AWS configuration and resolve caller identity.
- Implement the inventory tools with input validation, pagination, bounded output, and context cancellation.
- Keep returned fields useful for inventory analysis while excluding secrets and unnecessary sensitive details.
- Document the minimum IAM read permissions for each tool.

**Exit criteria:** each tool works against a test AWS account or mocked clients, returns stable structured data, and reports missing permissions clearly.

### 3. Cost analysis

- Add Cost Explorer client integration and date-range validation.
- Implement total cost and service breakdown tools, including pagination where applicable and consistent currency/unit fields.
- Explain data freshness and that Cost Explorer availability or use may incur AWS charges.

**Exit criteria:** tests cover valid and invalid ranges, empty results, multiple pages, and AWS API errors; sample responses match the documented output shape.

### 4. Hardening and release documentation

- Add end-to-end MCP tool tests using mocked AWS clients and transport-level tests for discovery and invocation.
- Add a least-privilege IAM policy example and setup instructions for profiles, regions, and MCP clients.
- Verify no credentials or sensitive request data appear in logs; verify all tools remain read-only.
- Add build, test, and lint checks to CI once the repository's build conventions are established.

**Exit criteria:** documented setup works from a clean checkout, tests pass, and the server fails safely when credentials, permissions, or AWS services are unavailable.

## Security and reliability requirements

- Require read-only IAM permissions; do not include mutation APIs in the initial release.
- Use the AWS SDK credential chain and respect its normal credential resolution behavior. Never print credential material.
- Allow users to constrain regions through server configuration; reject unsupported or disallowed regions rather than silently broadening scope.
- Validate dates, filters, and limits before making AWS requests. Apply pagination and output bounds to avoid unbounded responses.
- Use request contexts and surface AWS errors without exposing sensitive internals.
- Avoid persistent storage in the MVP. If caching is added later, define its lifetime and invalidation behavior explicitly.

## Testing strategy

- Unit tests for argument validation, response mapping, pagination, and cost/date handling.
- AWS client tests using the repository's chosen mocking approach; never require live AWS credentials for the default test suite.
- MCP transport tests for tool registration, schemas, invocation, and error responses.
- Optional manual smoke test against a dedicated, least-privilege AWS test account.
- Build and run static checks using the Go version and tools selected when the module is initialized.

## Decisions to confirm during implementation

- Which maintained Go MCP SDK and protocol version to adopt.
- Whether initial inventory should include additional services (for example, EBS, VPC, or ECS) beyond the MVP list.
- Whether cross-account role assumption is needed in the first release or can follow as a separately configured enhancement.
- Whether cost reporting should include only unblended cost initially or also support amortized/net cost metrics.
