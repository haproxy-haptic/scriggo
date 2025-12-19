// Copyright 2024 The Scriggo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package pgo provides benchmarks for Profile-Guided Optimization (PGO).
// These benchmarks simulate realistic template workloads to generate
// representative CPU profiles for the Go compiler's PGO feature.
package pgo

import (
	"fmt"
	"strings"
	"testing"

	"github.com/open2b/scriggo"
	"github.com/open2b/scriggo/native"
)

// Service represents a Kubernetes-like service
type Service struct {
	Name        string
	Namespace   string
	ClusterIP   string
	Ports       []ServicePort
	Annotations map[string]string
	Labels      map[string]string
}

// ServicePort represents a port in a service
type ServicePort struct {
	Name       string
	Port       int
	TargetPort int
	Protocol   string
}

// Endpoint represents a Kubernetes-like endpoint
type Endpoint struct {
	Name      string
	Namespace string
	Addresses []EndpointAddress
}

// EndpointAddress represents an endpoint address
type EndpointAddress struct {
	IP       string
	Hostname string
	Ready    bool
}

// Backend represents a backend server configuration
type Backend struct {
	Name        string
	Address     string
	Port        int
	Weight      int
	MaxConn     int
	Annotations map[string]string
}

// K8sData holds all the generated Kubernetes-like data
type K8sData struct {
	Services  []Service
	Endpoints []Endpoint
	Backends  []Backend
	Config    map[string]interface{}
}

// generateK8sData generates simulated Kubernetes data for benchmarks
func generateK8sData(numServices, numEndpoints, numBackends int) (*K8sData, native.Declarations) {
	data := &K8sData{
		Services:  make([]Service, numServices),
		Endpoints: make([]Endpoint, numEndpoints),
		Backends:  make([]Backend, numBackends),
		Config: map[string]interface{}{
			"maxconn":       10000,
			"timeout":       30,
			"check_enabled": true,
		},
	}

	for i := 0; i < numServices; i++ {
		ports := make([]ServicePort, 2)
		for j := 0; j < 2; j++ {
			ports[j] = ServicePort{
				Name:       fmt.Sprintf("port-%d", j),
				Port:       8080 + j,
				TargetPort: 8080 + j,
				Protocol:   "TCP",
			}
		}
		data.Services[i] = Service{
			Name:      fmt.Sprintf("service-%d", i),
			Namespace: "default",
			ClusterIP: fmt.Sprintf("10.0.%d.%d", i/256, i%256),
			Ports:     ports,
			Annotations: map[string]string{
				"haproxy.org/check":          "true",
				"haproxy.org/load-balance":   "roundrobin",
				"haproxy.org/timeout-server": "30s",
			},
			Labels: map[string]string{
				"app":     fmt.Sprintf("app-%d", i),
				"version": "v1",
			},
		}
	}

	for i := 0; i < numEndpoints; i++ {
		addrs := make([]EndpointAddress, 3)
		for j := 0; j < 3; j++ {
			addrs[j] = EndpointAddress{
				IP:       fmt.Sprintf("192.168.%d.%d", i/256, (i*3+j)%256),
				Hostname: fmt.Sprintf("pod-%d-%d", i, j),
				Ready:    j != 2, // last one is not ready
			}
		}
		data.Endpoints[i] = Endpoint{
			Name:      fmt.Sprintf("endpoint-%d", i),
			Namespace: "default",
			Addresses: addrs,
		}
	}

	for i := 0; i < numBackends; i++ {
		data.Backends[i] = Backend{
			Name:    fmt.Sprintf("backend-%d", i),
			Address: fmt.Sprintf("10.1.%d.%d", i/256, i%256),
			Port:    8080,
			Weight:  100,
			MaxConn: 1000,
			Annotations: map[string]string{
				"check-interval": "5s",
				"fall":           "3",
				"rise":           "2",
			},
		}
	}

	// Use pointers for imported variables in Scriggo
	return data, native.Declarations{
		"services":  &data.Services,
		"endpoints": &data.Endpoints,
		"backends":  &data.Backends,
		"config":    &data.Config,
	}
}

// PGO benchmark templates that exercise key VM operations
var pgoTemplates = map[string][]byte{
	// Template with field access and loops
	"field_access.txt": []byte(`{% for svc in services %}
server {{ svc.Name }} {{ svc.ClusterIP }}:{{ svc.Ports[0].Port }}
{% end %}`),

	// Template with map access
	"map_access.txt": []byte(`{% for svc in services %}
{% if svc.Annotations["haproxy.org/check"] == "true" %}
check inter {{ svc.Annotations["haproxy.org/timeout-server"] }}
{% end %}
{% end %}`),

	// Template with nested loops
	"nested_loops.txt": []byte(`{% for ep in endpoints %}
{% for addr in ep.Addresses %}
{% if addr.Ready %}
server {{ ep.Name }}-{{ addr.Hostname }} {{ addr.IP }}:8080 check
{% end %}
{% end %}
{% end %}`),

	// Template with conditionals and map access
	"conditionals.txt": []byte(`{% for backend in backends %}
{% if backend.Weight > 50 %}
{% if backend.Annotations["check-interval"] != "" %}
server {{ backend.Name }} {{ backend.Address }}:{{ backend.Port }} weight {{ backend.Weight }} check inter {{ backend.Annotations["check-interval"] }}
{% else %}
server {{ backend.Name }} {{ backend.Address }}:{{ backend.Port }} weight {{ backend.Weight }}
{% end %}
{% end %}
{% end %}`),

	// Combined realistic template
	"haproxy_backend.txt": []byte(`backend app
    mode http
    balance roundrobin
{% for svc in services %}
    # Service: {{ svc.Name }}
{% if svc.Labels["version"] == "v1" %}
{% for port in svc.Ports %}
    server {{ svc.Name }}-{{ port.Name }} {{ svc.ClusterIP }}:{{ port.Port }}{% if svc.Annotations["haproxy.org/check"] == "true" %} check{% end %}

{% end %}
{% end %}
{% end %}`),
}

func BenchmarkPGO(b *testing.B) {
	// Small scale (10 items)
	b.Run("Small", func(b *testing.B) {
		runPGOBenchmark(b, 10, 10, 10)
	})

	// Medium scale (100 items)
	b.Run("Medium", func(b *testing.B) {
		runPGOBenchmark(b, 100, 100, 100)
	})

	// Large scale (500 items)
	b.Run("Large", func(b *testing.B) {
		runPGOBenchmark(b, 500, 500, 500)
	})
}

func runPGOBenchmark(b *testing.B, numServices, numEndpoints, numBackends int) {
	_, globals := generateK8sData(numServices, numEndpoints, numBackends)

	for name, tmpl := range pgoTemplates {
		sources := scriggo.Files{
			"index.txt": tmpl,
		}

		template, err := scriggo.BuildTemplate(sources, "index.txt", &scriggo.BuildOptions{
			Globals: globals,
		})
		if err != nil {
			b.Fatalf("%s: build error: %v", name, err)
		}

		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for n := 0; n < b.N; n++ {
				var out strings.Builder
				err := template.Run(&out, nil, nil)
				if err != nil {
					b.Fatalf("run error: %v", err)
				}
			}
		})
	}
}

// BenchmarkPGOParallel benchmarks parallel rendering patterns
func BenchmarkPGOParallel(b *testing.B) {
	_, globals := generateK8sData(100, 100, 100)

	// Template with parallel renders using go render
	indexTemplate := []byte(`{% for backend in backends %}{{ go render "server.txt" }}{% end %}`)
	serverTemplate := []byte(`server backend
`)

	sources := scriggo.Files{
		"index.txt":  indexTemplate,
		"server.txt": serverTemplate,
	}

	template, err := scriggo.BuildTemplate(sources, "index.txt", &scriggo.BuildOptions{
		Globals:     globals,
		AllowGoStmt: true,
	})
	if err != nil {
		b.Fatalf("build error: %v", err)
	}

	b.Run("ParallelRender100", func(b *testing.B) {
		b.ReportAllocs()
		for n := 0; n < b.N; n++ {
			var out strings.Builder
			err := template.Run(&out, nil, nil)
			if err != nil {
				b.Fatalf("run error: %v", err)
			}
		}
	})
}
