// Package metadata reads who the instance the node plugin runs on is from
// the instance's metadata service, `GET /origin/v1/instance`. The service
// answers only the instance asking, by its address, and needs no
// credentials.
package metadata

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// DefaultEndpoint is the instance's metadata service.
const DefaultEndpoint = "http://169.254.169.254"

// Instance is what the node plugin needs to know about its instance.
type Instance struct {
	// Name is the instance's full name,
	// `//compute.origincloudapis.eu/projects/{project}/zones/{zone}/instances/{instance}`.
	Name string `json:"name"`
	// Zone is the zone the instance runs in, such as eu-central1-az1.
	Zone string `json:"zone"`
}

// Client asks the metadata service.
type Client struct {
	// Endpoint is the metadata service; DefaultEndpoint when empty.
	Endpoint string
	// HTTPClient sends the request; when nil, a client that never goes
	// through a proxy, which the metadata service refuses.
	HTTPClient *http.Client
}

var direct = &http.Client{
	Timeout:   5 * time.Second,
	Transport: &http.Transport{Proxy: nil},
}

// Instance returns the instance the plugin runs on.
func (c Client) Instance(ctx context.Context) (Instance, error) {
	endpoint := strings.TrimSuffix(c.Endpoint, "/")
	if endpoint == "" {
		endpoint = DefaultEndpoint
	}
	client := c.HTTPClient
	if client == nil {
		client = direct
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"/origin/v1/instance", nil)
	if err != nil {
		return Instance{}, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return Instance{}, fmt.Errorf("asking the metadata service: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return Instance{}, fmt.Errorf("reading the metadata service's answer: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return Instance{}, fmt.Errorf("the metadata service answered HTTP %d", resp.StatusCode)
	}
	var inst Instance
	if err := json.Unmarshal(body, &inst); err != nil {
		return Instance{}, fmt.Errorf("the metadata service's answer is no instance: %w", err)
	}
	if inst.Name == "" || inst.Zone == "" {
		return Instance{}, fmt.Errorf("the metadata service's answer names no instance or no zone")
	}
	return inst, nil
}
