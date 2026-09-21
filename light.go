package hue

import (
	"context"
	"fmt"

	toggleswitch "go.viam.com/rdk/components/switch"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/resource"
)

// HueLight is a single bulb as one switch: named preset positions plus
// DoCommand for arbitrary color, brightness, and color temperature.
var HueLight = family.WithModel("hue-light")

func init() {
	resource.RegisterComponent(toggleswitch.API, HueLight,
		resource.Registration[toggleswitch.Switch, *LightConfig]{
			Constructor: newHueLight,
		},
	)
}

type LightConfig struct {
	Bridge     string `json:"bridge,omitempty"`      // name of a hue-bridge component
	BridgeHost string `json:"bridge_host,omitempty"` // or inline credentials
	Username   string `json:"username,omitempty"`
	LightID    int    `json:"light_id"`

	// Optional state applied when the component starts and by position "on".
	Color      string `json:"color,omitempty"`      // hex, e.g. "#FF8800"
	Brightness int    `json:"brightness,omitempty"` // 1-100
}

func (cfg *LightConfig) Validate(path string) ([]string, []string, error) {
	deps, err := validateBridgeRef(cfg.Bridge, cfg.Username)
	if err != nil {
		return nil, nil, err
	}
	if cfg.LightID == 0 {
		return nil, nil, fmt.Errorf("need a light_id (Hue light IDs start at 1)")
	}
	if _, err := onStateFromConfig(cfg.Color, cfg.Brightness); err != nil {
		return nil, nil, err
	}
	return deps, nil, nil
}

type hueLight struct {
	resource.AlwaysRebuild
	resource.TriviallyCloseable
	presetSwitch

	name resource.Name
	cfg  *LightConfig
}

func newHueLight(ctx context.Context, deps resource.Dependencies, rawConf resource.Config, logger logging.Logger) (toggleswitch.Switch, error) {
	conf, err := resource.NativeConfig[*LightConfig](rawConf)
	if err != nil {
		return nil, err
	}

	bridge, err := resolveBridge(ctx, deps, conf.Bridge, conf.BridgeHost, conf.Username, logger)
	if err != nil {
		return nil, err
	}

	light, err := bridge.GetLightContext(ctx, conf.LightID)
	if err != nil {
		return nil, fmt.Errorf("can't get light %d from Hue bridge: %w", conf.LightID, err)
	}
	if conf.Color != "" && !lightSupportsColor(*light) {
		logger.Warnf("light %d (%s, type %q) does not appear to support color; the configured color may be rejected", light.ID, light.Name, light.Type)
	}

	onState, err := onStateFromConfig(conf.Color, conf.Brightness)
	if err != nil {
		return nil, err
	}

	s := &hueLight{
		name: rawConf.ResourceName(),
		cfg:  conf,
		presetSwitch: presetSwitch{
			target:  lightTarget{bridge: bridge, id: conf.LightID},
			logger:  logger,
			onState: onState,
		},
	}

	if err := s.applyOnState(ctx); err != nil {
		return nil, fmt.Errorf("failed to apply configured color/brightness: %w", err)
	}

	return s, nil
}

func (s *hueLight) Name() resource.Name {
	return s.name
}
