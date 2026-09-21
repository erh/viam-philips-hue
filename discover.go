package hue

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/amimof/huego"
	"go.viam.com/rdk/components/generic"
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
	Bridge     string `json:"bridge,omitempty"`      // name of a hue-bridge component
	BridgeHost string `json:"bridge_host,omitempty"` // or inline credentials
	Username   string `json:"username,omitempty"`
}

func (cfg *DiscoveryConfig) Validate(path string) ([]string, []string, error) {
	deps, err := validateBridgeRef(cfg.Bridge, cfg.Username)
	if err != nil {
		return nil, nil, err
	}
	return deps, nil, nil
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

// SetBridge points the discovery at a bridge using inline credentials (CLI use).
func (s *HueDiscover) SetBridge(host, username string) {
	s.bridgeName = ""
	s.host = host
	s.username = username
	s.bridge = huego.New(host, username)
}

type HueDiscover struct {
	resource.AlwaysRebuild
	resource.TriviallyCloseable

	name   resource.Name
	logger logging.Logger

	bridgeName string // name of the hue-bridge component, if configured that way
	host       string // inline credentials, if configured that way
	username   string
	bridge     *huego.Bridge
}

func newHueDiscover(ctx context.Context, deps resource.Dependencies, rawConf resource.Config, logger logging.Logger) (discovery.Service, error) {
	conf, err := resource.NativeConfig[*DiscoveryConfig](rawConf)
	if err != nil {
		return nil, err
	}

	bridge, err := resolveBridge(ctx, deps, conf.Bridge, conf.BridgeHost, conf.Username, logger)
	if err != nil {
		return nil, err
	}

	s := &HueDiscover{
		name:       rawConf.ResourceName(),
		logger:     logger,
		bridgeName: conf.Bridge,
		host:       bridge.Host,
		username:   conf.Username,
		bridge:     bridge,
	}

	// Test connection by getting bridge config
	if _, err := s.bridge.GetConfigContext(ctx); err != nil {
		return nil, fmt.Errorf("cannot connect to Hue bridge at %s: %w\n%s", bridge.Host, err, SetupHelp)
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
	return map[string]interface{}{"bridge_host": s.host}, nil
}

// DiscoverResources emits configs for everything on the bridge. Optional extras:
//
//	"rooms":  false  -> skip the per-room switches
//	"lights": false  -> skip the per-light switches
//	"mode":   false  -> skip the lights-mode switch
func (s *HueDiscover) DiscoverResources(ctx context.Context, extra map[string]any) ([]resource.Config, error) {
	return s.DiscoverHue(ctx, extra)
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

// nameAllocator hands out unique resource names. On a collision (a bulb named
// the same as its room, or two bulbs with the same name) the kind is appended
// first ("Bedroom-light"), then the Hue ID ("Bedroom-light-3"). A name that
// sanitizes to nothing falls back to "<kind>-<id>".
type nameAllocator struct {
	used map[string]bool
}

func newNameAllocator() *nameAllocator {
	return &nameAllocator{used: map[string]bool{}}
}

func (a *nameAllocator) allocate(base, kind string, id int) string {
	if base == "" {
		base = fmt.Sprintf("%s-%d", kind, id)
	}
	// A name that already ends in the kind ("Lamp-light") gets the ID only.
	stem := base
	if strings.HasSuffix(strings.ToLower(base), "-"+kind) {
		stem = base[:len(base)-len(kind)-1]
	}
	candidates := []string{
		base,
		fmt.Sprintf("%s-%s", stem, kind),
		fmt.Sprintf("%s-%s-%d", stem, kind, id),
	}
	name := ""
	for _, c := range candidates {
		if !a.used[c] {
			name = c
			break
		}
	}
	for i := 2; name == ""; i++ {
		c := fmt.Sprintf("%s-%s-%d-%d", base, kind, id, i)
		if !a.used[c] {
			name = c
		}
	}
	a.used[name] = true
	return name
}

func extraBool(extra map[string]any, key string, def bool) bool {
	if extra == nil {
		return def
	}
	if v, ok := extra[key].(bool); ok {
		return v
	}
	return def
}

func (s *HueDiscover) DiscoverHue(ctx context.Context, extra map[string]any) ([]resource.Config, error) {
	lights, err := s.bridge.GetLightsContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("cannot get lights from Hue bridge: %w", err)
	}
	groups, err := s.bridge.GetGroupsContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("cannot get groups from Hue bridge: %w", err)
	}
	sort.Slice(lights, func(i, j int) bool { return lights[i].ID < lights[j].ID })
	sort.Slice(groups, func(i, j int) bool { return groups[i].ID < groups[j].ID })

	configs := []resource.Config{}
	names := newNameAllocator()

	// The bridge: reuse the configured one, or emit one for inline credentials.
	bridgeName := s.bridgeName
	if bridgeName == "" {
		bridgeName = names.allocate("hue-bridge", "bridge", 0)
		configs = append(configs, resource.Config{
			Name:  bridgeName,
			API:   generic.API,
			Model: HueBridge,
			Attributes: utils.AttributeMap{
				"bridge_host": s.host,
				"username":    s.username,
			},
		})
	}

	colorIDs := map[int]bool{}
	ctIDs := map[int]bool{}
	for _, light := range lights {
		if lightSupportsColor(light) {
			colorIDs[light.ID] = true
		}
		if colorIDs[light.ID] || strings.Contains(strings.ToLower(light.Type), "temperature") {
			ctIDs[light.ID] = true
		}
	}

	// Rooms and zones, one switch each.
	danceGroups := map[string][]int{}
	if extraBool(extra, "rooms", true) {
		for _, g := range groups {
			if g.Type != "Room" && g.Type != "Zone" {
				continue
			}
			name := names.allocate(sanitizeName(g.Name), "room", g.ID)
			s.logger.Debugf("discovery result group: %d %s type: %s lights: %v", g.ID, g.Name, g.Type, g.Lights)
			configs = append(configs, resource.Config{
				Name:  name,
				API:   toggleswitch.API,
				Model: HueRoom,
				Attributes: utils.AttributeMap{
					"bridge":   bridgeName,
					"group_id": g.ID,
				},
			})
			if g.Type == "Room" {
				for _, id := range groupLightIDs(g) {
					if colorIDs[id] {
						danceGroups[name] = append(danceGroups[name], id)
					}
				}
			}
		}
	}

	// Individual lights.
	if extraBool(extra, "lights", true) {
		for _, light := range lights {
			s.logger.Debugf("discovery result light: %d %s type: %s color: %v", light.ID, light.Name, light.Type, colorIDs[light.ID])
			configs = append(configs, resource.Config{
				Name:  names.allocate(sanitizeName(light.Name), "light", light.ID),
				API:   toggleswitch.API,
				Model: HueLight,
				Attributes: utils.AttributeMap{
					"bridge":   bridgeName,
					"light_id": light.ID,
				},
			})
		}
	}

	// A single mode switch: dance groups follow rooms so each room cycles at a
	// different point on the hue wheel; daylight/warm cover every light that
	// supports color temperature.
	if extraBool(extra, "mode", true) && (len(colorIDs) > 0 || len(ctIDs) > 0) {
		if len(danceGroups) == 0 && len(colorIDs) > 0 {
			danceGroups["all"] = sortedIDs(colorIDs)
		}
		attrs := utils.AttributeMap{"bridge": bridgeName}
		if len(danceGroups) > 0 {
			attrs["dance"] = danceGroups
		}
		if len(ctIDs) > 0 {
			attrs["daylight"] = sortedIDs(ctIDs)
			attrs["warm"] = sortedIDs(ctIDs)
		}
		configs = append(configs, resource.Config{
			Name:       names.allocate("hue-mode", "mode", 0),
			API:        toggleswitch.API,
			Model:      HueLightMode,
			Attributes: attrs,
		})
	}

	return configs, nil
}

func sortedIDs(set map[int]bool) []int {
	ids := make([]int, 0, len(set))
	for id := range set {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	return ids
}
