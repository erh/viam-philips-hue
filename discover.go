package hue

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/amimof/huego"
	toggleswitch "go.viam.com/rdk/components/switch"
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
		return nil, nil, errMissingUsername()
	}
	return nil, nil, nil
}

func NewDiscovery(logger logging.Logger) *HueDiscover {
	return &HueDiscover{logger: logger}
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

	bridgeHost, err := resolveBridgeHost(conf.BridgeHost, logger)
	if err != nil {
		return nil, err
	}

	s := &HueDiscover{
		name:   rawConf.ResourceName(),
		logger: logger,
		// Copy the config so the resolved host is stored without mutating the
		// converted attributes owned by the resource.Config.
		cfg: &DiscoveryConfig{
			BridgeHost: bridgeHost,
			Username:   conf.Username,
		},
		bridge: huego.New(bridgeHost, conf.Username),
	}

	// Test connection by getting bridge config
	_, err = s.bridge.GetConfig()
	if err != nil {
		return nil, fmt.Errorf("cannot connect to Hue bridge at %s: %w\n%s", bridgeHost, err, SetupHelp)
	}

	return s, nil
}

func (s *HueDiscover) Name() resource.Name {
	return s.name
}

func (s *HueDiscover) DoCommand(ctx context.Context, cmd map[string]interface{}) (map[string]interface{}, error) {
	return map[string]interface{}{}, nil
}

func (s *HueDiscover) Status(ctx context.Context) (map[string]interface{}, error) {
	return map[string]interface{}{"bridge_host": s.cfg.BridgeHost}, nil
}

func (s *HueDiscover) DiscoverResources(ctx context.Context, extra map[string]any) ([]resource.Config, error) {
	return s.DiscoverHue(ctx)
}

// sanitizeName replaces any character that is not alphanumeric, '-', or '_'
// with '-', then collapses runs of '-' and trims leading/trailing '-'.
var reUnsafe = regexp.MustCompile(`[^a-zA-Z0-9_-]+`)
var reCollapse = regexp.MustCompile(`-{2,}`)

func sanitizeName(name string) string {
	s := reUnsafe.ReplaceAllString(name, "-")
	s = reCollapse.ReplaceAllString(s, "-")
	return strings.Trim(s, "-")
}

// nameAllocator hands out unique resource names. Two lights whose names
// sanitize to the same string get the light ID appended; a name that
// sanitizes to nothing falls back to "hue-light-<id>".
type nameAllocator struct {
	used map[string]bool
}

func newNameAllocator() *nameAllocator {
	return &nameAllocator{used: map[string]bool{}}
}

func (a *nameAllocator) allocate(base string, lightID int) string {
	if base == "" {
		base = fmt.Sprintf("hue-light-%d", lightID)
	}
	name := base
	if a.used[name] {
		name = fmt.Sprintf("%s-%d", base, lightID)
	}
	for i := 2; a.used[name]; i++ {
		name = fmt.Sprintf("%s-%d-%d", base, lightID, i)
	}
	a.used[name] = true
	return name
}

func (s *HueDiscover) DiscoverHue(ctx context.Context) ([]resource.Config, error) {
	lights, err := s.bridge.GetLights()
	if err != nil {
		return nil, fmt.Errorf("cannot get lights from Hue bridge: %w", err)
	}

	configs := []resource.Config{}
	var colorLightIDs []int
	names := newNameAllocator()

	for _, light := range lights {
		colorMode := ""
		if light.State != nil {
			colorMode = light.State.ColorMode
		}
		supportsColor := lightSupportsColor(light)

		s.logger.Debugf("discovery result light: %d %s type: %s colormode: %s color: %v", light.ID, light.Name, light.Type, colorMode, supportsColor)

		safeName := names.allocate(sanitizeName(light.Name), light.ID)

		baseAttrs := utils.AttributeMap{
			"bridge_host": s.cfg.BridgeHost,
			"username":    s.cfg.Username,
			"light_id":    light.ID,
		}

		// All lights support brightness control.
		configs = append(configs, resource.Config{
			Name:       safeName,
			API:        toggleswitch.API,
			Model:      HueLightBrightness,
			Attributes: baseAttrs,
		})

		// Color lights get one switch per RGB channel.
		if supportsColor {
			colorLightIDs = append(colorLightIDs, light.ID)
			for _, channel := range []string{"red", "green", "blue"} {
				channelAttrs := utils.AttributeMap{
					"bridge_host": s.cfg.BridgeHost,
					"username":    s.cfg.Username,
					"light_id":    light.ID,
					"channel":     channel,
				}
				configs = append(configs, resource.Config{
					Name:       names.allocate(fmt.Sprintf("%s-%s", safeName, channel), light.ID),
					API:        toggleswitch.API,
					Model:      HueLightColor,
					Attributes: channelAttrs,
				})
			}
		}
	}

	// Emit a single mode switch covering all color-capable lights.
	if len(colorLightIDs) > 0 {
		configs = append(configs, resource.Config{
			Name:  names.allocate("hue-mode", 0),
			API:   toggleswitch.API,
			Model: HueLightMode,
			Attributes: utils.AttributeMap{
				"bridge_host": s.cfg.BridgeHost,
				"username":    s.cfg.Username,
				"dance":       map[string][]int{"all": colorLightIDs},
				"daylight":    colorLightIDs,
				"warm":        colorLightIDs,
			},
		})
	}

	return configs, nil
}
