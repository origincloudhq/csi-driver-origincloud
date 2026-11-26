package metadata

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func serve(t *testing.T, status int, body string) Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/origin/v1/instance" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return Client{Endpoint: srv.URL + "/"}
}

func TestInstance(t *testing.T) {
	c := serve(t, http.StatusOK, `{
		"name": "//compute.origincloudapis.eu/projects/p/zones/eu-central1-az1/instances/i-1",
		"instanceId": "i-1",
		"zone": "eu-central1-az1",
		"hostname": "ip-10-0-1-5.internal"
	}`)
	inst, err := c.Instance(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := Instance{Name: "//compute.origincloudapis.eu/projects/p/zones/eu-central1-az1/instances/i-1", Zone: "eu-central1-az1"}
	if inst != want {
		t.Errorf("Instance = %+v, want %+v", inst, want)
	}
}

func TestInstanceFails(t *testing.T) {
	for name, c := range map[string]Client{
		"404":         serve(t, http.StatusNotFound, ""),
		"not json":    serve(t, http.StatusOK, "<html>"),
		"no zone":     serve(t, http.StatusOK, `{"name": "//compute.origincloudapis.eu/projects/p/zones/z/instances/i"}`),
		"unreachable": {Endpoint: "http://127.0.0.1:1"},
	} {
		if _, err := c.Instance(context.Background()); err == nil {
			t.Errorf("%s: Instance succeeded", name)
		}
	}
}
