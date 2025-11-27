package hue

import (
	"context"
	"fmt"

	"github.com/amimof/huego"
	"go.viam.com/rdk/components/switch"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/resource"
)

var HueLight = family.WithModel("hue-light")

func init() {
	resource.RegisterComponent(toggleswitch.API, HueLight,
		resource.Registration[toggleswitch.Switch, *LightConfig]{
			Constructor: newHueLight,
		},
	)
}

type LightConfig struct {
	BridgeHost string `json:"bridge_host,omitempty"`
	Username   string `json:"username"`
	LightID    int    `json:"light_id"`
}

func (cfg *LightConfig) Validate(path string) ([]string, []string, error) {
	if cfg.Username == "" {
		return nil, nil, fmt.Errorf("need a username (API key) for the Hue bridge")
	}
	if cfg.LightID == 0 {
		return nil, nil, fmt.Errorf("need a light_id")
	}
	return nil, nil, nil
}

type hueLight struct {
	resource.AlwaysRebuild
	resource.TriviallyCloseable

	name resource.Name

	logger logging.Logger
	cfg    *LightConfig

	bridge *huego.Bridge
	light  *huego.Light
}

func newHueLight(ctx context.Context, deps resource.Dependencies, rawConf resource.Config, logger logging.Logger) (toggleswitch.Switch, error) {
	conf, err := resource.NativeConfig[*LightConfig](rawConf)
	if err != nil {
		return nil, err
	}

	s := &hueLight{
		name:   rawConf.ResourceName(),
		logger: logger,
		cfg:    conf,
	}

	bridgeHost := conf.BridgeHost

	// If no bridge host specified, discover it automatically
	if bridgeHost == "" {
		logger.Info("No bridge_host specified, discovering Hue bridge...")
		bridge, err := huego.Discover()
		if err != nil {
			return nil, fmt.Errorf("failed to discover Hue bridge: %w", err)
		}
		bridgeHost = bridge.Host
		logger.Infof("Discovered Hue bridge at %s", bridgeHost)
	}

	s.bridge = huego.New(bridgeHost, conf.Username)

	s.light, err = s.bridge.GetLight(conf.LightID)
	if err != nil {
		return nil, fmt.Errorf("can't get light %d from Hue bridge @ (%s): %w", conf.LightID, bridgeHost, err)
	}

	return s, nil
}

func (s *hueLight) Name() resource.Name {
	return s.name
}

func (s *hueLight) DoCommand(ctx context.Context, cmd map[string]interface{}) (map[string]interface{}, error) {
	return nil, nil
}

func (s *hueLight) SetPosition(ctx context.Context, position uint32, extra map[string]interface{}) error {
	// Refresh light state
	light, err := s.bridge.GetLight(s.cfg.LightID)
	if err != nil {
		return fmt.Errorf("failed to get light state: %w", err)
	}
	s.light = light

	if position == 0 {
		// Turn off
		err := s.light.Off()
		if err != nil {
			return fmt.Errorf("failed to turn off light: %w", err)
		}
	} else {
		// Turn on - position 1 is full brightness, higher values could map to brightness levels
		err := s.light.On()
		if err != nil {
			return fmt.Errorf("failed to turn on light: %w", err)
		}

		// If position > 1, use it as a brightness percentage (2-100 maps to brightness)
		if position > 1 && position <= 100 {
			// Hue brightness is 1-254
			bri := uint8((float64(position) / 100.0) * 254)
			if bri < 1 {
				bri = 1
			}
			err := s.light.Bri(bri)
			if err != nil {
				return fmt.Errorf("failed to set brightness: %w", err)
			}
		}
	}

	return nil
}

func (s *hueLight) GetPosition(ctx context.Context, extra map[string]interface{}) (uint32, error) {
	// Refresh light state
	light, err := s.bridge.GetLight(s.cfg.LightID)
	if err != nil {
		return 0, fmt.Errorf("failed to get light state: %w", err)
	}
	s.light = light

	if !s.light.State.On {
		return 0, nil
	}

	// Return brightness as position (1-100 scale)
	// Hue brightness is 1-254
	if s.light.State.Bri > 0 {
		pos := uint32((float64(s.light.State.Bri) / 254.0) * 100)
		if pos < 1 {
			pos = 1
		}
		return pos, nil
	}

	return 1, nil
}

func (s *hueLight) GetNumberOfPositions(ctx context.Context, extra map[string]interface{}) (uint32, []string, error) {
	// 0 = off, 1-100 = brightness levels
	return 101, nil, nil
}
