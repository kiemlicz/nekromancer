package power

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"gitlab.l3a.dev/infra/nekromancer/internal/config"
)

type stubProber struct {
	offline bool
	err     error
}

func (s stubProber) Offline(context.Context) (bool, error) { return s.offline, s.err }

func TestMikrotikPowerOnSendsWoL(t *testing.T) {
	var gotPath, gotUser, gotPass string
	var body map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.Method + " " + r.URL.Path
		gotUser, gotPass, _ = r.BasicAuth()
		_ = json.NewDecoder(r.Body).Decode(&body)
		_, _ = w.Write([]byte("[]"))
	}))
	defer srv.Close()

	m := &Mikrotik{HTTP: srv.Client(), URL: srv.URL + "/", Username: "u", Password: "p", MAC: "AA:BB", Interface: "bridge"}
	if err := m.PowerOn(context.Background()); err != nil {
		t.Fatal(err)
	}
	if gotPath != "POST /rest/tool/wol" || gotUser != "u" || gotPass != "p" {
		t.Errorf("request = %q user=%q pass=%q", gotPath, gotUser, gotPass)
	}
	if body["mac"] != "AA:BB" || body["interface"] != "bridge" {
		t.Errorf("body = %v", body)
	}
}

func TestMikrotikPowerOnSurfacesAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":400,"message":"Bad Request"}`))
	}))
	defer srv.Close()
	m := &Mikrotik{HTTP: srv.Client(), URL: srv.URL}
	if err := m.PowerOn(context.Background()); err == nil {
		t.Fatal("want error")
	}
}

func TestMikrotikOfflineDelegatesToProber(t *testing.T) {
	m := &Mikrotik{Prober: stubProber{offline: true}}
	off, err := m.Offline(context.Background())
	if err != nil || !off {
		t.Fatalf("off=%v err=%v", off, err)
	}
}

// pikvm fakes the ATX API with a settable power LED and records power actions.
func pikvm(t *testing.T, payload string, status int) (*PiKVM, *[]string) {
	t.Helper()
	var actions []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-KVMD-User") != "u" || r.Header.Get("X-KVMD-Passwd") != "p" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		switch r.Method + " " + r.URL.Path {
		case "GET /api/atx":
			w.WriteHeader(status)
			_, _ = w.Write([]byte(payload))
		case "POST /api/atx/power":
			actions = append(actions, r.URL.Query().Get("action"))
			_, _ = w.Write([]byte(`{"ok":true,"result":{}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return &PiKVM{HTTP: srv.Client(), URL: srv.URL, Username: "u", Password: "p"}, &actions
}

const (
	ledOn  = `{"ok":true,"result":{"enabled":true,"busy":false,"leds":{"power":true,"hdd":false}}}`
	ledOff = `{"ok":true,"result":{"enabled":true,"busy":false,"leds":{"power":false,"hdd":false}}}`
)

func TestPiKVMPowerOn(t *testing.T) {
	p, actions := pikvm(t, ledOff, 200)
	if err := p.PowerOn(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(*actions) != 1 || (*actions)[0] != "on" {
		t.Errorf("actions = %v", *actions)
	}
}

func TestPiKVMPowerOffPressesWhenOn(t *testing.T) {
	p, actions := pikvm(t, ledOn, 200)
	if err := p.PowerOff(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(*actions) != 1 || (*actions)[0] != "off" {
		t.Errorf("actions = %v", *actions)
	}
}

// "off" emulates a button click, a retry must not risk toggling a dead host on.
func TestPiKVMPowerOffSkipsPressWhenAlreadyOff(t *testing.T) {
	p, actions := pikvm(t, ledOff, 200)
	if err := p.PowerOff(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(*actions) != 0 {
		t.Errorf("actions = %v", *actions)
	}
}

func TestPiKVMOffline(t *testing.T) {
	for name, tc := range map[string]struct {
		payload string
		status  int
		want    bool
		wantErr bool
	}{
		"led on":         {ledOn, 200, false, false},
		"led off":        {ledOff, 200, true, false},
		"atx disabled":   {`{"ok":true,"result":{"enabled":false,"leds":{"power":false}}}`, 200, false, true},
		"server failure": {`{}`, 500, false, true},
		"garbage":        {`nope`, 200, false, true},
	} {
		t.Run(name, func(t *testing.T) {
			p, _ := pikvm(t, tc.payload, tc.status)
			got, err := p.Offline(context.Background())
			if (err != nil) != tc.wantErr || got != tc.want {
				t.Fatalf("got=%v err=%v", got, err)
			}
		})
	}
}

func TestICMPOnlineForLoopback(t *testing.T) {
	off, err := (&ICMP{Address: "127.0.0.1", Attempts: 2}).Offline(context.Background())
	if err != nil {
		t.Skipf("ping sockets unavailable: %v", err)
	}
	if off {
		t.Fatal("loopback reported offline")
	}
}

func TestClassifySendError(t *testing.T) {
	for name, tc := range map[string]struct {
		err  error
		want bool
	}{
		"no route to host is silence": {errHostUnreachable, true},
		"permission is unknown":       {errors.New("permission denied"), false},
	} {
		t.Run(name, func(t *testing.T) {
			if got := isSilence(tc.err); got != tc.want {
				t.Fatalf("got %v", got)
			}
		})
	}
}

func TestICMPBadAddressIsNeverOffline(t *testing.T) {
	off, err := (&ICMP{Address: "not-an-ip", Attempts: 1}).Offline(context.Background())
	if err == nil || off {
		t.Fatalf("off=%v err=%v", off, err)
	}
}

// TEST-NET-1 never answers. ENETUNREACH on hosts without a default route is
// deliberately an error, so that outcome is a skip rather than a failure.
func TestICMPOfflineForSilentAddress(t *testing.T) {
	off, err := (&ICMP{Address: "192.0.2.1", Attempts: 1}).Offline(context.Background())
	if err != nil {
		t.Skipf("no route to probe with: %v", err)
	}
	if !off {
		t.Fatal("silent address reported online")
	}
}

func TestBuildCreatesProviderPerHost(t *testing.T) {
	c := &config.Config{
		Mikrotik: config.Mikrotik{URL: "https://r"},
		Hosts: []config.Host{
			{Name: "a", Provider: config.ProviderMikrotik, Address: "10.0.0.1"},
			{Name: "b", Provider: config.ProviderPiKVM, PiKVMURL: "https://p"},
		},
	}
	got, err := Build(c, Credentials{})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got["a"].(*Mikrotik); !ok {
		t.Errorf("a = %T", got["a"])
	}
	if _, ok := got["b"].(*PiKVM); !ok {
		t.Errorf("b = %T", got["b"])
	}
}
