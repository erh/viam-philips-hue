package hue

import (
	"context"
	"fmt"

	"github.com/amimof/huego"
	"go.viam.com/rdk/components/switch"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/resource"
	"go.viam.com/rdk/services/discovery"
	"go.viam.com/rdk/utils"
)

var HueDiscovery = family.WithModel("hue-discovery")

func init() {
	resource.RegisterService(discovery.API, HueDiscovery,
		resource.Registration[discovery.Service, *DiscoveryConfig]{
			Constructor: newHueDiscover,
		},
	)
}

type DiscoveryConfig struct {
	BridgeHost string `json:"bridge_host,omitempty"`
	Username   string `json:"username"`
}

func (cfg *DiscoveryConfig) Validate(path string) ([]string, []string, error) {
	if cfg.Username == "" {
		return nil, nil, fmt.Errorf("need a username (API key) for the Hue bridge")
	}
	return nil, nil, nil
}

func NewDiscovery(logger logging.Logger) *HueDiscover {
	return &HueDiscover{logger: logger}
}

// DiscoverBridge finds a Hue bridge on the network and returns its host address
func DiscoverBridge() (string, error) {
	bridge, err := huego.Discover()
	if err != nil {
		return "", err
	}
	return bridge.Host, nil
}

// CreateUser creates a new user on the Hue bridge. The link button must be pressed first.
func CreateUser(bridgeHost, deviceType string) (string, error) {
	bridge := huego.New(bridgeHost, "")
	user, err := bridge.CreateUser(deviceType)
	if err != nil {
		return "", err
	}
	return user, nil
}

func (s *HueDiscover) SetBridge(host, username string) {
	s.cfg = &DiscoveryConfig{
		BridgeHost: host,
		Username:   username,
	}
	s.bridge = huego.New(host, username)
}

type HueDiscover struct {
	resource.AlwaysRebuild
	resource.TriviallyCloseable

	name resource.Name

	logger logging.Logger
	cfg    *DiscoveryConfig
	bridge *huego.Bridge
}

func newHueDiscover(ctx context.Context, _ resource.Dependencies, rawConf resource.Config, logger logging.Logger) (discovery.Service, error) {
	conf, err := resource.NativeConfig[*DiscoveryConfig](rawConf)
	if err != nil {
		return nil, err
	}

	s := &HueDiscover{
		name:   rawConf.ResourceName(),
		logger: logger,
		cfg:    conf,
	}

	bridgeHost := conf.BridgeHost

	// If no bridge host specified, discover it automatically
	if bridgeHost == "" {
		s.logger.Info("No bridge_host specified, discovering Hue bridge...")
		bridge, err := huego.Discover()
		if err != nil {
			return nil, fmt.Errorf("failed to discover Hue bridge: %w", err)
		}
		bridgeHost = bridge.Host
		s.logger.Infof("Discovered Hue bridge at %s", bridgeHost)
		s.cfg.BridgeHost = bridgeHost
	}

	s.bridge = huego.New(bridgeHost, conf.Username)

	// Test connection by getting bridge config
	_, err = s.bridge.GetConfig()
	if err != nil {
		return nil, fmt.Errorf("cannot connect to Hue bridge at %s: %w", bridgeHost, err)
	}

	return s, nil
}

func (s *HueDiscover) Name() resource.Name {
	return s.name
}

func (s *HueDiscover) DoCommand(ctx context.Context, cmd map[string]interface{}) (map[string]interface{}, error) {
	return nil, nil
}

func (s *HueDiscover) DiscoverResources(ctx context.Context, extra map[string]any) ([]resource.Config, error) {
	return s.DiscoverHue(ctx)
}

func (s *HueDiscover) DiscoverHue(ctx context.Context) ([]resource.Config, error) {
	lights, err := s.bridge.GetLights()
	if err != nil {
		return nil, fmt.Errorf("cannot get lights from Hue bridge: %w", err)
	}

	configs := []resource.Config{}
	for _, light := range lights {
		s.logger.Debugf("discovery result light: %d %s type: %s", light.ID, light.Name, light.Type)

		c := resource.Config{
			Name: light.Name,
			API:  toggleswitch.API,
			Attributes: utils.AttributeMap{
				"bridge_host": s.cfg.BridgeHost,
				"username":    s.cfg.Username,
				"light_id":    light.ID,
			},
		}

		// All Hue lights use the same model - they all support on/off and brightness
		c.Model = HueLight

		configs = append(configs, c)
	}
	return configs, nil
}
