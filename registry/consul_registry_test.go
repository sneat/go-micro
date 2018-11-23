package registry

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"testing"
	"time"

	consul "github.com/hashicorp/consul/api"
)

type mockRegistry struct {
	body   []byte
	status int
	err    error
	url    string
}

func encodeData(obj interface{}) ([]byte, error) {
	buf := bytes.NewBuffer(nil)
	enc := json.NewEncoder(buf)
	if err := enc.Encode(obj); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func newMockServer(rg *mockRegistry, l net.Listener) error {
	mux := http.NewServeMux()
	mux.HandleFunc(rg.url, func(w http.ResponseWriter, r *http.Request) {
		if rg.err != nil {
			http.Error(w, rg.err.Error(), 500)
			return
		}
		w.WriteHeader(rg.status)
		w.Write(rg.body)
	})
	return http.Serve(l, mux)
}

func newConsulTestRegistry(r *mockRegistry) (*consulRegistry, func()) {
	l, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		// blurgh?!!
		panic(err.Error())
	}
	cfg := consul.DefaultConfig()
	cfg.Address = l.Addr().String()
	cl, _ := consul.NewClient(cfg)

	go newMockServer(r, l)

	return &consulRegistry{
			Address:     cfg.Address,
			Client:      cl,
			opts:        Options{},
			register:    make(map[string]uint64),
			lastChecked: make(map[string]time.Time),
			queryOptions: &consul.QueryOptions{
				AllowStale: true,
			},
		}, func() {
			l.Close()
		}
}

func newServiceList(svc []*consul.ServiceEntry) []byte {
	bts, _ := encodeData(svc)
	return bts
}

func TestConsul_GetService_WithError(t *testing.T) {
	cr, cl := newConsulTestRegistry(&mockRegistry{
		err: errors.New("client-error"),
		url: "/v1/health/service/service-name",
	})
	defer cl()

	if _, err := cr.GetService("test-service"); err == nil {
		t.Fatalf("Expected error not to be `nil`")
	}
}

func TestConsul_GetService_WithHealthyServiceNodes(t *testing.T) {
	// warning is still seen as healthy, critical is not
	svcs := []*consul.ServiceEntry{
		newServiceEntry(
			"node-name-1", "node-address-1", "service-name", "v1.0.0",
			[]*consul.HealthCheck{
				newHealthCheck("node-name-1", "service-name", "passing"),
				newHealthCheck("node-name-1", "service-name", "warning"),
			},
		),
		newServiceEntry(
			"node-name-2", "node-address-2", "service-name", "v1.0.0",
			[]*consul.HealthCheck{
				newHealthCheck("node-name-2", "service-name", "passing"),
				newHealthCheck("node-name-2", "service-name", "warning"),
			},
		),
	}

	cr, cl := newConsulTestRegistry(&mockRegistry{
		status: 200,
		body:   newServiceList(svcs),
		url:    "/v1/health/service/service-name",
	})
	defer cl()

	svc, err := cr.GetService("service-name")
	if err != nil {
		t.Fatal("Unexpected error", err)
	}

	if exp, act := 1, len(svc); exp != act {
		t.Fatalf("Expected len of svc to be `%d`, got `%d`.", exp, act)
	}

	if exp, act := 2, len(svc[0].Nodes); exp != act {
		t.Fatalf("Expected len of nodes to be `%d`, got `%d`.", exp, act)
	}
}

func TestConsul_GetService_WithUnhealthyServiceNode(t *testing.T) {
	// warning is still seen as healthy, critical is not
	svcs := []*consul.ServiceEntry{
		newServiceEntry(
			"node-name-1", "node-address-1", "service-name", "v1.0.0",
			[]*consul.HealthCheck{
				newHealthCheck("node-name-1", "service-name", "passing"),
				newHealthCheck("node-name-1", "service-name", "warning"),
			},
		),
		newServiceEntry(
			"node-name-2", "node-address-2", "service-name", "v1.0.0",
			[]*consul.HealthCheck{
				newHealthCheck("node-name-2", "service-name", "passing"),
				newHealthCheck("node-name-2", "service-name", "critical"),
			},
		),
	}

	cr, cl := newConsulTestRegistry(&mockRegistry{
		status: 200,
		body:   newServiceList(svcs),
		url:    "/v1/health/service/service-name",
	})
	defer cl()

	svc, err := cr.GetService("service-name")
	if err != nil {
		t.Fatal("Unexpected error", err)
	}

	if exp, act := 1, len(svc); exp != act {
		t.Fatalf("Expected len of svc to be `%d`, got `%d`.", exp, act)
	}

	if exp, act := 1, len(svc[0].Nodes); exp != act {
		t.Fatalf("Expected len of nodes to be `%d`, got `%d`.", exp, act)
	}
}

func TestConsul_GetService_WithUnhealthyServiceNodes(t *testing.T) {
	// warning is still seen as healthy, critical is not
	svcs := []*consul.ServiceEntry{
		newServiceEntry(
			"node-name-1", "node-address-1", "service-name", "v1.0.0",
			[]*consul.HealthCheck{
				newHealthCheck("node-name-1", "service-name", "passing"),
				newHealthCheck("node-name-1", "service-name", "critical"),
			},
		),
		newServiceEntry(
			"node-name-2", "node-address-2", "service-name", "v1.0.0",
			[]*consul.HealthCheck{
				newHealthCheck("node-name-2", "service-name", "passing"),
				newHealthCheck("node-name-2", "service-name", "critical"),
			},
		),
	}

	cr, cl := newConsulTestRegistry(&mockRegistry{
		status: 200,
		body:   newServiceList(svcs),
		url:    "/v1/health/service/service-name",
	})
	defer cl()

	svc, err := cr.GetService("service-name")
	if err != nil {
		t.Fatal("Unexpected error", err)
	}

	if exp, act := 1, len(svc); exp != act {
		t.Fatalf("Expected len of svc to be `%d`, got `%d`.", exp, act)
	}

	if exp, act := 0, len(svc[0].Nodes); exp != act {
		t.Fatalf("Expected len of nodes to be `%d`, got `%d`.", exp, act)
	}
}

// AllowStale sets whether any Consul server (non-leader) can service
// a read. This allows for lower latency and higher throughput
// at the cost of potentially stale data.
// Works similar to Consul DNS Config option [1].
// Defaults to true.
//
// [1] https://www.consul.io/docs/agent/options.html#allow_stale
//
func AllowStale(v bool) Option {
	return func(o *Options) {
		if o.Context == nil {
			o.Context = context.Background()
		}
		o.Context = context.WithValue(o.Context, "consul_allow_stale", v)
	}
}

// QueryOptions specifies the QueryOptions to be used when calling
// Consul. See `Consul API` for more information [1].
//
// [1] https://godoc.org/github.com/hashicorp/consul/api#QueryOptions
//
func QueryOptions(q *consul.QueryOptions) Option {
	return func(o *Options) {
		if q == nil {
			return
		}
		if o.Context == nil {
			o.Context = context.Background()
		}
		o.Context = context.WithValue(o.Context, "consul_query_options", q)
	}
}

func TestQueryOptions_Default(t *testing.T) {
	cr, cl := newConsulTestRegistry(&mockRegistry{
		err: errors.New("client-error"),
		url: "/v1/health/service/service-name",
	})
	defer cl()

	configure(cr)
	if !cr.queryOptions.AllowStale {
		t.Fatalf("Expected AllowStale to be `true`, got `%t`.", cr.queryOptions.AllowStale)
	}
}

func TestQueryOptions_AllowStale(t *testing.T) {
	cr, cl := newConsulTestRegistry(&mockRegistry{
		err: errors.New("client-error"),
		url: "/v1/health/service/service-name",
	})
	defer cl()

	configure(cr, AllowStale(true))
	if !cr.queryOptions.AllowStale {
		t.Fatalf("Expected AllowStale to be `true`, got `%t`.", cr.queryOptions.AllowStale)
	}

	configure(cr, AllowStale(false))
	if cr.queryOptions.AllowStale {
		t.Fatalf("Expected AllowStale to be `false`, got `%t`.", cr.queryOptions.AllowStale)
	}
}

func TestQueryOptions_QueryOptions(t *testing.T) {
	cr, cl := newConsulTestRegistry(&mockRegistry{
		err: errors.New("client-error"),
		url: "/v1/health/service/service-name",
	})
	defer cl()

	configure(cr, QueryOptions(nil))
	if !cr.queryOptions.AllowStale {
		t.Fatalf("Expected AllowStale to be `true`, got `%t`.", cr.queryOptions.AllowStale)
	}

	configure(cr, QueryOptions(&consul.QueryOptions{}))
	if cr.queryOptions.AllowStale {
		t.Fatalf("Expected AllowStale to be `false`, got `%t`.", cr.queryOptions.AllowStale)
	}

	configure(cr, QueryOptions(&consul.QueryOptions{
		NodeMeta: map[string]string{
			"foo": "bar",
		},
	}))
	if cr.queryOptions.NodeMeta == nil || cr.queryOptions.NodeMeta["foo"] != "bar" {
		t.Fatalf("Expected NodeMeta to be `map[string]string{\"foo\":\"bar\"}`, got `%#v`.", cr.queryOptions.NodeMeta)
	}
}
