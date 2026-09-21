package hue

import (
	"context"
	"fmt"
	"sync"

	"github.com/amimof/huego"
	"go.viam.com/rdk/components/generic"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/resource"
)

// HueBridge is a generic component that owns the bridge connection details.
// Every other model can reference it by name with a "bridge" attribute instead
// of repeating bridge_host and username.
var HueBridge = family.WithModel("hue-bridge")

func init() {
	resource.RegisterComponent(generic.API, HueBridge,
		resource.Registration[resource.Resource, *BridgeConfig]{
			Constructor: newHueBridge,
		},
	)
}

type BridgeConfig struct {
	BridgeHost string `json:"bridge_host,omitempty"`
	Username   string `json:"username"`
}

func (cfg *BridgeConfig) Validate(path string) ([]string, []string, error) {
	if cfg.Username == "" {
		return nil, nil, errMissingUsername()
	}
	return nil, nil, nil
}

type hueBridge struct {
	resource.AlwaysRebuild

	name     resource.Name
	logger   logging.Logger
	host     string
	username string
	bridge   *huego.Bridge
}

func newHueBridge(ctx context.Context, _ resource.Dependencies, rawConf resource.Config, logger logging.Logger) (resource.Resource, error) {
	conf, err := resource.NativeConfig[*BridgeConfig](rawConf)
	if err != nil {
		return nil, err
	}

	host, err := resolveBridgeHost(conf.BridgeHost, logger)
	if err != nil {
		return nil, err
	}

	b := &hueBridge{
		name:     rawConf.ResourceName(),
		logger:   logger,
		host:     host,
		username: conf.Username,
		bridge:   huego.New(host, conf.Username),
	}

	if _, err := b.bridge.GetConfig(); err != nil {
		return nil, fmt.Errorf("cannot connect to Hue bridge at %s: %w\n%s", host, err, SetupHelp)
	}

	registerBridge(b.name, b.bridge)
	return b, nil
}

func (b *hueBridge) Name() resource.Name {
	return b.name
}

func (b *hueBridge) Close(ctx context.Context) error {
	unregisterBridge(b.name)
	return nil
}

func (b *hueBridge) Status(ctx context.Context) (map[string]interface{}, error) {
	return map[string]interface{}{"bridge_host": b.host}, nil
}

// DoCommand supports:
//
//	{"command": "bridge_info"} -> bridge_host and username (used by dependents)
//	{"command": "lights"}      -> every light the bridge knows about
//	{"command": "groups"}      -> every room/zone/group the bridge knows about
func (b *hueBridge) DoCommand(ctx context.Context, cmd map[string]interface{}) (map[string]interface{}, error) {
	command, _ := cmd["command"].(string)
	switch command {
	case "bridge_info":
		return map[string]interface{}{"bridge_host": b.host, "username": b.username}, nil
	case "lights":
		lights, err := b.bridge.GetLightsContext(ctx)
		if err != nil {
			return nil, err
		}
		out := make([]interface{}, 0, len(lights))
		for _, l := range lights {
			entry := map[string]interface{}{
				"id":    l.ID,
				"name":  l.Name,
				"type":  l.Type,
				"color": lightSupportsColor(l),
			}
			if l.State != nil {
				entry["on"] = l.State.On
			}
			out = append(out, entry)
		}
		return map[string]interface{}{"lights": out}, nil
	case "groups":
		groups, err := b.bridge.GetGroupsContext(ctx)
		if err != nil {
			return nil, err
		}
		out := make([]interface{}, 0, len(groups))
		for _, g := range groups {
			out = append(out, map[string]interface{}{
				"id":     g.ID,
				"name":   g.Name,
				"type":   g.Type,
				"class":  g.Class,
				"lights": groupLightIDs(g),
			})
		}
		return map[string]interface{}{"groups": out}, nil
	default:
		return nil, fmt.Errorf("unknown command %q; expected bridge_info, lights, or groups", command)
	}
}

// Bridges constructed in this process are registered by name so that dependent
// resources in the same module can share the connection directly. When the
// dependency is handed to us as a client instead, resolveBridge falls back to
// asking it for bridge_info over DoCommand.
var (
	bridgeRegistryMu sync.Mutex
	bridgeRegistry   = map[string]*huego.Bridge{}
)

func registerBridge(name resource.Name, b *huego.Bridge) {
	bridgeRegistryMu.Lock()
	defer bridgeRegistryMu.Unlock()
	bridgeRegistry[name.Name] = b
}

func unregisterBridge(name resource.Name) {
	bridgeRegistryMu.Lock()
	defer bridgeRegistryMu.Unlock()
	delete(bridgeRegistry, name.Name)
}

func lookupRegisteredBridge(name string) *huego.Bridge {
	bridgeRegistryMu.Lock()
	defer bridgeRegistryMu.Unlock()
	return bridgeRegistry[name]
}

func errMissingBridge() error {
	return fmt.Errorf(`need a Hue bridge: set "bridge" to the name of a %s component, or set "username" (and optionally "bridge_host") directly
%s`, HueBridge, SetupHelp)
}

// validateBridgeRef checks the bridge attributes shared by every model and
// returns the dependency list (the named bridge, if any).
func validateBridgeRef(bridgeName, username string) ([]string, error) {
	if bridgeName != "" {
		return []string{bridgeName}, nil
	}
	if username == "" {
		return nil, errMissingBridge()
	}
	return nil, nil
}

// resolveBridge returns a connected bridge from either the named hue-bridge
// dependency or inline credentials.
func resolveBridge(ctx context.Context, deps resource.Dependencies, bridgeName, bridgeHost, username string, logger logging.Logger) (*huego.Bridge, error) {
	if bridgeName == "" {
		if username == "" {
			return nil, errMissingBridge()
		}
		host, err := resolveBridgeHost(bridgeHost, logger)
		if err != nil {
			return nil, err
		}
		return huego.New(host, username), nil
	}

	if b := lookupRegisteredBridge(bridgeName); b != nil {
		return b, nil
	}

	var dep resource.Resource
	for n, r := range deps {
		if n.Name == bridgeName || n.ShortName() == bridgeName || n.String() == bridgeName {
			dep = r
			break
		}
	}
	if dep == nil {
		return nil, fmt.Errorf("bridge %q not found in dependencies; add a %s component with that name", bridgeName, HueBridge)
	}

	info, err := dep.DoCommand(ctx, map[string]interface{}{"command": "bridge_info"})
	if err != nil {
		return nil, fmt.Errorf("bridge %q did not return connection info: %w", bridgeName, err)
	}
	host, _ := info["bridge_host"].(string)
	user, _ := info["username"].(string)
	if host == "" || user == "" {
		return nil, fmt.Errorf("bridge %q returned incomplete connection info", bridgeName)
	}
	return huego.New(host, user), nil
}

// groupLightIDs converts a Hue group's light ID strings to ints.
func groupLightIDs(g huego.Group) []int {
	ids := make([]int, 0, len(g.Lights))
	for _, s := range g.Lights {
		var id int
		if _, err := fmt.Sscanf(s, "%d", &id); err == nil {
			ids = append(ids, id)
		}
	}
	return ids
}
