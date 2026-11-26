// Command origincloud-csi-driver is the Origin CSI driver,
// pvs.csi.origincloud.eu. One binary serves both plugins: --mode=controller
// runs the controller plugin (one replica per cluster, beside the CSI
// sidecars), --mode=node the node plugin on every node.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/origincloudhq/csi-driver-origincloud/internal/cloud"
	"github.com/origincloudhq/csi-driver-origincloud/internal/driver"
	"go.origincloud.eu/sdk/core"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if err := run(); err != nil {
		slog.Error("origincloud-csi-driver", "error", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		mode        = flag.String("mode", "", "the plugin to run: controller or node")
		endpoint    = flag.String("endpoint", "unix:///csi/csi.sock", "where to serve CSI: unix:///path or tcp://host:port")
		project     = flag.String("project", os.Getenv("ORIGINCLOUD_PROJECT"), "controller: the project to create volumes in (default $ORIGINCLOUD_PROJECT)")
		region      = flag.String("region", os.Getenv("ORIGINCLOUD_REGION"), "the region whose API to call (default $ORIGINCLOUD_REGION)")
		defaultZone = flag.String("default-zone", "", "controller: the zone to create a volume in when the CO names none")
		showVersion = flag.Bool("version", false, "print the version and exit")
	)
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return nil
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch *mode {
	case "controller":
	case "node":
		return errors.New("--mode=node: the node plugin isn't implemented yet")
	default:
		return fmt.Errorf("--mode must be controller or node, got %q", *mode)
	}

	client, err := newClient(ctx, *region)
	if err != nil {
		return err
	}
	d, err := driver.New(driver.Config{
		Version:     version,
		Cloud:       cloud.New(client),
		Project:     *project,
		DefaultZone: *defaultZone,
	})
	if err != nil {
		return err
	}
	l, err := driver.Listen(*endpoint)
	if err != nil {
		return err
	}
	slog.Info("serving", "driver", driver.Name, "version", version, "mode", *mode, "endpoint", *endpoint)
	return d.Serve(ctx, l)
}

// newClient returns an API client that authenticates as the service
// account of the instance the driver runs on, through the instance's
// metadata service. It takes no other credentials: no static key, token
// or file is read, whatever the environment holds, and no service account
// is impersonated.
func newClient(ctx context.Context, region string) (*core.Client, error) {
	credentials := core.InstanceMetadata()
	if endpoint := os.Getenv("ORIGINCLOUD_METADATA_ENDPOINT"); endpoint != "" {
		credentials.Endpoint = endpoint
	}
	opts := []core.Option{
		core.WithCredentials(credentials),
		core.WithImpersonateServiceAccount(""),
		core.WithUserAgent("origincloud-csi-driver/" + version),
	}
	if region != "" {
		opts = append(opts, core.WithRegion(region))
	}
	return core.NewClient(ctx, opts...)
}
