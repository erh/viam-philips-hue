package main

import (
	"go.viam.com/rdk/components/generic"
	toggleswitch "go.viam.com/rdk/components/switch"
	"go.viam.com/rdk/module"
	"go.viam.com/rdk/resource"
	"go.viam.com/rdk/services/discovery"

	hue "github.com/erh/hue"
)

func main() {
	module.ModularMain(
		resource.APIModel{API: generic.API, Model: hue.HueBridge},
		resource.APIModel{API: toggleswitch.API, Model: hue.HueLight},
		resource.APIModel{API: toggleswitch.API, Model: hue.HueRoom},
		resource.APIModel{API: toggleswitch.API, Model: hue.HueLightBrightness},
		resource.APIModel{API: toggleswitch.API, Model: hue.HueLightColor},
		resource.APIModel{API: toggleswitch.API, Model: hue.HueLightMode},
		resource.APIModel{API: discovery.API, Model: hue.HueDiscovery},
	)
}
