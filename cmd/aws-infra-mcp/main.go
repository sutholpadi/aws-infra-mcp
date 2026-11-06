package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/uttamtholpadi/aws-infra-mcp/internal/tools"
)

func main() {
	if err := run(); err != nil {
		slog.Error("server stopped", "error", err)
		os.Exit(1)
	}
}

func run() error {
	options, err := parseOptions(os.Args[1:])
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	awsConfig, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return fmt.Errorf("load AWS configuration: %w", err)
	}
	if awsConfig.Region == "" {
		return errors.New("AWS region is not configured; set AWS_REGION or configure a default region")
	}

	identity, err := sts.NewFromConfig(awsConfig).GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return fmt.Errorf("resolve AWS caller identity: %w", err)
	}
	if identity.Account == nil || *identity.Account == "" {
		return errors.New("AWS STS returned an empty account ID")
	}

	allowedRegions, err := parseAllowedRegions(os.Getenv("AWS_ALLOWED_REGIONS"))
	if err != nil {
		return err
	}
	if len(allowedRegions) > 0 && !allowedRegions[awsConfig.Region] {
		return fmt.Errorf("configured default region %q is not in AWS_ALLOWED_REGIONS", awsConfig.Region)
	}

	slog.Info("starting AWS inventory MCP server",
		"account_id", *identity.Account,
		"default_region", awsConfig.Region,
	)

	server := mcp.NewServer(&mcp.Implementation{
		Name:    "aws-infra-mcp",
		Version: "0.1.0",
	}, nil)
	tools.Register(server, tools.Config{
		AWS:            awsConfig,
		AccountID:      *identity.Account,
		AllowedRegions: allowedRegions,
	})

	switch options.transport {
	case "stdio":
		return server.Run(ctx, &mcp.StdioTransport{})
	case "http":
		return runHTTPServer(ctx, server, options.httpAddr)
	default:
		return fmt.Errorf("unsupported transport %q", options.transport)
	}
}

type cliOptions struct {
	transport string
	httpAddr  string
}

func parseOptions(args []string) (cliOptions, error) {
	options := cliOptions{}
	flags := flag.NewFlagSet("aws-infra-mcp", flag.ContinueOnError)
	flags.StringVar(&options.transport, "transport", "stdio", "MCP transport: stdio or http")
	flags.StringVar(&options.httpAddr, "http-addr", "127.0.0.1:8080", "HTTP listen address when using the http transport")
	if err := flags.Parse(args); err != nil {
		return cliOptions{}, err
	}
	if len(flags.Args()) > 0 {
		return cliOptions{}, fmt.Errorf("unexpected positional arguments: %s", strings.Join(flags.Args(), " "))
	}
	if options.transport != "stdio" && options.transport != "http" {
		return cliOptions{}, fmt.Errorf("transport must be stdio or http, got %q", options.transport)
	}
	if options.transport == "http" && strings.TrimSpace(options.httpAddr) == "" {
		return cliOptions{}, errors.New("HTTP listen address cannot be empty")
	}
	return options, nil
}

func runHTTPServer(ctx context.Context, server *mcp.Server, addr string) error {
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen for MCP HTTP requests on %q: %w", addr, err)
	}
	defer listener.Close()

	mux := http.NewServeMux()
	mux.Handle("/mcp", mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return server },
		nil,
	))
	httpServer := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	slog.Info("serving MCP over HTTP", "endpoint", "http://"+listener.Addr().String()+"/mcp")

	serveErr := make(chan error, 1)
	go func() {
		serveErr <- httpServer.Serve(listener)
	}()

	select {
	case err := <-serveErr:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("serve MCP over HTTP: %w", err)
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("shut down MCP HTTP server: %w", err)
		}
		if err := <-serveErr; err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("serve MCP over HTTP: %w", err)
		}
		return nil
	}
}

func parseAllowedRegions(value string) (map[string]bool, error) {
	regions := make(map[string]bool)
	for _, region := range strings.Split(value, ",") {
		region = strings.TrimSpace(region)
		if region == "" {
			continue
		}
		if strings.ContainsAny(region, " \t\r\n") {
			return nil, fmt.Errorf("invalid region %q in AWS_ALLOWED_REGIONS", region)
		}
		regions[region] = true
	}
	if len(regions) == 0 {
		return nil, nil
	}
	return regions, nil
}
