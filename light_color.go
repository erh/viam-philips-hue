package hue

import (
	"context"
	"fmt"

	"github.com/amimof/huego"
	toggleswitch "go.viam.com/rdk/components/switch"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/resource"
)

var HueLightColor = family.WithModel("hue-light-color")

func init() {
	resource.RegisterComponent(toggleswitch.API, HueLightColor,
		resource.Registration[toggleswitch.Switch, *LightColorConfig]{
			Constructor: newHueLightColor,
		},
	)
}

type LightColorConfig struct {
	BridgeHost string `json:"bridge_host,omitempty"`
	Username   string `json:"username"`
	LightID    int    `json:"light_id"`
	Channel    string `json:"channel"` // "red", "green", or "blue"
}

func (cfg *LightColorConfig) Validate(path string) ([]string, []string, error) {
	if cfg.Username == "" {
		return nil, nil, errMissingUsername()
	}
	if cfg.LightID == 0 {
		return nil, nil, fmt.Errorf("need a light_id")
	}
	switch cfg.Channel {
	case "red", "green", "blue":
	default:
		return nil, nil, fmt.Errorf("channel must be \"red\", \"green\", or \"blue\", got %q", cfg.Channel)
	}
	return nil, nil, nil
}

type hueLightColor struct {
	resource.AlwaysRebuild
	resource.TriviallyCloseable

	name   resource.Name
	logger logging.Logger
	cfg    *LightColorConfig

	bridge *huego.Bridge
}

func newHueLightColor(ctx context.Context, deps resource.Dependencies, rawConf resource.Config, logger logging.Logger) (toggleswitch.Switch, error) {
	conf, err := resource.NativeConfig[*LightColorConfig](rawConf)
	if err != nil {
		return nil, err
	}

	s := &hueLightColor{
		name:   rawConf.ResourceName(),
		logger: logger,
		cfg:    conf,
	}

	var light *huego.Light
	s.bridge, light, err = connectToLight(conf.BridgeHost, conf.Username, conf.LightID, logger)
	if err != nil {
		return nil, err
	}
	if !lightSupportsColor(*light) {
		logger.Warnf("light %d (%s, type %q) does not appear to support color; color changes may be rejected", light.ID, light.Name, light.Type)
	}

	return s, nil
}

func (s *hueLightColor) Name() resource.Name {
	return s.name
}

func (s *hueLightColor) DoCommand(ctx context.Context, cmd map[string]interface{}) (map[string]interface{}, error) {
	return map[string]interface{}{}, nil
}

func (s *hueLightColor) Status(ctx context.Context) (map[string]interface{}, error) {
	return map[string]interface{}{}, nil
}

// SetPosition sets the configured RGB channel to the given value.
// Position maps 1-to-1 to the channel value (0–255).
func (s *hueLightColor) SetPosition(ctx context.Context, position uint32, extra map[string]interface{}) error {
	if position > 255 {
		return fmt.Errorf("position must be 0–255, got %d", position)
	}

	light, err := s.bridge.GetLight(s.cfg.LightID)
	if err != nil {
		return fmt.Errorf("failed to get light state: %w", err)
	}

	// An off light reads as (0,0,0) in GetPosition, so compose the new color
	// from black rather than from the stale color the bridge still reports.
	var r, g, b uint8
	if light.State != nil && light.State.On {
		r, g, b = xyBriToRGB(light.State.Xy, light.State.Bri)
	}

	channelValue := uint8(position)
	switch s.cfg.Channel {
	case "red":
		r = channelValue
	case "green":
		g = channelValue
	case "blue":
		b = channelValue
	}

	maxChan := maxUint8(r, g, b)
	if maxChan == 0 {
		if err := light.SetState(huego.State{On: false}); err != nil {
			return fmt.Errorf("failed to turn off light: %w", err)
		}
		return nil
	}

	x, y := rgbToXY(r, g, b)
	if err := light.SetState(huego.State{
		On:  true,
		Xy:  []float32{x, y},
		Bri: maxToBri(maxChan),
	}); err != nil {
		return fmt.Errorf("failed to set color: %w", err)
	}

	return nil
}

// GetPosition returns the current value of the configured RGB channel (0–255).
func (s *hueLightColor) GetPosition(ctx context.Context, extra map[string]interface{}) (uint32, error) {
	light, err := s.bridge.GetLight(s.cfg.LightID)
	if err != nil {
		return 0, fmt.Errorf("failed to get light state: %w", err)
	}

	if light.State == nil || !light.State.On {
		return 0, nil
	}

	r, g, b := xyBriToRGB(light.State.Xy, light.State.Bri)

	var channelValue uint8
	switch s.cfg.Channel {
	case "red":
		channelValue = r
	case "green":
		channelValue = g
	case "blue":
		channelValue = b
	}

	return uint32(channelValue), nil
}

func (s *hueLightColor) GetNumberOfPositions(ctx context.Context, extra map[string]interface{}) (uint32, []string, error) {
	return 256, nil, nil
}
