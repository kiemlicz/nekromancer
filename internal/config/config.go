// Package config loads the static hardware inventory.
package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"
)

const (
	ProviderMikrotik = "mikrotik"
	ProviderPiKVM    = "pikvm"
)

type Config struct {
	DefaultIdleTimeout metav1.Duration `json:"defaultIdleTimeout"`
	// NotReadyGrace debounces kubelet blips before the host is probed.
	NotReadyGrace metav1.Duration `json:"notReadyGrace"`
	// WakeCooldown is how long a woken host gets to register before another attempt.
	WakeCooldown metav1.Duration `json:"wakeCooldown"`
	// PowerOffRetry is how long a host may stay online before power off is requested again.
	PowerOffRetry metav1.Duration `json:"powerOffRetry"`
	Mikrotik      Mikrotik        `json:"mikrotik"`
	PiKVM         TLS             `json:"pikvm"`
	Hosts         []Host          `json:"hosts"`
}

type TLS struct {
	InsecureSkipVerify bool   `json:"insecureSkipVerify"`
	CAFile             string `json:"caFile"`
}

type Mikrotik struct {
	URL string `json:"url"`
	TLS `json:",inline"`
}

// Host describes one physical machine. Name must equal its Node name.
type Host struct {
	Name string `json:"name"`
	// Labels must mirror the kubelet --node-labels, pending pods are matched against them.
	Labels    map[string]string `json:"labels"`
	Provider  string            `json:"provider"`
	MAC       string            `json:"mac"`
	Interface string            `json:"interface"`
	Address   string            `json:"address"`
	PiKVMURL  string            `json:"pikvmURL"`
}

func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(raw)
}

func Parse(raw []byte) (*Config, error) {
	var c Config
	if err := yaml.UnmarshalStrict(raw, &c); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}
	for _, d := range []struct {
		name string
		v    *metav1.Duration
		def  time.Duration
	}{
		{"defaultIdleTimeout", &c.DefaultIdleTimeout, 10 * time.Minute},
		{"notReadyGrace", &c.NotReadyGrace, time.Minute},
		{"wakeCooldown", &c.WakeCooldown, 5 * time.Minute},
		{"powerOffRetry", &c.PowerOffRetry, 2 * time.Minute},
	} {
		if d.v.Duration < 0 {
			return nil, fmt.Errorf("%s must not be negative", d.name)
		}
		if d.v.Duration == 0 {
			d.v.Duration = d.def
		}
	}
	if err := c.validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) validate() error {
	if len(c.Hosts) == 0 {
		return errors.New("no hosts configured")
	}
	seen := map[string]bool{}
	for i, h := range c.Hosts {
		if h.Name == "" {
			return fmt.Errorf("hosts[%d]: name is required", i)
		}
		if seen[h.Name] {
			return fmt.Errorf("hosts[%d]: duplicate name %q", i, h.Name)
		}
		seen[h.Name] = true
		if err := c.validateProvider(h); err != nil {
			return fmt.Errorf("host %q: %w", h.Name, err)
		}
	}
	return nil
}

func (c *Config) validateProvider(h Host) error {
	switch h.Provider {
	case ProviderMikrotik:
		if !validURL(c.Mikrotik.URL) {
			return errors.New("mikrotik.url must be set to an absolute URL")
		}
		if _, err := net.ParseMAC(h.MAC); err != nil {
			return fmt.Errorf("invalid mac: %w", err)
		}
		if h.Interface == "" {
			return errors.New("interface is required")
		}
		if ip := net.ParseIP(h.Address); ip == nil || ip.To4() == nil {
			return fmt.Errorf("address %q is not an IPv4 address", h.Address)
		}
	case ProviderPiKVM:
		if !validURL(h.PiKVMURL) {
			return errors.New("pikvmURL must be set to an absolute URL")
		}
	default:
		return fmt.Errorf("unknown provider %q", h.Provider)
	}
	return nil
}

func validURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && u.Scheme != "" && u.Host != ""
}
