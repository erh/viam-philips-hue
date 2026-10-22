package hue

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pion/dtls/v3"
	"go.viam.com/rdk/components/generic"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/resource"
)

const (
	testStreamUser      = "streamuser"
	testStreamClientKey = "0123456789abcdef0123456789abcdef"
	testEntGroupID      = 200
)

// fakeBridgeDep stands in for a hue-bridge dependency and answers bridge_info.
type fakeBridgeDep struct {
	name      resource.Name
	host      string
	clientKey string
}

func (f *fakeBridgeDep) Name() resource.Name { return f.name }
func (f *fakeBridgeDep) Status(context.Context) (map[string]interface{}, error) {
	return map[string]interface{}{}, nil
}
func (f *fakeBridgeDep) Close(context.Context) error { return nil }
func (f *fakeBridgeDep) DoCommand(_ context.Context, cmd map[string]interface{}) (map[string]interface{}, error) {
	return map[string]interface{}{"bridge_host": f.host, "username": testStreamUser, "clientkey": f.clientKey}, nil
}

type recordedPut struct {
	path string
	body string
}

// fakeHueHTTP is a v1 REST bridge with one room (81) and one Entertainment
// area (200) holding lights 7 and 12.
type fakeHueHTTP struct {
	srv  *httptest.Server
	mu   sync.Mutex
	puts []recordedPut
}

func newFakeHueHTTP(t *testing.T) *fakeHueHTTP {
	f := &fakeHueHTTP{}
	prefix := "/api/" + testStreamUser
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimSuffix(r.URL.Path, "/")
		switch {
		case r.Method == http.MethodGet && path == prefix+"/groups":
			_, _ = io.WriteString(w, `{
				"81": {"name": "Basement", "type": "Room", "lights": ["7", "12", "19"]},
				"200": {"name": "rehearsal", "type": "Entertainment", "lights": ["7", "12"], "stream": {"active": false}},
				"201": {"name": "Studio sync", "type": "Entertainment", "lights": ["19"]}
			}`)
		case r.Method == http.MethodGet && path == prefix+"/lights":
			_, _ = io.WriteString(w, `{
				"7": {"name": "Art-Right", "type": "Extended color light"},
				"12": {"name": "Art-Center", "type": "Extended color light"},
				"19": {"name": "Guitars", "type": "Extended color light"}
			}`)
		case r.Method == http.MethodPut && strings.HasPrefix(path, prefix+"/groups/"):
			body, _ := io.ReadAll(r.Body)
			f.mu.Lock()
			f.puts = append(f.puts, recordedPut{path: strings.TrimPrefix(path, prefix), body: string(body)})
			f.mu.Unlock()
			_, _ = io.WriteString(w, `[{"success": {"/groups/200/stream/active": true}}]`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeHueHTTP) recordedPuts() []recordedPut {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recordedPut(nil), f.puts...)
}

func (f *fakeHueHTTP) host() string {
	return strings.TrimPrefix(f.srv.URL, "http://")
}

type streamPacket struct {
	at   time.Time
	data []byte
}

// fakeStreamServer is a DTLS-PSK server on loopback playing the bridge's
// Entertainment port. It records the client's PSK identity, the negotiated
// cipher suite, and every datagram received.
type fakeStreamServer struct {
	port      int
	handshake chan dtls.State
	packets   chan streamPacket
}

func newFakeStreamServer(t *testing.T) *fakeStreamServer {
	psk, err := hex.DecodeString(testStreamClientKey)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := dtls.ListenWithOptions("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)},
		dtls.WithPSK(func([]byte) ([]byte, error) { return psk, nil }),
		dtls.WithPSKIdentityHint([]byte("hue-bridge")),
		// Offer other PSK suites first, so a client that does not insist on
		// AES-128-GCM would negotiate something else.
		dtls.WithCipherSuites(
			dtls.TLS_PSK_WITH_AES_128_CCM_8,
			dtls.TLS_PSK_WITH_AES_128_CCM,
			dtls.TLS_PSK_WITH_AES_128_CBC_SHA256,
			dtls.TLS_PSK_WITH_AES_128_GCM_SHA256,
		),
	)
	if err != nil {
		t.Fatal(err)
	}
	s := &fakeStreamServer{
		port:      ln.Addr().(*net.UDPAddr).Port,
		handshake: make(chan dtls.State, 4),
		packets:   make(chan streamPacket, 4096),
	}
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				dc := c.(*dtls.Conn)
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				err := dc.HandshakeContext(ctx)
				cancel()
				if err != nil {
					return
				}
				if st, ok := dc.ConnectionState(); ok {
					s.handshake <- st
				}
				buf := make([]byte, 1500)
				for {
					n, err := dc.Read(buf)
					if err != nil {
						return
					}
					select {
					case s.packets <- streamPacket{at: time.Now(), data: append([]byte(nil), buf[:n]...)}:
					default:
					}
				}
			}(c)
		}
	}()
	return s
}

// waitPacket returns the first packet satisfying pred, failing after 2 s.
func (s *fakeStreamServer) waitPacket(t *testing.T, pred func(map[uint16][3]uint16) bool) []byte {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case p := <-s.packets:
			if pred(decodeTestPacket(t, p.data)) {
				return p.data
			}
		case <-deadline:
			t.Fatal("timed out waiting for a matching HueStream packet")
			return nil
		}
	}
}

// decodeTestPacket checks the HueStream v1 header and returns light id -> RGB.
func decodeTestPacket(t *testing.T, p []byte) map[uint16][3]uint16 {
	t.Helper()
	if len(p) < 16 || (len(p)-16)%9 != 0 {
		t.Fatalf("packet length %d is not 16 + 9n", len(p))
	}
	if string(p[:9]) != "HueStream" {
		t.Fatalf("protocol name = %q, want HueStream", p[:9])
	}
	if p[9] != 0x01 || p[10] != 0x00 {
		t.Fatalf("version = %d.%d, want 1.0", p[9], p[10])
	}
	if p[12] != 0 || p[13] != 0 || p[15] != 0 {
		t.Fatalf("reserved bytes = % x, want zero", []byte{p[12], p[13], p[15]})
	}
	if p[14] != 0x00 {
		t.Fatalf("color space = %d, want 0 (RGB)", p[14])
	}
	out := map[uint16][3]uint16{}
	for off := 16; off < len(p); off += 9 {
		if p[off] != 0x00 {
			t.Fatalf("device type = %d, want 0 (light)", p[off])
		}
		id := binary.BigEndian.Uint16(p[off+1:])
		out[id] = [3]uint16{
			binary.BigEndian.Uint16(p[off+3:]),
			binary.BigEndian.Uint16(p[off+5:]),
			binary.BigEndian.Uint16(p[off+7:]),
		}
	}
	return out
}

func newTestEntertainment(t *testing.T, conf *EntertainmentConfig, clientKey string) (*hueEntertainment, *fakeHueHTTP, error) {
	t.Helper()
	hb := newFakeHueHTTP(t)
	dep := &fakeBridgeDep{name: generic.Named("bridge"), host: hb.host(), clientKey: clientKey}
	deps := resource.Dependencies{dep.name: dep}
	if conf.Bridge == "" {
		conf.Bridge = "bridge"
	}
	raw := resource.Config{Name: "stream", API: generic.API, Model: HueEntertainment, ConvertedAttributes: conf}
	res, err := newHueEntertainment(context.Background(), deps, raw, logging.NewTestLogger(t))
	if err != nil {
		return nil, hb, err
	}
	e := res.(*hueEntertainment)
	t.Cleanup(func() { _ = e.Close(context.Background()) })
	return e, hb, nil
}

func intPtr(i int) *int { return &i }

func rgbFrame(lights map[string][3]float64, bri map[string]float64) map[string]interface{} {
	frame := map[string]interface{}{}
	if lights != nil {
		m := map[string]interface{}{}
		for id, c := range lights {
			m[id] = []interface{}{c[0], c[1], c[2]}
		}
		frame["lights"] = m
	}
	if bri != nil {
		m := map[string]interface{}{}
		for id, b := range bri {
			m[id] = b
		}
		frame["brightness"] = m
	}
	return map[string]interface{}{"frame": frame}
}

func startStreaming(t *testing.T, e *hueEntertainment, srv *fakeStreamServer) {
	t.Helper()
	e.dtlsPort = srv.port
	if _, err := e.DoCommand(context.Background(), map[string]interface{}{"start": true}); err != nil {
		t.Fatalf("start: %v", err)
	}
}

func TestEntertainmentConfigValidate(t *testing.T) {
	cases := []struct {
		name string
		cfg  EntertainmentConfig
		want string
	}{
		{"no bridge", EntertainmentConfig{GroupID: intPtr(200)}, `"bridge"`},
		{"no group", EntertainmentConfig{Bridge: "b"}, `"group_id" or "group_name"`},
		{"both groups", EntertainmentConfig{Bridge: "b", GroupID: intPtr(200), GroupName: "x"}, "only one"},
		{"rate too high", EntertainmentConfig{Bridge: "b", GroupID: intPtr(200), FrameRateHz: 61}, "frame_rate_hz"},
		{"rate negative", EntertainmentConfig{Bridge: "b", GroupID: intPtr(200), FrameRateHz: -1}, "frame_rate_hz"},
	}
	for _, c := range cases {
		if _, _, err := c.cfg.Validate("x"); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want mention of %s", c.name, err, c.want)
		}
	}
	good := EntertainmentConfig{Bridge: "b", GroupName: "rehearsal", FrameRateHz: 60}
	deps, _, err := good.Validate("x")
	if err != nil || len(deps) != 1 || deps[0] != "b" {
		t.Fatalf("valid config: deps %v err %v, want [b] nil", deps, err)
	}
}

func TestSRGBToLinear16(t *testing.T) {
	cases := []struct {
		v, bri float64
		want   uint16
	}{
		{255, 1, 65535},
		{255, 0.5, 32768},
		{0, 1, 0},
		{128, 1, 14146}, // ((128/255 + 0.055) / 1.055)^2.4 * 65535
		{128, 0.5, 7073},
		{10, 1, 199}, // linear segment: 10/255/12.92 * 65535
	}
	for _, c := range cases {
		if got := srgbToLinear16(c.v, c.bri); got != c.want {
			t.Errorf("srgbToLinear16(%v, %v) = %d, want %d", c.v, c.bri, got, c.want)
		}
	}
}

// The handshake uses the username as PSK identity, the hex-decoded clientkey
// as PSK, and AES-128-GCM; packets for two lights carry the exact layout.
func TestEntertainmentHandshakeAndPacketLayout(t *testing.T) {
	srv := newFakeStreamServer(t)
	e, _, err := newTestEntertainment(t, &EntertainmentConfig{GroupID: intPtr(testEntGroupID)}, testStreamClientKey)
	if err != nil {
		t.Fatal(err)
	}
	startStreaming(t, e, srv)

	select {
	case st := <-srv.handshake:
		if string(st.IdentityHint) != testStreamUser {
			t.Errorf("PSK identity = %q, want %q", st.IdentityHint, testStreamUser)
		}
		if st.CipherSuiteID != dtls.TLS_PSK_WITH_AES_128_GCM_SHA256 {
			t.Errorf("cipher = %v, want TLS_PSK_WITH_AES_128_GCM_SHA256", st.CipherSuiteID)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server never completed a handshake")
	}

	// Nothing is sent before the first frame.
	select {
	case p := <-srv.packets:
		t.Fatalf("got a packet before any frame: % x", p.data)
	case <-time.After(100 * time.Millisecond):
	}

	if _, err := e.DoCommand(context.Background(), rgbFrame(
		map[string][3]float64{"7": {255, 0, 0}, "12": {255, 0, 0}},
		map[string]float64{"12": 0.5},
	)); err != nil {
		t.Fatal(err)
	}
	p := srv.waitPacket(t, func(map[uint16][3]uint16) bool { return true })

	want := []byte("HueStream")
	want = append(want, 0x01, 0x00, p[11], 0x00, 0x00, 0x00, 0x00)
	want = append(want, 0x00, 0x00, 0x07, 0xff, 0xff, 0x00, 0x00, 0x00, 0x00) // light 7: 65535,0,0
	want = append(want, 0x00, 0x00, 0x0c, 0x80, 0x00, 0x00, 0x00, 0x00, 0x00) // light 12: 32768,0,0
	if !bytes.Equal(p, want) {
		t.Fatalf("packet\n got % x\nwant % x", p, want)
	}

	// The sequence byte advances.
	p2 := srv.waitPacket(t, func(map[uint16][3]uint16) bool { return true })
	if p2[11] == p[11] {
		t.Errorf("sequence byte did not advance: %d then %d", p[11], p2[11])
	}
}

// Review Focus pin: a frame that omits a light leaves it at its previous
// value, never black; brightness alone rescales the last color.
func TestEntertainmentFrameOmittedLightKeepsValue(t *testing.T) {
	srv := newFakeStreamServer(t)
	e, _, err := newTestEntertainment(t, &EntertainmentConfig{GroupName: "rehearsal"}, testStreamClientKey)
	if err != nil {
		t.Fatal(err)
	}
	startStreaming(t, e, srv)
	ctx := context.Background()

	if _, err := e.DoCommand(ctx, rgbFrame(map[string][3]float64{"7": {255, 0, 0}, "12": {0, 255, 0}}, nil)); err != nil {
		t.Fatal(err)
	}
	srv.waitPacket(t, func(m map[uint16][3]uint16) bool {
		return m[7] == [3]uint16{65535, 0, 0} && m[12] == [3]uint16{0, 65535, 0}
	})

	if _, err := e.DoCommand(ctx, rgbFrame(map[string][3]float64{"7": {0, 0, 255}}, nil)); err != nil {
		t.Fatal(err)
	}
	p := srv.waitPacket(t, func(m map[uint16][3]uint16) bool { return m[7] == [3]uint16{0, 0, 65535} })
	if got := decodeTestPacket(t, p)[12]; got != [3]uint16{0, 65535, 0} {
		t.Fatalf("light 12 omitted from the frame went to %v, want its previous green [0 65535 0]", got)
	}

	if _, err := e.DoCommand(ctx, rgbFrame(nil, map[string]float64{"12": 0.5})); err != nil {
		t.Fatal(err)
	}
	p = srv.waitPacket(t, func(m map[uint16][3]uint16) bool { return m[12] == [3]uint16{0, 32768, 0} })
	if got := decodeTestPacket(t, p)[7]; got != [3]uint16{0, 0, 65535} {
		t.Fatalf("light 7 changed to %v on a brightness-only frame for 12", got)
	}
}

// At the default 50 Hz the loop delivers 50 +/- 5 packets in a measured second.
func TestEntertainmentSendRate(t *testing.T) {
	srv := newFakeStreamServer(t)
	e, _, err := newTestEntertainment(t, &EntertainmentConfig{GroupID: intPtr(testEntGroupID)}, testStreamClientKey)
	if err != nil {
		t.Fatal(err)
	}
	startStreaming(t, e, srv)
	if _, err := e.DoCommand(context.Background(), rgbFrame(map[string][3]float64{"7": {255, 128, 0}}, nil)); err != nil {
		t.Fatal(err)
	}
	srv.waitPacket(t, func(map[uint16][3]uint16) bool { return true })

	start := time.Now()
	end := start.Add(time.Second)
	count := 0
	timeout := time.After(1500 * time.Millisecond)
collect:
	for {
		select {
		case p := <-srv.packets:
			if !p.at.Before(start) && p.at.Before(end) {
				count++
			}
			if p.at.After(end) {
				break collect
			}
		case <-timeout:
			break collect
		}
	}
	if count < 45 || count > 55 {
		t.Fatalf("received %d packets in 1 s at the default rate, want 50 +/- 5", count)
	}
}

// Start PUTs stream.active true once (a second start is a no-op), stop PUTs
// false once (a second stop is a no-op), and status reflects both.
func TestEntertainmentStartStopREST(t *testing.T) {
	srv := newFakeStreamServer(t)
	e, hb, err := newTestEntertainment(t, &EntertainmentConfig{GroupID: intPtr(testEntGroupID)}, testStreamClientKey)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	startStreaming(t, e, srv)
	st, err := e.DoCommand(ctx, map[string]interface{}{"start": true})
	if err != nil {
		t.Fatal(err)
	}
	if st["active"] != true {
		t.Fatalf("status after start = %v, want active", st)
	}
	puts := hb.recordedPuts()
	if len(puts) != 1 || puts[0].path != "/groups/200" || puts[0].body != `{"stream":{"active":true}}` {
		t.Fatalf("PUTs after two starts = %+v, want one /groups/200 {\"stream\":{\"active\":true}}", puts)
	}

	if _, err := e.DoCommand(ctx, rgbFrame(map[string][3]float64{"12": {255, 255, 255}}, nil)); err != nil {
		t.Fatal(err)
	}
	srv.waitPacket(t, func(map[uint16][3]uint16) bool { return true })

	for i := 0; i < 2; i++ {
		st, err = e.DoCommand(ctx, map[string]interface{}{"stop": true})
		if err != nil {
			t.Fatalf("stop %d: %v", i+1, err)
		}
	}
	puts = hb.recordedPuts()
	if len(puts) != 2 || puts[1].path != "/groups/200" || puts[1].body != `{"stream":{"active":false}}` {
		t.Fatalf("PUTs after start and two stops = %+v, want a second /groups/200 {\"stream\":{\"active\":false}}", puts)
	}
	if st["active"] != false || st["last_error"] != nil {
		t.Fatalf("status after stop = %v, want inactive with null last_error", st)
	}
	if n, _ := st["frames_sent"].(int64); n < 1 {
		t.Fatalf("frames_sent = %v, want at least 1", st["frames_sent"])
	}

	// No packets after stop.
	for len(srv.packets) > 0 {
		<-srv.packets
	}
	select {
	case p := <-srv.packets:
		t.Fatalf("packet after stop: % x", p.data)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestEntertainmentWrongGroup(t *testing.T) {
	_, _, err := newTestEntertainment(t, &EntertainmentConfig{GroupID: intPtr(81)}, testStreamClientKey)
	if err == nil {
		t.Fatal("group 81 (a Room) was accepted")
	}
	for _, want := range []string{`"Basement" (id 81)`, `"Room"`, `"rehearsal" (id 200)`, `"Studio sync" (id 201)`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %s", err, want)
		}
	}

	_, _, err = newTestEntertainment(t, &EntertainmentConfig{GroupName: "nope"}, testStreamClientKey)
	if err == nil || !strings.Contains(err.Error(), `"rehearsal" (id 200)`) {
		t.Fatalf("unknown group name: err = %v, want it to list the Entertainment areas", err)
	}
}

func TestEntertainmentMissingClientKey(t *testing.T) {
	_, _, err := newTestEntertainment(t, &EntertainmentConfig{GroupID: intPtr(testEntGroupID)}, "")
	if err == nil || !strings.Contains(err.Error(), `"clientkey"`) || !strings.Contains(err.Error(), "generateclientkey") {
		t.Fatalf("err = %v, want it to name the clientkey attribute and generateclientkey", err)
	}
}

func TestEntertainmentFrameErrors(t *testing.T) {
	e, _, err := newTestEntertainment(t, &EntertainmentConfig{GroupID: intPtr(testEntGroupID)}, testStreamClientKey)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	_, err = e.DoCommand(ctx, rgbFrame(map[string][3]float64{"7": {1, 2, 3}, "19": {255, 0, 0}}, nil))
	if err == nil || !strings.Contains(err.Error(), `"19"`) {
		t.Fatalf("frame with light 19 (not in the group): err = %v, want it to name 19", err)
	}
	// A rejected frame changes nothing.
	if _, ok := e.state[7]; ok {
		t.Fatal("a rejected frame still updated light 7")
	}
	_, err = e.DoCommand(ctx, rgbFrame(nil, map[string]float64{"99": 0.5}))
	if err == nil || !strings.Contains(err.Error(), `"99"`) {
		t.Fatalf("brightness for light 99: err = %v, want it to name 99", err)
	}
	if _, err := e.DoCommand(ctx, rgbFrame(nil, map[string]float64{"7": 1.5})); err == nil {
		t.Fatal("brightness 1.5 was accepted")
	}
}

func TestEntertainmentLightsCommand(t *testing.T) {
	e, _, err := newTestEntertainment(t, &EntertainmentConfig{GroupID: intPtr(testEntGroupID)}, testStreamClientKey)
	if err != nil {
		t.Fatal(err)
	}
	out, err := e.DoCommand(context.Background(), map[string]interface{}{"lights": true})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := json.Marshal(out)
	want := `{"lights":[{"id":7,"name":"Art-Right"},{"id":12,"name":"Art-Center"}]}`
	if string(got) != want {
		t.Fatalf("lights = %s, want %s", got, want)
	}
}
