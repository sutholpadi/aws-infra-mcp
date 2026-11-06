package main

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/uttamtholpadi/aws-infra-mcp/internal/tools"
)

func TestParseOptions(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		want    cliOptions
		wantErr bool
	}{
		{
			name: "defaults to stdio",
			want: cliOptions{transport: "stdio", httpAddr: "127.0.0.1:8080"},
		},
		{
			name: "http transport and custom address",
			args: []string{"-transport", "http", "-http-addr", ":9000"},
			want: cliOptions{transport: "http", httpAddr: ":9000"},
		},
		{
			name:    "unsupported transport",
			args:    []string{"-transport", "sse"},
			wantErr: true,
		},
		{
			name:    "empty HTTP address",
			args:    []string{"-transport", "http", "-http-addr", " "},
			wantErr: true,
		},
		{
			name:    "unexpected positional argument",
			args:    []string{"extra"},
			wantErr: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := parseOptions(test.args)
			if (err != nil) != test.wantErr {
				t.Fatalf("parseOptions(%v) error = %v, wantErr %v", test.args, err, test.wantErr)
			}
			if err == nil && got != test.want {
				t.Errorf("parseOptions(%v) = %#v, want %#v", test.args, got, test.want)
			}
		})
	}
}

func TestRunHTTPServer(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve HTTP address: %v", err)
	}
	addr := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("release reserved HTTP address: %v", err)
	}

	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "test"}, nil)
	tools.Register(server, tools.Config{})
	ctx, cancel := context.WithCancel(context.Background())
	serverErr := make(chan error, 1)
	go func() {
		serverErr <- runHTTPServer(ctx, server, addr)
	}()

	endpoint := "http://" + addr + "/mcp"
	deadline := time.Now().Add(5 * time.Second)
	for {
		conn, dialErr := net.DialTimeout("tcp", addr, 50*time.Millisecond)
		if dialErr == nil {
			_ = conn.Close()
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("HTTP server did not start listening: %v", dialErr)
		}
		time.Sleep(10 * time.Millisecond)
	}

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "test"}, nil)
	session, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint:             endpoint,
		DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		cancel()
		t.Fatalf("connect to MCP HTTP endpoint: %v", err)
	}
	result, err := session.ListTools(context.Background(), nil)
	if err != nil {
		_ = session.Close()
		cancel()
		t.Fatalf("list tools over MCP HTTP: %v", err)
	}
	_ = session.Close()

	if len(result.Tools) != 8 {
		cancel()
		t.Fatalf("HTTP MCP server exposed %d tools, want 8", len(result.Tools))
	}

	cancel()
	select {
	case err := <-serverErr:
		if err != nil {
			t.Fatalf("runHTTPServer() after shutdown = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("HTTP server did not shut down after context cancellation")
	}
}

func TestRunHTTPServerReportsListenError(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve HTTP address: %v", err)
	}
	defer listener.Close()

	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "test"}, nil)
	err = runHTTPServer(context.Background(), server, listener.Addr().String())
	if err == nil {
		t.Fatal("runHTTPServer() succeeded with an address that is already in use")
	}
}
