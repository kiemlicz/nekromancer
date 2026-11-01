package power

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"gitlab.l3a.dev/infra/nekromancer/internal/config"
)

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

// NotReady hosts get woken and may be running, a press there would shut them down.
func TestPiKVMPowerOnSkipsPressWhenAlreadyOn(t *testing.T) {
	p, actions := pikvm(t, ledOn, 200)
	if err := p.PowerOn(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(*actions) != 0 {
		t.Errorf("actions = %v", *actions)
	}
}

func TestPiKVMPowerOnFailsWhenStateUnknown(t *testing.T) {
	p, actions := pikvm(t, `{}`, 500)
	if err := p.PowerOn(context.Background()); err == nil {
		t.Fatal("want error")
	}
	if len(*actions) != 0 {
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

// recorder fakes one PiKVM and counts the presses it receives.
func recorder(t *testing.T, led string) (string, *[]string) {
	t.Helper()
	var actions []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "GET /api/atx":
			_, _ = w.Write([]byte(led))
		case "POST /api/atx/power":
			actions = append(actions, r.URL.Query().Get("action"))
			_, _ = w.Write([]byte(`{"ok":true,"result":{}}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL, &actions
}

// A press on the wrong PiKVM would power off the wrong machine.
func TestBuildPressesOnlyThatHostsPiKVM(t *testing.T) {
	urlA, a := recorder(t, ledOn)
	urlB, b := recorder(t, ledOn)
	got, err := Build(&config.Config{Hosts: []config.Host{
		{Name: "a", Wake: config.WakePiKVM, Shutdown: config.ShutdownPiKVM, PiKVMURL: urlA},
		{Name: "b", Wake: config.WakePiKVM, Shutdown: config.ShutdownPiKVM, PiKVMURL: urlB},
	}}, Credentials{})
	if err != nil {
		t.Fatal(err)
	}
	if err := got["b"].PowerOff(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(*a) != 0 || len(*b) != 1 || (*b)[0] != "off" {
		t.Fatalf("a = %v, b = %v", *a, *b)
	}
}

// The node-agent runs the command on these hosts. A press as well would race it.
func TestBuildOSShutdownNeverPresses(t *testing.T) {
	url, actions := recorder(t, ledOn)
	got, err := Build(&config.Config{Hosts: []config.Host{
		{Name: "a", Wake: config.WakePiKVM, Shutdown: config.ShutdownOS, PiKVMURL: url},
	}}, Credentials{})
	if err != nil {
		t.Fatal(err)
	}
	if err := got["a"].PowerOff(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(*actions) != 0 {
		t.Fatalf("actions = %v", *actions)
	}
}

func TestBuildWakesOverPiKVMWithOSShutdown(t *testing.T) {
	url, actions := recorder(t, ledOff)
	got, err := Build(&config.Config{Hosts: []config.Host{
		{Name: "a", Wake: config.WakePiKVM, Shutdown: config.ShutdownOS, PiKVMURL: url},
	}}, Credentials{})
	if err != nil {
		t.Fatal(err)
	}
	if err := got["a"].PowerOn(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(*actions) != 1 || (*actions)[0] != "on" {
		t.Fatalf("actions = %v", *actions)
	}
}

func TestBuildWakesOverMikrotikAndShutsDownOverPiKVM(t *testing.T) {
	url, actions := recorder(t, ledOn)
	wol := 0
	router := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		wol++
		_, _ = w.Write([]byte("[]"))
	}))
	defer router.Close()
	got, err := Build(&config.Config{
		Mikrotik: config.Mikrotik{URL: router.URL},
		Hosts:    []config.Host{{Name: "a", Wake: config.WakeMikrotik, Shutdown: config.ShutdownPiKVM, PiKVMURL: url}},
	}, Credentials{})
	if err != nil {
		t.Fatal(err)
	}
	if err := got["a"].PowerOn(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := got["a"].PowerOff(context.Background()); err != nil {
		t.Fatal(err)
	}
	if wol != 1 || len(*actions) != 1 || (*actions)[0] != "off" {
		t.Fatalf("wol = %d, actions = %v", wol, *actions)
	}
}

func TestBuildProbesLEDOnlyWhenWiredToPiKVM(t *testing.T) {
	url, _ := recorder(t, ledOn)
	got, err := Build(&config.Config{Hosts: []config.Host{
		{Name: "atx", Wake: config.WakePiKVM, Shutdown: config.ShutdownOS, PiKVMURL: url},
		{Name: "wol", Wake: config.WakeMikrotik, Shutdown: config.ShutdownOS, Address: "127.0.0.1"},
	}}, Credentials{})
	if err != nil {
		t.Fatal(err)
	}
	if h := got["atx"].(*Host); !isPiKVM(h.Prober) {
		t.Errorf("atx prober = %T", h.Prober)
	}
	if h := got["wol"].(*Host); isPiKVM(h.Prober) {
		t.Errorf("wol prober = %T", h.Prober)
	}
}

func isPiKVM(p Prober) bool { _, ok := p.(*PiKVM); return ok }

// Stock PiKVMs serve a self-signed certificate.
func TestBuildPiKVMCertificateVerification(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(ledOn))
	}))
	t.Cleanup(srv.Close)
	offline := func(insecure bool) error {
		got, err := Build(&config.Config{
			PiKVM: config.TLS{InsecureSkipVerify: insecure},
			Hosts: []config.Host{{Name: "a", Wake: config.WakePiKVM, PiKVMURL: srv.URL}},
		}, Credentials{})
		if err != nil {
			t.Fatal(err)
		}
		_, err = got["a"].Offline(context.Background())
		return err
	}
	var unknownAuthority x509.UnknownAuthorityError
	if err := offline(false); !errors.As(err, &unknownAuthority) {
		t.Fatalf("verified by default: err = %v", err)
	}
	if err := offline(true); err != nil {
		t.Fatalf("insecureSkipVerify: %v", err)
	}
}

// bridge fakes one PiKVM with Switch ports. The plain ATX API must never be pressed.
func bridge(t *testing.T, state string) (string, *[]string) {
	t.Helper()
	var presses []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "GET /api/switch":
			_, _ = w.Write([]byte(state))
		case "POST /api/switch/atx/power":
			presses = append(presses, r.URL.Query().Get("port")+":"+r.URL.Query().Get("action"))
			_, _ = w.Write([]byte(`{"ok":true,"result":{}}`))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL, &presses
}

// Port 0 is off, port 2 is on, and the port order isn't the inventory's.
const bridgeState = `{"ok":true,"result":{"model":{"ports":[
{"name":"mini1"},{"name":""},{"name":"mini2"},{"name":"dup"},{"name":"dup"}]},
"atx":{"leds":{"power":[false,false,true,false,false]}}}}`

func TestBridgeTargetsPortByName(t *testing.T) {
	url, presses := bridge(t, bridgeState)
	got, err := Build(&config.Config{Hosts: []config.Host{
		{Name: "a", Wake: config.WakePiKVM, Shutdown: config.ShutdownPiKVM, PiKVMURL: url, PiKVMPort: new("mini1")},
		{Name: "b", Wake: config.WakePiKVM, Shutdown: config.ShutdownPiKVM, PiKVMURL: url, PiKVMPort: new("mini2")},
	}}, Credentials{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if off, err := got["a"].Offline(ctx); err != nil || !off {
		t.Fatalf("a: offline = %v, err = %v", off, err)
	}
	if off, err := got["b"].Offline(ctx); err != nil || off {
		t.Fatalf("b: offline = %v, err = %v", off, err)
	}
	// Each of these is the only one that applies to that host's LED, the opposite calls are skipped.
	for _, call := range []func(context.Context) error{got["a"].PowerOn, got["a"].PowerOff, got["b"].PowerOff, got["b"].PowerOn} {
		if err := call(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if len(*presses) != 2 || (*presses)[0] != "0:on" || (*presses)[1] != "2:off" {
		t.Fatalf("presses = %v", *presses)
	}
}

// Anything but one exact match must neither press nor count as offline.
func TestBridgeUnresolvedPortIsAnError(t *testing.T) {
	cases := map[string]struct{ port, state string }{
		"unknown":      {"missing", bridgeState},
		"duplicate":    {"dup", bridgeState},
		"case differs": {"N150-0", bridgeState},
		"no ports":     {"mini1", `{"ok":true,"result":{"model":{"ports":[]},"atx":{"leds":{"power":[]}}}}`},
		"LED missing":  {"mini2", `{"ok":true,"result":{"model":{"ports":[{"name":"mini1"},{"name":"mini2"}]},"atx":{"leds":{"power":[true]}}}}`},
		"not json":     {"mini1", `<html>`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			url, presses := bridge(t, tc.state)
			got, err := Build(&config.Config{Hosts: []config.Host{
				{Name: "a", Wake: config.WakePiKVM, Shutdown: config.ShutdownPiKVM, PiKVMURL: url, PiKVMPort: new(tc.port)},
			}}, Credentials{})
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			if off, err := got["a"].Offline(ctx); err == nil || off {
				t.Fatalf("offline = %v, err = %v", off, err)
			}
			if err := got["a"].PowerOn(ctx); err == nil {
				t.Fatal("PowerOn succeeded")
			}
			if err := got["a"].PowerOff(ctx); err == nil {
				t.Fatal("PowerOff succeeded")
			}
			if len(*presses) != 0 {
				t.Fatalf("pressed: %v", *presses)
			}
		})
	}
}

// An inventory host without pikvmPort uses the Switch port named after it.
func TestBridgeDefaultsToPortNamedAfterHost(t *testing.T) {
	url, presses := bridge(t, bridgeState)
	c, err := config.Parse([]byte("hosts:\n- {name: mini2, wake: pikvm, shutdown: pikvm, pikvmURL: '" + url + "'}\n"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := Build(c, Credentials{})
	if err != nil {
		t.Fatal(err)
	}
	if err := got["mini2"].PowerOff(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(*presses) != 1 || (*presses)[0] != "2:off" {
		t.Fatalf("presses = %v", *presses)
	}
}

// pikvmPort: "" keeps the plain ATX API for a PiKVM without a Switch.
func TestEmptyPortUsesPlainATX(t *testing.T) {
	url, actions := recorder(t, ledOn)
	c, err := config.Parse([]byte("hosts:\n- {name: a, wake: pikvm, shutdown: pikvm, pikvmURL: '" + url + "', pikvmPort: ''}\n"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := Build(c, Credentials{})
	if err != nil {
		t.Fatal(err)
	}
	if err := got["a"].PowerOff(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(*actions) != 1 || (*actions)[0] != "off" {
		t.Fatalf("actions = %v", *actions)
	}
}
