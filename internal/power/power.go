// Package power drives the physical machines.
package power

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"gitlab.l3a.dev/infra/nekromancer/internal/config"
)

// Provider controls one host.
type Provider interface {
	Waker
	Shutdowner
	Prober
}

type Waker interface {
	PowerOn(ctx context.Context) error
}

type Shutdowner interface {
	PowerOff(ctx context.Context) error
}

type Prober interface {
	// Offline must only return true when the host is confirmed down: a false
	// positive deletes the Node of a running machine. Uncertainty is an error.
	Offline(ctx context.Context) (bool, error)
}

// Host combines the methods chosen for one inventory entry.
type Host struct {
	Waker
	Shutdowner
	Prober
}

// OS leaves the shutdown to the node-agent, which acts on the shutdown-requested annotation.
type OS struct{}

func (OS) PowerOff(context.Context) error { return nil }

// Credentials are supplied from a Secret, never from the inventory.
type Credentials struct {
	MikrotikUsername, MikrotikPassword string
	PiKVMUsername, PiKVMPassword       string
}

// Build creates a provider per inventory host, keyed by host name.
func Build(c *config.Config, creds Credentials) (map[string]Provider, error) {
	mikrotikHTTP, err := httpClient(c.Mikrotik.TLS)
	if err != nil {
		return nil, fmt.Errorf("mikrotik TLS: %w", err)
	}
	pikvmHTTP, err := httpClient(c.PiKVM)
	if err != nil {
		return nil, fmt.Errorf("pikvm TLS: %w", err)
	}
	out := make(map[string]Provider, len(c.Hosts))
	for _, h := range c.Hosts {
		host := &Host{Shutdowner: OS{}, Prober: &ICMP{Address: h.Address}}
		kvm := &PiKVM{
			HTTP: pikvmHTTP, URL: h.PiKVMURL,
			Username: creds.PiKVMUsername, Password: creds.PiKVMPassword,
			Port: h.Port(),
		}
		if h.UsesPiKVM() {
			host.Prober = kvm
		}
		if h.Shutdown == config.ShutdownPiKVM {
			host.Shutdowner = kvm
		}
		switch h.Wake {
		case config.WakeMikrotik:
			host.Waker = &Mikrotik{
				HTTP: mikrotikHTTP, URL: c.Mikrotik.URL,
				Username: creds.MikrotikUsername, Password: creds.MikrotikPassword,
				MAC: h.MAC, Interface: h.Interface,
			}
		case config.WakePiKVM:
			host.Waker = kvm
		default:
			return nil, fmt.Errorf("host %q: unknown wake provider %q", h.Name, h.Wake)
		}
		out[h.Name] = host
	}
	return out, nil
}

func httpClient(t config.TLS) (*http.Client, error) {
	cfg := &tls.Config{InsecureSkipVerify: t.InsecureSkipVerify} //nolint:gosec // opt-in, homelab devices ship self-signed certs
	if t.CAFile != "" {
		pem, err := os.ReadFile(t.CAFile)
		if err != nil {
			return nil, err
		}
		cfg.RootCAs = x509.NewCertPool()
		if !cfg.RootCAs.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("no certificates in %s", t.CAFile)
		}
	}
	return &http.Client{
		Timeout:   10 * time.Second,
		Transport: &http.Transport{TLSClientConfig: cfg},
	}, nil
}

// do returns the body of a 2xx response and an error for anything else.
func do(c *http.Client, req *http.Request) ([]byte, error) {
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("%s %s: %s: %s", req.Method, req.URL.Path, resp.Status, strings.TrimSpace(string(body)))
	}
	return body, nil
}
