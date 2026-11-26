package driver

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"strings"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc"
)

// Listen opens the endpoint the CO calls the driver at: `unix:///path` (a
// stale socket file there is removed first) or `tcp://host:port`.
func Listen(endpoint string) (net.Listener, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, fmt.Errorf("endpoint %q: %w", endpoint, err)
	}
	switch u.Scheme {
	case "unix":
		path := u.Path
		if u.Host != "" {
			path = u.Host + path
		}
		if path == "" {
			return nil, fmt.Errorf("endpoint %q names no socket path", endpoint)
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return nil, fmt.Errorf("removing the stale socket %s: %w", path, err)
		}
		return net.Listen("unix", path)
	case "tcp":
		return net.Listen("tcp", u.Host)
	}
	return nil, fmt.Errorf("endpoint %q: the scheme must be unix or tcp", endpoint)
}

// Serve serves the controller plugin's Identity and Controller services on
// l until ctx ends, then stops gracefully.
func (d *Driver) Serve(ctx context.Context, l net.Listener) error {
	return serve(ctx, l, func(srv *grpc.Server) {
		csi.RegisterIdentityServer(srv, d)
		csi.RegisterControllerServer(srv, d)
	})
}

// Serve serves the node plugin's Identity and Node services on l until ctx
// ends, then stops gracefully.
func (n *Node) Serve(ctx context.Context, l net.Listener) error {
	return serve(ctx, l, func(srv *grpc.Server) {
		csi.RegisterIdentityServer(srv, n)
		csi.RegisterNodeServer(srv, n)
	})
}

func serve(ctx context.Context, l net.Listener, register func(*grpc.Server)) error {
	srv := grpc.NewServer(grpc.UnaryInterceptor(logCalls))
	register(srv)
	stopped := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			srv.GracefulStop()
		case <-stopped:
		}
	}()
	defer close(stopped)
	return srv.Serve(l)
}

// logCalls logs every call that changes something, and every failure.
func logCalls(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	resp, err := handler(ctx, req)
	method := info.FullMethod[strings.LastIndex(info.FullMethod, "/")+1:]
	switch {
	case err != nil:
		slog.WarnContext(ctx, "call failed", "method", method, "error", err)
	case !strings.HasPrefix(method, "Get") && !strings.HasPrefix(method, "NodeGet") && method != "Probe" && !strings.HasSuffix(method, "Capabilities"):
		slog.InfoContext(ctx, "call", "method", method)
	}
	return resp, err
}
