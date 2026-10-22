package hue

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/amimof/huego"
	"github.com/pion/dtls/v3"
	"go.viam.com/rdk/components/generic"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/resource"
)

// HueEntertainment streams colors to the lights of a Hue Entertainment area
// over the Entertainment API (HueStream v1 over DTLS), which is fast enough for
// smooth animation where the REST API is limited to a few updates per second.
var HueEntertainment = family.WithModel("hue-entertainment")

func init() {
	resource.RegisterComponent(generic.API, HueEntertainment,
		resource.Registration[resource.Resource, *EntertainmentConfig]{
			Constructor: newHueEntertainment,
		},
	)
}

const (
	// hueStreamPort is the bridge's DTLS port for Entertainment streaming.
	hueStreamPort = 2100
	// defaultStreamFrameRate is the default send rate. The bridge forwards
	// about 25 updates per second to the lights, and Signify recommends
	// sending at 50-60 Hz so dropped UDP packets are covered.
	defaultStreamFrameRate = 50
	maxStreamFrameRate     = 60
	// streamHandshakeTimeout bounds the DTLS handshake on start.
	streamHandshakeTimeout = 5 * time.Second
	// entertainmentGroupType is the v1 group type of an Entertainment area.
	entertainmentGroupType = "Entertainment"
)

type EntertainmentConfig struct {
	Bridge string `json:"bridge"` // name of a hue-bridge component (must have clientkey)

	// Identify the Entertainment area by v1 group ID or by name (as shown in
	// the Hue app). Exactly one is required.
	GroupID   *int   `json:"group_id,omitempty"`
	GroupName string `json:"group_name,omitempty"`

	// FrameRateHz is how many frames per second are sent while streaming
	// (default 50, 1-60).
	FrameRateHz int `json:"frame_rate_hz,omitempty"`
}

func (cfg *EntertainmentConfig) Validate(path string) ([]string, []string, error) {
	if cfg.Bridge == "" {
		return nil, nil, fmt.Errorf(`need "bridge": the name of a %s component (with "clientkey" set) to stream through`, HueBridge)
	}
	if cfg.GroupID == nil && cfg.GroupName == "" {
		return nil, nil, errors.New(`need "group_id" or "group_name" of a Hue Entertainment area`)
	}
	if cfg.GroupID != nil && cfg.GroupName != "" {
		return nil, nil, errors.New(`set only one of "group_id" or "group_name"`)
	}
	if cfg.FrameRateHz != 0 && (cfg.FrameRateHz < 1 || cfg.FrameRateHz > maxStreamFrameRate) {
		return nil, nil, fmt.Errorf(`"frame_rate_hz" must be between 1 and %d, got %d`, maxStreamFrameRate, cfg.FrameRateHz)
	}
	return []string{cfg.Bridge}, nil, nil
}

// lightColor is the last color and brightness set for one light.
type lightColor struct {
	rgb        [3]float64 // sRGB 0-255
	hasColor   bool
	brightness float64 // 0-1
}

type hueEntertainment struct {
	resource.AlwaysRebuild

	name     resource.Name
	logger   logging.Logger
	bridge   *huego.Bridge
	host     string // bridge address without scheme or port
	username string
	psk      []byte

	groupID   int
	groupName string
	lightIDs  []int // in group order
	inGroup   map[int]bool
	period    time.Duration
	dtlsPort  int // overridable by tests

	startStopMu sync.Mutex // serializes start, stop, and Close

	mu         sync.Mutex
	state      map[int]*lightColor
	active     bool
	conn       *dtls.Conn
	cancelLoop context.CancelFunc
	loopDone   chan struct{}
	seq        byte
	framesSent int64
	lastErr    string
}

func newHueEntertainment(ctx context.Context, deps resource.Dependencies, rawConf resource.Config, logger logging.Logger) (resource.Resource, error) {
	conf, err := resource.NativeConfig[*EntertainmentConfig](rawConf)
	if err != nil {
		return nil, err
	}

	host, username, clientKey, err := entertainmentBridgeInfo(ctx, deps, conf.Bridge)
	if err != nil {
		return nil, err
	}
	psk, err := hex.DecodeString(clientKey)
	if err != nil || len(psk) != 16 {
		return nil, fmt.Errorf(`bridge %q has an invalid "clientkey": it must be the 32 hex digits returned by a registration with "generateclientkey": true`, conf.Bridge)
	}

	bridge := huego.New(host, username)
	group, err := findEntertainmentGroup(ctx, bridge, conf)
	if err != nil {
		return nil, err
	}

	rate := conf.FrameRateHz
	if rate == 0 {
		rate = defaultStreamFrameRate
	}

	e := &hueEntertainment{
		name:      rawConf.ResourceName(),
		logger:    logger,
		bridge:    bridge,
		host:      streamHost(host),
		username:  username,
		psk:       psk,
		groupID:   group.ID,
		groupName: group.Name,
		lightIDs:  groupLightIDs(*group),
		inGroup:   map[int]bool{},
		period:    time.Second / time.Duration(rate),
		dtlsPort:  hueStreamPort,
		state:     map[int]*lightColor{},
	}
	for _, id := range e.lightIDs {
		e.inGroup[id] = true
	}
	return e, nil
}

// entertainmentBridgeInfo asks the named hue-bridge dependency for its
// connection details, including the Entertainment client key.
func entertainmentBridgeInfo(ctx context.Context, deps resource.Dependencies, bridgeName string) (host, username, clientKey string, err error) {
	var dep resource.Resource
	for n, r := range deps {
		if n.Name == bridgeName || n.ShortName() == bridgeName || n.String() == bridgeName {
			dep = r
			break
		}
	}
	if dep == nil {
		return "", "", "", fmt.Errorf("bridge %q not found in dependencies; add a %s component with that name", bridgeName, HueBridge)
	}

	info, err := dep.DoCommand(ctx, map[string]interface{}{"command": "bridge_info"})
	if err != nil {
		return "", "", "", fmt.Errorf("bridge %q did not return connection info: %w", bridgeName, err)
	}
	host, _ = info["bridge_host"].(string)
	username, _ = info["username"].(string)
	clientKey, _ = info["clientkey"].(string)
	if host == "" || username == "" {
		return "", "", "", fmt.Errorf("bridge %q returned incomplete connection info", bridgeName)
	}
	if clientKey == "" {
		return "", "", "", fmt.Errorf(`bridge %q has no "clientkey"; Entertainment streaming needs one.
Set "clientkey" on the %s component to the key from a registration that asked for it: press the link button on the bridge, then within 30 seconds run:
  curl -X POST http://<bridge-ip>/api -d '{"devicetype":"viam#module","generateclientkey":true}'
and use the returned "username" and "clientkey" (the hue-discovery service does this for you)`, bridgeName, HueBridge)
	}
	return host, username, clientKey, nil
}

// findEntertainmentGroup resolves the configured group and checks that it is
// an Entertainment area.
func findEntertainmentGroup(ctx context.Context, bridge *huego.Bridge, conf *EntertainmentConfig) (*huego.Group, error) {
	groups, err := bridge.GetGroupsContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("can't list groups on Hue bridge: %w", err)
	}
	sort.Slice(groups, func(i, j int) bool { return groups[i].ID < groups[j].ID })

	var match *huego.Group
	if conf.GroupID != nil {
		for i := range groups {
			if groups[i].ID == *conf.GroupID {
				match = &groups[i]
				break
			}
		}
		if match == nil {
			return nil, fmt.Errorf("no Hue group with id %d; %s", *conf.GroupID, describeEntertainmentGroups(groups))
		}
	} else {
		// A room and an Entertainment area can share a name, so prefer the
		// Entertainment one.
		want := strings.ToLower(strings.TrimSpace(conf.GroupName))
		for i := range groups {
			g := &groups[i]
			if strings.ToLower(g.Name) == want || sanitizeName(g.Name) == conf.GroupName {
				if match == nil || (match.Type != entertainmentGroupType && g.Type == entertainmentGroupType) {
					match = g
				}
			}
		}
		if match == nil {
			return nil, fmt.Errorf("no Hue group named %q; %s", conf.GroupName, describeEntertainmentGroups(groups))
		}
	}

	if match.Type != entertainmentGroupType {
		return nil, fmt.Errorf("Hue group %q (id %d) is type %q, not %s; %s",
			match.Name, match.ID, match.Type, entertainmentGroupType, describeEntertainmentGroups(groups))
	}
	return match, nil
}

func describeEntertainmentGroups(groups []huego.Group) string {
	var found []string
	for _, g := range groups {
		if g.Type == entertainmentGroupType {
			found = append(found, fmt.Sprintf("%q (id %d)", g.Name, g.ID))
		}
	}
	if len(found) == 0 {
		return "the bridge has no Entertainment areas; create one in the Hue app (Settings > Entertainment areas)"
	}
	return "Entertainment areas on the bridge: " + strings.Join(found, ", ")
}

// streamHost strips any scheme, path, and port from a bridge_host value.
func streamHost(bridgeHost string) string {
	h := bridgeHost
	if i := strings.Index(h, "://"); i >= 0 {
		h = h[i+3:]
	}
	if i := strings.Index(h, "/"); i >= 0 {
		h = h[:i]
	}
	if host, _, err := net.SplitHostPort(h); err == nil {
		return host
	}
	return strings.Trim(h, "[]")
}

func (e *hueEntertainment) Name() resource.Name {
	return e.name
}

func (e *hueEntertainment) Close(ctx context.Context) error {
	return e.stop(ctx)
}

func (e *hueEntertainment) Status(ctx context.Context) (map[string]interface{}, error) {
	return e.status(), nil
}

// DoCommand supports:
//
//	{"start": true}  -> activate streaming on the group, connect, and send
//	                    the current frame at frame_rate_hz (no-op if active)
//	{"stop": true}   -> stop sending, disconnect, deactivate streaming
//	{"frame": {"lights": {"<id>": [r,g,b]}, "brightness": {"<id>": 0-1}}}
//	                 -> update some lights; omitted lights keep their values
//	{"lights": true} -> the group's v1 light ids and names
//	{"status": true} -> active, frames_sent, last_error
func (e *hueEntertainment) DoCommand(ctx context.Context, cmd map[string]interface{}) (map[string]interface{}, error) {
	if _, ok := cmd["start"]; ok {
		if err := e.start(ctx); err != nil {
			return nil, err
		}
		return e.status(), nil
	}
	if _, ok := cmd["stop"]; ok {
		if err := e.stop(ctx); err != nil {
			return nil, err
		}
		return e.status(), nil
	}
	if frame, ok := cmd["frame"]; ok {
		if err := e.applyFrame(frame); err != nil {
			return nil, err
		}
		return map[string]interface{}{}, nil
	}
	if _, ok := cmd["lights"]; ok {
		return e.lightsInfo(ctx)
	}
	if _, ok := cmd["status"]; ok {
		return e.status(), nil
	}
	return nil, errors.New(`unknown command; expected one of "start", "stop", "frame", "lights", or "status"`)
}

func (e *hueEntertainment) status() map[string]interface{} {
	e.mu.Lock()
	defer e.mu.Unlock()
	var lastErr interface{}
	if e.lastErr != "" {
		lastErr = e.lastErr
	}
	return map[string]interface{}{
		"active":      e.active,
		"frames_sent": e.framesSent,
		"last_error":  lastErr,
	}
}

func (e *hueEntertainment) setLastError(err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.lastErr = err.Error()
}

func (e *hueEntertainment) lightsInfo(ctx context.Context) (map[string]interface{}, error) {
	lights, err := e.bridge.GetLightsContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("can't list lights on Hue bridge: %w", err)
	}
	names := make(map[int]string, len(lights))
	for _, l := range lights {
		names[l.ID] = l.Name
	}
	out := make([]interface{}, 0, len(e.lightIDs))
	for _, id := range e.lightIDs {
		out = append(out, map[string]interface{}{"id": id, "name": names[id]})
	}
	return map[string]interface{}{"lights": out}, nil
}

// applyFrame validates a whole frame and then merges it into the light state.
// Lights the frame does not mention keep their previous color and brightness.
func (e *hueEntertainment) applyFrame(raw interface{}) error {
	frame, ok := raw.(map[string]interface{})
	if !ok {
		return errors.New(`"frame" must be an object with optional "lights" and "brightness"`)
	}

	colors := map[int][3]float64{}
	if v, ok := frame["lights"]; ok && v != nil {
		m, ok := v.(map[string]interface{})
		if !ok {
			return errors.New(`"frame.lights" must map light ids to [r, g, b]`)
		}
		for key, val := range m {
			id, err := e.frameLightID(key)
			if err != nil {
				return err
			}
			rgb, err := parseFrameRGB(val)
			if err != nil {
				return fmt.Errorf("light %q: %w", key, err)
			}
			colors[id] = rgb
		}
	}

	bris := map[int]float64{}
	if v, ok := frame["brightness"]; ok && v != nil {
		m, ok := v.(map[string]interface{})
		if !ok {
			return errors.New(`"frame.brightness" must map light ids to 0-1`)
		}
		for key, val := range m {
			id, err := e.frameLightID(key)
			if err != nil {
				return err
			}
			b, ok := frameNumber(val)
			if !ok || b < 0 || b > 1 {
				return fmt.Errorf("light %q: brightness must be a number from 0 to 1, got %v", key, val)
			}
			bris[id] = b
		}
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	for id, rgb := range colors {
		lc := e.lightState(id)
		lc.rgb = rgb
		lc.hasColor = true
	}
	for id, b := range bris {
		e.lightState(id).brightness = b
	}
	return nil
}

// lightState returns the state for a light, creating it at full brightness.
// Callers hold e.mu.
func (e *hueEntertainment) lightState(id int) *lightColor {
	lc, ok := e.state[id]
	if !ok {
		lc = &lightColor{brightness: 1}
		e.state[id] = lc
	}
	return lc
}

func (e *hueEntertainment) frameLightID(key string) (int, error) {
	id, err := strconv.Atoi(strings.TrimSpace(key))
	if err != nil || !e.inGroup[id] {
		return 0, fmt.Errorf("light %q is not in Entertainment area %q (id %d); its lights are %v", key, e.groupName, e.groupID, e.lightIDs)
	}
	return id, nil
}

func parseFrameRGB(v interface{}) ([3]float64, error) {
	var rgb [3]float64
	list, ok := v.([]interface{})
	if !ok || len(list) != 3 {
		return rgb, fmt.Errorf("color must be [r, g, b] with values 0-255, got %v", v)
	}
	for i, c := range list {
		f, ok := frameNumber(c)
		if !ok || f < 0 || f > 255 {
			return rgb, fmt.Errorf("color must be [r, g, b] with values 0-255, got %v", v)
		}
		rgb[i] = f
	}
	return rgb, nil
}

func frameNumber(v interface{}) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int32:
		return float64(n), true
	case int64:
		return float64(n), true
	case uint8:
		return float64(n), true
	default:
		return 0, false
	}
}

// start activates streaming on the group, completes the DTLS handshake, and
// launches the send loop. Starting while active does nothing.
func (e *hueEntertainment) start(ctx context.Context) error {
	e.startStopMu.Lock()
	defer e.startStopMu.Unlock()

	e.mu.Lock()
	active := e.active
	e.mu.Unlock()
	if active {
		return nil
	}

	if err := e.setStreamActive(ctx, true); err != nil {
		err = fmt.Errorf("can't activate streaming on Entertainment area %q (id %d): %w", e.groupName, e.groupID, err)
		e.setLastError(err)
		return err
	}

	conn, err := e.dial(ctx)
	if err != nil {
		err = fmt.Errorf("can't open Entertainment stream to %s: %w", net.JoinHostPort(e.host, strconv.Itoa(e.dtlsPort)), err)
		e.setLastError(err)
		if offErr := e.setStreamActive(ctx, false); offErr != nil {
			e.logger.Warnf("deactivating streaming after failed connect: %v", offErr)
		}
		return err
	}

	loopCtx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	e.mu.Lock()
	e.active = true
	e.conn = conn
	e.cancelLoop = cancel
	e.loopDone = done
	e.lastErr = ""
	e.mu.Unlock()

	go e.sendLoop(loopCtx, conn, done)
	return nil
}

func (e *hueEntertainment) dial(ctx context.Context) (*dtls.Conn, error) {
	addr, err := net.ResolveUDPAddr("udp", net.JoinHostPort(e.host, strconv.Itoa(e.dtlsPort)))
	if err != nil {
		return nil, err
	}
	psk := e.psk
	conn, err := dtls.DialWithOptions("udp", addr,
		dtls.WithPSK(func([]byte) ([]byte, error) { return psk, nil }),
		dtls.WithPSKIdentityHint([]byte(e.username)),
		dtls.WithCipherSuites(dtls.TLS_PSK_WITH_AES_128_GCM_SHA256),
	)
	if err != nil {
		return nil, err
	}
	hsCtx, cancel := context.WithTimeout(ctx, streamHandshakeTimeout)
	defer cancel()
	if err := conn.HandshakeContext(hsCtx); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("DTLS handshake failed (check the bridge's clientkey): %w", err)
	}
	return conn, nil
}

// stop ends the send loop, closes the DTLS connection, and deactivates
// streaming on the group. Stopping while inactive does nothing.
func (e *hueEntertainment) stop(ctx context.Context) error {
	e.startStopMu.Lock()
	defer e.startStopMu.Unlock()

	e.mu.Lock()
	if !e.active {
		e.mu.Unlock()
		return nil
	}
	e.active = false
	conn, cancel, done := e.conn, e.cancelLoop, e.loopDone
	e.conn, e.cancelLoop, e.loopDone = nil, nil, nil
	e.mu.Unlock()

	cancel()
	<-done
	var errs []error
	if err := conn.Close(); err != nil {
		errs = append(errs, fmt.Errorf("closing Entertainment stream: %w", err))
	}
	if err := e.setStreamActive(ctx, false); err != nil {
		errs = append(errs, fmt.Errorf("can't deactivate streaming on Entertainment area %q (id %d): %w", e.groupName, e.groupID, err))
	}
	if err := errors.Join(errs...); err != nil {
		e.setLastError(err)
		return err
	}
	return nil
}

// setStreamActive PUTs {"stream":{"active":<active>}} to the group.
func (e *hueEntertainment) setStreamActive(ctx context.Context, active bool) error {
	_, err := e.bridge.UpdateGroupContext(ctx, e.groupID, huego.Group{
		Stream: &huego.Stream{ActiveRaw: &active},
	})
	return err
}

func (e *hueEntertainment) sendLoop(ctx context.Context, conn *dtls.Conn, done chan struct{}) {
	defer close(done)
	ticker := time.NewTicker(e.period)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		msg := e.nextMessage()
		if msg == nil {
			continue
		}
		if _, err := conn.Write(msg); err != nil {
			if ctx.Err() != nil {
				return
			}
			e.mu.Lock()
			changed := e.lastErr != err.Error()
			e.lastErr = err.Error()
			e.mu.Unlock()
			if changed {
				e.logger.Warnf("Entertainment stream send failed: %v", err)
			}
			continue
		}
		e.mu.Lock()
		e.framesSent++
		e.mu.Unlock()
	}
}

// nextMessage encodes the current state of every light that has a color, or
// returns nil when none has one yet.
func (e *hueEntertainment) nextMessage() []byte {
	e.mu.Lock()
	defer e.mu.Unlock()
	lights := make([]streamLight, 0, len(e.lightIDs))
	for _, id := range e.lightIDs {
		lc, ok := e.state[id]
		if !ok || !lc.hasColor {
			continue
		}
		lights = append(lights, streamLight{
			id: uint16(id),
			r:  srgbToLinear16(lc.rgb[0], lc.brightness),
			g:  srgbToLinear16(lc.rgb[1], lc.brightness),
			b:  srgbToLinear16(lc.rgb[2], lc.brightness),
		})
	}
	if len(lights) == 0 {
		return nil
	}
	msg := encodeHueStreamV1(e.seq, lights)
	e.seq++
	return msg
}
