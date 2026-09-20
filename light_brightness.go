package hue

import (
	"context"
	"fmt"
	"math"
	"sync"

	"github.com/amimof/huego"
	toggleswitch "go.viam.com/rdk/components/switch"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/resource"
)

var HueLightBrightness = family.WithModel("hue-light-brightness")

func init() {
	resource.RegisterComponent(toggleswitch.API, HueLightBrightness,
		resource.Registration[toggleswitch.Switch, *LightBrightnessConfig]{
			Constructor: newHueLightBrightness,
		},
	)
}

type LightBrightnessConfig struct {
	BridgeHost string `json:"bridge_host,omitempty"`
	Username   string `json:"username"`
	LightID    int    `json:"light_id"`
}

func (cfg *LightBrightnessConfig) Validate(path string) ([]string, []string, error) {
	if cfg.Username == "" {
		return nil, nil, errMissingUsername()
	}
	if cfg.LightID == 0 {
		return nil, nil, fmt.Errorf("need a light_id")
	}
	return nil, nil, nil
}

type hueLightBrightness struct {
	resource.AlwaysRebuild
	resource.TriviallyCloseable

	name   resource.Name
	logger logging.Logger
	cfg    *LightBrightnessConfig

	bridge *huego.Bridge

	mu      sync.Mutex
	lastBri uint8 // last brightness set via positions 2-100, used by position 1 to restore
}

func newHueLightBrightness(ctx context.Context, deps resource.Dependencies, rawConf resource.Config, logger logging.Logger) (toggleswitch.Switch, error) {
	conf, err := resource.NativeConfig[*LightBrightnessConfig](rawConf)
	if err != nil {
		return nil, err
	}

	s := &hueLightBrightness{
		name:   rawConf.ResourceName(),
		logger: logger,
		cfg:    conf,
	}

	var light *huego.Light
	s.bridge, light, err = connectToLight(conf.BridgeHost, conf.Username, conf.LightID, logger)
	if err != nil {
		return nil, err
	}

	s.lastBri = light.State.Bri
	if s.lastBri == 0 {
		s.lastBri = 254
	}

	return s, nil
}

func (s *hueLightBrightness) Name() resource.Name {
	return s.name
}

func (s *hueLightBrightness) DoCommand(ctx context.Context, cmd map[string]interface{}) (map[string]interface{}, error) {
	return map[string]interface{}{}, nil
}

func (s *hueLightBrightness) Status(ctx context.Context) (map[string]interface{}, error) {
	return map[string]interface{}{}, nil
}

// SetPosition controls on/off and brightness.
// 0 = off. 1 = on at last-set brightness. 2-100 map to brightness levels (Hue Bri 1-254).
func (s *hueLightBrightness) SetPosition(ctx context.Context, position uint32, extra map[string]interface{}) error {
	if position > 100 {
		return fmt.Errorf("position must be 0-100, got %d", position)
	}

	light, err := s.bridge.GetLight(s.cfg.LightID)
	if err != nil {
		return fmt.Errorf("failed to get light state: %w", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if position == 0 {
		return light.SetState(huego.State{On: false})
	}
	if position == 1 {
		return light.SetState(huego.State{On: true, Bri: s.lastBri})
	}

	// Map 2-100 linearly to Hue brightness range 1-254.
	bri := max(uint8(math.Round(float64(position-2)/98.0*253.0)), 1)
	if err := light.SetState(huego.State{On: true, Bri: bri}); err != nil {
		return err
	}
	s.lastBri = bri
	return nil
}

func (s *hueLightBrightness) GetPosition(ctx context.Context, extra map[string]interface{}) (uint32, error) {
	light, err := s.bridge.GetLight(s.cfg.LightID)
	if err != nil {
		return 0, fmt.Errorf("failed to get light state: %w", err)
	}
	if light.State == nil || !light.State.On {
		return 0, nil
	}

	if light.State.Bri >= 254 {
		return 1, nil
	}

	// Map Hue brightness 1-253 back to position 2-100.
	pos := uint32(math.Round(float64(light.State.Bri)/253.0*98.0)) + 2
	if pos > 100 {
		pos = 100
	}
	return pos, nil
}

func (s *hueLightBrightness) GetNumberOfPositions(ctx context.Context, extra map[string]interface{}) (uint32, []string, error) {
	// 0 = off, 1-100 = brightness levels
	return 101, nil, nil
}
