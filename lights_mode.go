package hue

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"github.com/amimof/huego"
	toggleswitch "go.viam.com/rdk/components/switch"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/resource"
)

var HueLightMode = family.WithModel("hue-lights-mode")

func init() {
	resource.RegisterComponent(toggleswitch.API, HueLightMode,
		resource.Registration[toggleswitch.Switch, *LightModeConfig]{
			Constructor: newHueLightMode,
		},
	)
}

type LightModeConfig struct {
	BridgeHost string           `json:"bridge_host,omitempty"`
	Username   string           `json:"username"`
	Dance      map[string][]int `json:"dance,omitempty"` // group name -> light IDs, lights in a group stay in sync
	Daylight   []int            `json:"daylight,omitempty"`
	Warm       []int            `json:"warm,omitempty"`
}

func (cfg *LightModeConfig) Validate(path string) ([]string, []string, error) {
	if cfg.Username == "" {
		return nil, nil, errMissingUsername()
	}
	return nil, nil, nil
}

// positions maps switch position to mode name
var modeNames = []string{"none", "dance", "daylight", "warm"}

type hueLightMode struct {
	resource.AlwaysRebuild
	resource.TriviallyCloseable

	name   resource.Name
	logger logging.Logger
	cfg    *LightModeConfig

	bridge *huego.Bridge

	mu          sync.Mutex
	position    uint32
	savedStates map[int]*huego.State // light ID -> saved state before any mode was activated
}

func newHueLightMode(ctx context.Context, deps resource.Dependencies, rawConf resource.Config, logger logging.Logger) (toggleswitch.Switch, error) {
	conf, err := resource.NativeConfig[*LightModeConfig](rawConf)
	if err != nil {
		return nil, err
	}

	bridgeHost, err := resolveBridgeHost(conf.BridgeHost, logger)
	if err != nil {
		return nil, err
	}

	s := &hueLightMode{
		name:        rawConf.ResourceName(),
		logger:      logger,
		cfg:         conf,
		bridge:      huego.New(bridgeHost, conf.Username),
		savedStates: make(map[int]*huego.State),
	}

	return s, nil
}

func (s *hueLightMode) Name() resource.Name {
	return s.name
}

func (s *hueLightMode) DoCommand(ctx context.Context, cmd map[string]interface{}) (map[string]interface{}, error) {
	return map[string]interface{}{}, nil
}

func (s *hueLightMode) Status(ctx context.Context) (map[string]interface{}, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return map[string]interface{}{"mode": modeNames[s.position]}, nil
}

// SetPosition switches between modes.
// Position 0 = "none" (restore saved state), Position 1 = "dance" (colorloop).
func (s *hueLightMode) SetPosition(ctx context.Context, position uint32, extra map[string]interface{}) error {
	if int(position) >= len(modeNames) {
		return fmt.Errorf("invalid position %d, must be 0-%d", position, len(modeNames)-1)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if position == 0 {
		return s.restoreState()
	}

	lightIDs := s.lightIDsForPosition(position)
	if err := s.saveState(lightIDs); err != nil {
		return err
	}

	switch modeNames[position] {
	case "dance":
		return s.activateDance(s.cfg.Dance, position)
	case "daylight":
		// Cool daylight white (~6500 K, 153 mireds) at full brightness.
		return s.activateWhite(lightIDs, 254, 153, position)
	case "warm":
		// Warm incandescent white (~2700 K, 370 mireds) at moderate brightness.
		return s.activateWhite(lightIDs, 200, 370, position)
	}

	return fmt.Errorf("unknown mode %q", modeNames[position])
}

func (s *hueLightMode) GetPosition(ctx context.Context, extra map[string]interface{}) (uint32, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.position, nil
}

func (s *hueLightMode) GetNumberOfPositions(ctx context.Context, extra map[string]interface{}) (uint32, []string, error) {
	return uint32(len(modeNames)), modeNames, nil
}

// lightIDsForPosition returns a flat list of all light IDs for a given mode position.
// For dance mode the groups are flattened so saveState can snapshot every light.
func (s *hueLightMode) lightIDsForPosition(position uint32) []int {
	switch modeNames[position] {
	case "dance":
		return flattenDanceGroups(s.cfg.Dance)
	case "daylight":
		return s.cfg.Daylight
	case "warm":
		return s.cfg.Warm
	}
	return nil
}

// flattenDanceGroups collapses all groups in the dance map into a single slice of IDs.
func flattenDanceGroups(groups map[string][]int) []int {
	var ids []int
	keys := sortedKeys(groups)
	for _, k := range keys {
		ids = append(ids, groups[k]...)
	}
	return ids
}

// sortedKeys returns the keys of a map[string][]int in sorted order for deterministic iteration.
func sortedKeys(m map[string][]int) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// saveState snapshots the current state of each light that has not already
// been snapshotted. Lights already in savedStates are skipped so that switching
// directly from one mode to another (dance -> daylight) keeps the original
// pre-mode state rather than overwriting it with the first mode's state.
// savedStates is only cleared by restoreState (position 0).
func (s *hueLightMode) saveState(lightIDs []int) error {
	for _, id := range lightIDs {
		if _, ok := s.savedStates[id]; ok {
			continue
		}
		light, err := s.bridge.GetLight(id)
		if err != nil {
			return fmt.Errorf("failed to get state for light %d: %w", id, err)
		}
		if light.State == nil {
			return fmt.Errorf("light %d returned no state", id)
		}
		saved := *light.State
		s.savedStates[id] = &saved
	}
	return nil
}

// stopEffect clears any running effect (colorloop) on a light. The bridge
// processes JSON fields in order and ignores color fields sent in the same
// request as an effect change, and it rejects effect changes on lights that
// are off, so this is always sent alone with On:true before any color fields.
func stopEffect(light *huego.Light) error {
	return light.SetState(huego.State{On: true, Effect: "none"})
}

// restoreState restores each saved light back to its pre-mode state.
// Three steps per light: stop any active effect, apply the saved brightness and
// color fields (with the light on, since the bridge rejects color fields on an
// off light), then turn the light off again if it was off.
func (s *hueLightMode) restoreState() error {
	var firstErr error
	noteErr := func(err error) {
		if firstErr == nil {
			firstErr = err
		}
	}

	for id, state := range s.savedStates {
		light, err := s.bridge.GetLight(id)
		if err != nil {
			s.logger.Warnf("failed to get light %d for restore: %v", id, err)
			noteErr(err)
			continue
		}

		// Step 1: stop the colorloop effect before changing color fields.
		if err := stopEffect(light); err != nil {
			s.logger.Warnf("failed to stop effect on light %d for restore: %v", id, err)
			noteErr(err)
			continue
		}

		// Step 2: restore brightness and the color fields matching the original
		// color mode, sidestepping the omitempty zero-value issue (e.g. Sat=0 for
		// white would be omitted, leaving dance mode's Sat=254 active).
		//
		// Bri, Hue, and Sat are uint8/uint16 with omitempty, so a stored zero
		// would be silently dropped from the JSON payload. Bumping each to a
		// minimum of 1 is visually indistinguishable and ensures the field is sent.
		bri := state.Bri
		if bri == 0 {
			bri = 1
		}
		restore := huego.State{
			On:  true,
			Bri: bri,
		}
		switch state.ColorMode {
		case "ct":
			restore.Ct = state.Ct
		case "xy":
			restore.Xy = state.Xy
		case "hs":
			restore.Hue = state.Hue
			if restore.Hue == 0 {
				restore.Hue = 1
			}
			restore.Sat = state.Sat
			if restore.Sat == 0 {
				restore.Sat = 1
			}
		}
		if err := light.SetState(restore); err != nil {
			s.logger.Warnf("failed to restore state for light %d: %v", id, err)
			noteErr(err)
			continue
		}

		// Step 3: the light was off before the mode was activated; turn it back
		// off. This must be a separate request with no other fields, because the
		// bridge rejects bri/color fields sent alongside on:false.
		if !state.On {
			if err := light.SetState(huego.State{On: false}); err != nil {
				s.logger.Warnf("failed to turn off light %d for restore: %v", id, err)
				noteErr(err)
			}
		}
	}
	s.savedStates = make(map[int]*huego.State)
	s.position = 0
	return firstErr
}

// activateDance enables the colorloop effect on each light, staggered by group.
// All lights within a group share the same starting hue so they stay in sync with
// each other. Groups are evenly offset across the full hue wheel (0–65535) so
// different groups cycle through different colors at the same time.
// Groups are iterated in sorted key order for deterministic staggering.
//
// A two-step approach is used per light: first commit the starting hue, then
// enable the colorloop. Combining both in one call is unreliable because the
// bridge may start the colorloop before honoring the hue seed, causing all
// groups to begin at the same position. The hue field has omitempty, so a
// startHue of 0 is bumped to 1 to prevent the field from being omitted.
func (s *hueLightMode) activateDance(groups map[string][]int, position uint32) error {
	keys := sortedKeys(groups)
	n := len(keys)
	for i, k := range keys {
		startHue := uint16(1) // minimum 1: hue 0 is omitted by omitempty, 1 is indistinguishable visually
		if n > 0 {
			h := uint16(i * 65535 / n)
			if h > 0 {
				startHue = h
			}
		}
		for _, id := range groups[k] {
			light, err := s.bridge.GetLight(id)
			if err != nil {
				return fmt.Errorf("failed to get light %d: %w", id, err)
			}
			// Step 1: seed the starting hue and saturation (no effect yet).
			if err := light.SetState(huego.State{
				On:  true,
				Hue: startHue,
				Sat: 254,
			}); err != nil {
				return fmt.Errorf("failed to seed hue on light %d: %w", id, err)
			}
			// Step 2: start the colorloop from the seeded hue.
			// On:true must be explicit — the bool field has no omitempty, so the
			// zero value would serialize as "on":false and turn the light off.
			if err := light.SetState(huego.State{
				On:     true,
				Effect: "colorloop",
			}); err != nil {
				return fmt.Errorf("failed to set dance mode on light %d: %w", id, err)
			}
		}
	}
	s.position = position
	return nil
}

// activateWhite sets each light to a white at the given brightness and color
// temperature (mireds). Any running effect is stopped first in a separate
// request; sending ct alongside effect:"none" would have the ct ignored while
// the colorloop is still active (see stopEffect).
func (s *hueLightMode) activateWhite(lightIDs []int, bri uint8, ct uint16, position uint32) error {
	mode := modeNames[position]
	for _, id := range lightIDs {
		light, err := s.bridge.GetLight(id)
		if err != nil {
			return fmt.Errorf("failed to get light %d: %w", id, err)
		}
		if err := stopEffect(light); err != nil {
			return fmt.Errorf("failed to stop effect on light %d for %s mode: %w", id, mode, err)
		}
		if err := light.SetState(huego.State{
			On:             true,
			Bri:            bri,
			Ct:             ct,
			TransitionTime: 4,
		}); err != nil {
			return fmt.Errorf("failed to set %s mode on light %d: %w", mode, id, err)
		}
	}
	s.position = position
	return nil
}
