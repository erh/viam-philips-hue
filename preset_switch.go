package hue

import (
	"context"
	"fmt"
	"math"
	"sync"

	"github.com/amimof/huego"
	"go.viam.com/rdk/logging"
)

// hueTarget is something whose state can be read and set: a single light or a
// group (room/zone).
type hueTarget interface {
	describe() string
	getState(ctx context.Context) (*huego.State, error)
	setState(ctx context.Context, s huego.State) error
}

type lightTarget struct {
	bridge *huego.Bridge
	id     int
}

func (t lightTarget) describe() string { return fmt.Sprintf("light %d", t.id) }

func (t lightTarget) getState(ctx context.Context) (*huego.State, error) {
	l, err := t.bridge.GetLightContext(ctx, t.id)
	if err != nil {
		return nil, fmt.Errorf("failed to get %s: %w", t.describe(), err)
	}
	if l.State == nil {
		return nil, fmt.Errorf("%s returned no state", t.describe())
	}
	return l.State, nil
}

func (t lightTarget) setState(ctx context.Context, s huego.State) error {
	_, err := t.bridge.SetLightStateContext(ctx, t.id, s)
	return err
}

type groupTarget struct {
	bridge *huego.Bridge
	id     int
}

func (t groupTarget) describe() string { return fmt.Sprintf("group %d", t.id) }

func (t groupTarget) getState(ctx context.Context) (*huego.State, error) {
	g, err := t.bridge.GetGroupContext(ctx, t.id)
	if err != nil {
		return nil, fmt.Errorf("failed to get %s: %w", t.describe(), err)
	}
	state := huego.State{}
	if g.State != nil {
		state = *g.State // the group's last "action"
	}
	if g.GroupState != nil {
		state.On = g.GroupState.AnyOn
	}
	return &state, nil
}

func (t groupTarget) setState(ctx context.Context, s huego.State) error {
	_, err := t.bridge.SetGroupStateContext(ctx, t.id, s)
	return err
}

// preset is a named switch position.
type preset struct {
	name  string
	state huego.State
}

// presets are the switch positions shared by hue-light and hue-room.
// Position 0 (off) and 1 (on) are handled specially: "on" applies the
// configured color/brightness when one is set, otherwise just turns on.
var presets = []preset{
	{"off", huego.State{On: false}},
	{"on", huego.State{On: true}},
	{"dim", huego.State{On: true, Bri: 64}},
	{"bright", huego.State{On: true, Bri: 254}},
	{"warm", huego.State{On: true, Bri: 200, Ct: 370}},
	{"daylight", huego.State{On: true, Bri: 254, Ct: 153}},
	{"white", huego.State{On: true, Bri: 254, Ct: 250}},
	{"red", colorState(255, 0, 0)},
	{"orange", colorState(255, 128, 0)},
	{"yellow", colorState(255, 255, 0)},
	{"green", colorState(0, 255, 0)},
	{"cyan", colorState(0, 255, 255)},
	{"blue", colorState(0, 0, 255)},
	{"purple", colorState(128, 0, 255)},
	{"pink", colorState(255, 0, 128)},
	{"colorloop", huego.State{On: true, Effect: "colorloop"}},
}

func presetNames(ps []preset) []string {
	names := make([]string, len(ps))
	for i, p := range ps {
		names[i] = p.name
	}
	return names
}

// onStateFromConfig builds the state applied at startup and by position "on"
// from an optional hex color and brightness percentage.
func onStateFromConfig(color string, brightness int) (*huego.State, error) {
	if color == "" && brightness == 0 {
		return nil, nil
	}
	if brightness < 0 || brightness > 100 {
		return nil, fmt.Errorf("brightness must be 1-100, got %d", brightness)
	}
	s := huego.State{On: true}
	if color != "" {
		r, g, b, err := parseHexColor(color)
		if err != nil {
			return nil, err
		}
		x, y := rgbToXY(r, g, b)
		s.Xy = []float32{x, y}
		s.Bri = maxToBri(maxUint8(r, g, b))
	}
	if brightness > 0 {
		s.Bri = percentToBri(brightness)
	}
	if s.Bri == 0 {
		s.Bri = 254
	}
	return &s, nil
}

// presetSwitch implements the switch API and DoCommand on top of a hueTarget.
// With a configured color/brightness (onState) the switch is a plain two
// position off/on toggle; otherwise every preset is a position.
type presetSwitch struct {
	mu      sync.Mutex
	target  hueTarget
	logger  logging.Logger
	onState *huego.State // state for position "on"; nil means just turn on
	lastPos uint32       // last position set through SetPosition or DoCommand
}

// apply writes a state to the target. If a colorloop is running and the new
// state does not keep it, the effect is stopped in a separate request first:
// the bridge ignores color fields sent alongside an effect change.
func (p *presetSwitch) apply(ctx context.Context, s huego.State) error {
	cur, err := p.target.getState(ctx)
	if err != nil {
		return err
	}
	if cur.Effect == "colorloop" && s.Effect != "colorloop" {
		if err := p.target.setState(ctx, huego.State{On: true, Effect: "none"}); err != nil {
			return fmt.Errorf("failed to stop effect on %s: %w", p.target.describe(), err)
		}
	}
	if err := p.target.setState(ctx, s); err != nil {
		return fmt.Errorf("failed to set %s: %w", p.target.describe(), err)
	}
	return nil
}

// applyOnState applies the configured color/brightness (used at startup).
func (p *presetSwitch) applyOnState(ctx context.Context) error {
	if p.onState == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.apply(ctx, *p.onState); err != nil {
		return err
	}
	p.lastPos = 1
	return nil
}

// positions returns the presets this switch exposes.
func (p *presetSwitch) positions() []preset {
	if p.onState != nil {
		return presets[:2]
	}
	return presets
}

func (p *presetSwitch) SetPosition(ctx context.Context, position uint32, extra map[string]interface{}) error {
	ps := p.positions()
	if int(position) >= len(ps) {
		return fmt.Errorf("position must be 0-%d, got %d", len(ps)-1, position)
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	s := ps[position].state
	if position == 1 && p.onState != nil {
		s = *p.onState
	}
	if err := p.apply(ctx, s); err != nil {
		return err
	}
	p.lastPos = position
	return nil
}

func (p *presetSwitch) GetPosition(ctx context.Context, extra map[string]interface{}) (uint32, error) {
	cur, err := p.target.getState(ctx)
	if err != nil {
		return 0, err
	}
	if !cur.On {
		return 0, nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.lastPos == 0 {
		return 1, nil // turned on outside of this switch
	}
	return p.lastPos, nil
}

func (p *presetSwitch) GetNumberOfPositions(ctx context.Context, extra map[string]interface{}) (uint32, []string, error) {
	ps := p.positions()
	return uint32(len(ps)), presetNames(ps), nil
}

// DoCommand sets any combination of:
//
//	"on":         bool
//	"color":      "#RRGGBB" or [r, g, b]
//	"brightness": 0-100 (0 turns the light off)
//	"kelvin":     color temperature in Kelvin (2000-6500)
//	"ct":         color temperature in mireds (153-500)
//	"effect":     "colorloop" or "none"
//	"transition": seconds for the change to take
//
// An empty command (or {"get": true}) just returns the current state.
func (p *presetSwitch) DoCommand(ctx context.Context, cmd map[string]interface{}) (map[string]interface{}, error) {
	s := huego.State{On: true}
	touched := false

	if v, ok := cmd["on"]; ok {
		on, isBool := v.(bool)
		if !isBool {
			return nil, fmt.Errorf(`"on" must be true or false`)
		}
		s.On = on
		touched = true
	}
	if v, ok := cmd["color"]; ok {
		r, g, b, err := parseColorValue(v)
		if err != nil {
			return nil, err
		}
		x, y := rgbToXY(r, g, b)
		s.Xy = []float32{x, y}
		s.Bri = maxToBri(maxUint8(r, g, b))
		touched = true
	}
	if v, ok := cmd["brightness"]; ok {
		pct, err := toInt(v)
		if err != nil {
			return nil, fmt.Errorf(`"brightness": %w`, err)
		}
		if pct < 0 || pct > 100 {
			return nil, fmt.Errorf(`"brightness" must be 0-100, got %d`, pct)
		}
		if pct == 0 {
			s.On = false
		} else {
			s.Bri = percentToBri(pct)
		}
		touched = true
	}
	if v, ok := cmd["kelvin"]; ok {
		k, err := toInt(v)
		if err != nil || k <= 0 {
			return nil, fmt.Errorf(`"kelvin" must be a positive number`)
		}
		s.Ct = uint16(math.Round(1e6 / float64(k)))
		touched = true
	}
	if v, ok := cmd["ct"]; ok {
		ct, err := toInt(v)
		if err != nil || ct <= 0 {
			return nil, fmt.Errorf(`"ct" must be a positive number of mireds`)
		}
		s.Ct = uint16(ct)
		touched = true
	}
	if v, ok := cmd["effect"]; ok {
		effect, _ := v.(string)
		if effect != "colorloop" && effect != "none" {
			return nil, fmt.Errorf(`"effect" must be "colorloop" or "none"`)
		}
		s.Effect = effect
		touched = true
	}
	if v, ok := cmd["transition"]; ok {
		sec, err := toFloat(v)
		if err != nil || sec < 0 {
			return nil, fmt.Errorf(`"transition" must be a number of seconds`)
		}
		s.TransitionTime = uint16(math.Round(sec * 10))
	}

	if touched {
		p.mu.Lock()
		if err := p.apply(ctx, s); err != nil {
			p.mu.Unlock()
			return nil, err
		}
		if s.On {
			p.lastPos = 1
		} else {
			p.lastPos = 0
		}
		p.mu.Unlock()
	}

	return p.Status(ctx)
}

// Status reports the target's current state in friendly units.
func (p *presetSwitch) Status(ctx context.Context) (map[string]interface{}, error) {
	cur, err := p.target.getState(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string]interface{}{
		"on":         cur.On,
		"brightness": briToPercent(cur.Bri),
		"colormode":  cur.ColorMode,
		"effect":     cur.Effect,
	}
	// Report the hue at full brightness so "color" and "brightness" are
	// independent: a light configured as #FF8800 at 60% reads back as #FF8800
	// and 60, not as a darkened hex value. Color temperature is only
	// meaningful (and only current) when the light is in ct mode.
	switch cur.ColorMode {
	case "ct":
		if cur.Ct > 0 {
			out["ct"] = int(cur.Ct)
			out["kelvin"] = int(math.Round(1e6 / float64(cur.Ct)))
		}
	default:
		r, g, b := xyBriToRGB(cur.Xy, 254)
		out["color"] = hexColor(r, g, b)
	}
	p.mu.Lock()
	pos := p.lastPos
	p.mu.Unlock()
	if !cur.On {
		pos = 0
	} else if pos == 0 {
		pos = 1
	}
	out["position"] = int(pos)
	out["preset"] = presets[pos].name
	return out, nil
}

func parseColorValue(v interface{}) (r, g, b uint8, err error) {
	switch c := v.(type) {
	case string:
		return parseHexColor(c)
	case []interface{}:
		if len(c) != 3 {
			return 0, 0, 0, fmt.Errorf(`"color" list must have 3 values, got %d`, len(c))
		}
		var out [3]uint8
		for i, e := range c {
			n, err := toInt(e)
			if err != nil || n < 0 || n > 255 {
				return 0, 0, 0, fmt.Errorf(`"color" values must be 0-255`)
			}
			out[i] = uint8(n)
		}
		return out[0], out[1], out[2], nil
	default:
		return 0, 0, 0, fmt.Errorf(`"color" must be a hex string like "#FF8800" or [r, g, b]`)
	}
}

func toFloat(v interface{}) (float64, error) {
	switch n := v.(type) {
	case float64:
		return n, nil
	case float32:
		return float64(n), nil
	case int:
		return float64(n), nil
	case int64:
		return float64(n), nil
	case int32:
		return float64(n), nil
	case uint32:
		return float64(n), nil
	default:
		return 0, fmt.Errorf("expected a number, got %T", v)
	}
}

func toInt(v interface{}) (int, error) {
	f, err := toFloat(v)
	if err != nil {
		return 0, err
	}
	if f != math.Trunc(f) {
		return 0, fmt.Errorf("expected a whole number, got %v", f)
	}
	return int(f), nil
}
