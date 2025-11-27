package main

import (
	"context"
	"flag"
	"fmt"

	"go.viam.com/rdk/components/switch"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/resource"

	hue "github.com/erh/hue"
)

func main() {
	err := realMain()
	if err != nil {
		panic(err)
	}
}

func realMain() error {
	ctx := context.Background()
	logger := logging.NewLogger("viamhuecli")

	bridgeHost := flag.String("bridge", "", "Hue bridge host/IP (optional, will auto-discover)")
	username := flag.String("username", "", "Hue API username")
	debug := flag.Bool("debug", false, "debug")
	device := flag.String("device", "", "What device to control")
	setting := flag.Int("set", -1, "What to set the device to")
	register := flag.Bool("register", false, "Register with the Hue bridge to get a username (press link button first!)")

	flag.Parse()

	if *debug {
		logger.SetLevel(logging.DEBUG)
	}

	// If bridge not specified, discover it
	if *bridgeHost == "" {
		logger.Info("No bridge specified, discovering...")
		bridge, err := hue.DiscoverBridge()
		if err != nil {
			return fmt.Errorf("failed to discover bridge: %w", err)
		}
		*bridgeHost = bridge
		logger.Infof("Found bridge at %s", *bridgeHost)
	}

	// Handle registration mode
	if *register {
		fmt.Println("Press the link button on your Hue bridge, then press Enter...")
		fmt.Scanln()

		user, err := hue.CreateUser(*bridgeHost, "viam-hue-module")
		if err != nil {
			return fmt.Errorf("failed to create user: %w", err)
		}
		fmt.Printf("\nSuccess! Your username is:\n\n  %s\n\n", user)
		fmt.Println("Save this and use it in your Viam config.")
		return nil
	}

	if *username == "" {
		return fmt.Errorf("need -username flag (or use -register to create one)")
	}

	// Create a simple discovery helper directly
	d := hue.NewDiscovery(logger)
	d.SetBridge(*bridgeHost, *username)
	all, err := d.DiscoverHue(ctx)
	if err != nil {
		return err
	}

	var info resource.Config

	for _, c := range all {
		fmt.Printf("%v\n", c)
		if c.Name == *device {
			info = c
		}
	}

	if *device != "" {
		if info.Name == "" {
			return fmt.Errorf("cannot find device [%s]", *device)
		}

		logger.Infof("found device %v", info)

		reg, found := resource.LookupRegistration(info.API, info.Model)
		if !found {
			return fmt.Errorf("cannot find registration")
		}

		info.ConvertedAttributes, err = reg.AttributeMapConverter(info.Attributes)
		if err != nil {
			return err
		}

		thing, err := reg.Constructor(ctx, nil, info, logger)
		if err != nil {
			return err
		}

		realThing, ok := thing.(toggleswitch.Switch)
		if !ok {
			return fmt.Errorf("why aren't you a switch")
		}

		pos, err := realThing.GetPosition(ctx, nil)
		if err != nil {
			return err
		}

		numPositions, _, err := realThing.GetNumberOfPositions(ctx, nil)
		if err != nil {
			return err
		}

		logger.Infof("starting at position: %v out of %d", pos, numPositions)

		if *setting >= 0 {
			err = realThing.SetPosition(ctx, uint32(*setting), nil)
			if err != nil {
				return err
			}
		}

	}

	return nil
}
