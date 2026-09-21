package hue

import (
	"context"
	"fmt"
	"strings"

	toggleswitch "go.viam.com/rdk/components/switch"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/resource"
)

// HueRoom controls a Hue group (room or zone) as one switch, with the same
// presets and DoCommand as hue-light. One bridge call changes every bulb in
// the group together.
var HueRoom = family.WithModel("hue-room")

func init() {
	resource.RegisterComponent(toggleswitch.API, HueRoom,
		resource.Registration[toggleswitch.Switch, *RoomConfig]{
			Constructor: newHueRoom,
		},
	)
}

type RoomConfig struct {
	Bridge     string `json:"bridge,omitempty"`      // name of a hue-bridge component
	BridgeHost string `json:"bridge_host,omitempty"` // or inline credentials
	Username   string `json:"username,omitempty"`

	// Identify the group by ID or by name (as shown in the Hue app).
	// Group 0 is the bridge's built-in "all lights" group.
	GroupID   int    `json:"group_id,omitempty"`
	GroupName string `json:"group_name,omitempty"`

	// Optional state applied when the component starts and by position "on".
	Color      string `json:"color,omitempty"`      // hex, e.g. "#FF8800"
	Brightness int    `json:"brightness,omitempty"` // 1-100
}

func (cfg *RoomConfig) Validate(path string) ([]string, []string, error) {
	deps, err := validateBridgeRef(cfg.Bridge, cfg.Username)
	if err != nil {
		return nil, nil, err
	}
	if _, err := onStateFromConfig(cfg.Color, cfg.Brightness); err != nil {
		return nil, nil, err
	}
	return deps, nil, nil
}

type hueRoom struct {
	resource.AlwaysRebuild
	resource.TriviallyCloseable
	presetSwitch

	name resource.Name
	cfg  *RoomConfig
}

func newHueRoom(ctx context.Context, deps resource.Dependencies, rawConf resource.Config, logger logging.Logger) (toggleswitch.Switch, error) {
	conf, err := resource.NativeConfig[*RoomConfig](rawConf)
	if err != nil {
		return nil, err
	}

	bridge, err := resolveBridge(ctx, deps, conf.Bridge, conf.BridgeHost, conf.Username, logger)
	if err != nil {
		return nil, err
	}

	groupID := conf.GroupID
	if conf.GroupName != "" {
		groups, err := bridge.GetGroupsContext(ctx)
		if err != nil {
			return nil, fmt.Errorf("can't list groups on Hue bridge: %w", err)
		}
		found := false
		want := strings.ToLower(strings.TrimSpace(conf.GroupName))
		for _, g := range groups {
			if strings.ToLower(g.Name) == want || sanitizeName(g.Name) == conf.GroupName {
				groupID = g.ID
				found = true
				break
			}
		}
		if !found {
			names := make([]string, 0, len(groups))
			for _, g := range groups {
				names = append(names, fmt.Sprintf("%q (id %d)", g.Name, g.ID))
			}
			return nil, fmt.Errorf("no Hue group named %q; bridge has: %s", conf.GroupName, strings.Join(names, ", "))
		}
	} else if _, err := bridge.GetGroupContext(ctx, groupID); err != nil {
		return nil, fmt.Errorf("can't get group %d from Hue bridge: %w", groupID, err)
	}

	onState, err := onStateFromConfig(conf.Color, conf.Brightness)
	if err != nil {
		return nil, err
	}

	s := &hueRoom{
		name: rawConf.ResourceName(),
		cfg:  conf,
		presetSwitch: presetSwitch{
			target:  groupTarget{bridge: bridge, id: groupID},
			logger:  logger,
			onState: onState,
		},
	}

	if err := s.applyOnState(ctx); err != nil {
		return nil, fmt.Errorf("failed to apply configured color/brightness: %w", err)
	}

	return s, nil
}

func (s *hueRoom) Name() resource.Name {
	return s.name
}
