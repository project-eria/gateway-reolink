package main

import (
	"fmt"
	"time"

	"github.com/project-eria/go-wot/dataSchema"
	"github.com/project-eria/go-wot/interaction"
	"github.com/project-eria/go-wot/producer"
	"github.com/project-eria/go-wot/thing"

	eria "github.com/project-eria/eria-core"
	zlog "github.com/rs/zerolog/log"
)

var config = struct {
	// Seconds between refreshes of the `latest` image (sub stream) while nobody
	// observes it; 0 refreshes it only on detections. Observed, it is every second.
	SnapshotRefresh int            `yaml:"snapshotRefresh" default:"10"`
	Cameras         []configCamera `yaml:"cameras"`
}{}

type configCamera struct {
	Ref      string `yaml:"ref" required:"true"` // URL path of the Thing
	Name     string `yaml:"name" required:"true"`
	URL      string `yaml:"url" required:"true"` // http(s)://<camera address>
	User     string `yaml:"user" required:"true"`
	Password string `yaml:"password" required:"true"`
	// ONVIF must be enabled on the camera (Settings > Network > Advanced > Server Settings)
	OnvifPort int `yaml:"onvifPort" default:"8000"`
}

// camThing links a camera to its exposed Thing
type camThing struct {
	cam   *Camera
	thing producer.ExposedThing
	// Read by the event handler during EmitEvent, both on the watcher goroutine
	lastAlarm string
	latest    *feed // sub stream image
}

var _cams = map[string]*camThing{}

func main() {
	defer func() {
		zlog.Info().Msg("[main] Stopped")
	}()

	eria.Init("ERIA Reolink Gateway", &config)

	producer := eria.Producer("")
	for _, conf := range config.Cameras {
		ct := &camThing{cam: NewCamera(conf.URL, conf.User, conf.Password, conf.OnvifPort)}
		ct.thing, _ = producer.AddThing(conf.Ref, cameraTD(conf))
		setupHandlers(ct)
		ct.latest = newFeed(ct.cam, ct.thing, "latest", "sub", time.Duration(config.SnapshotRefresh)*time.Second)
		newFeed(ct.cam, ct.thing, "latestMain", "main", 0)
		_cams[conf.Ref] = ct
		go ct.cam.WatchEvents(func(rule string) {
			zlog.Debug().Str("camera", conf.Ref).Str("rule", rule).Msg("[main] Alarm")
			ct.lastAlarm = rule
			ct.thing.EmitEvent("alarm", nil)
			ct.latest.refreshNow()
		})
	}
	defer func() {
		for _, ct := range _cams {
			ct.cam.Logout()
		}
	}()

	eria.Start("")
}

func cameraTD(conf configCamera) *thing.Thing {
	td, _ := eria.NewThingDescription(
		"eria:gateway:reolink:"+conf.Ref,
		eria.AppVersion,
		conf.Name,
		"Reolink camera "+conf.Name,
		[]string{"Camera"}, // latest, latestMain, snapshot, alarm
	)

	streamsSchema, _ := dataSchema.NewObject()
	td.AddProperty(interaction.NewProperty(
		"streams",
		"Streams",
		"Main and sub streams: {codec, width, height, fps, rtsp, rtmp, flv}, URLs without credentials",
		streamsSchema,
		interaction.PropertyReadOnly(true),
	))

	return td
}

func setupHandlers(ct *camThing) {
	ct.thing.SetPropertyReadHandler("streams", func(t producer.ExposedThing, name string, params map[string]interface{}) (interface{}, error) {
		return ct.cam.Streams()
	})

	ct.thing.SetActionHandler("snapshot", func(input interface{}, params map[string]interface{}) (interface{}, error) {
		stream, err := snapshotStream(input)
		if err != nil {
			return nil, err
		}
		return ct.cam.Snapshot(stream)
	})

	ct.thing.SetEventHandler("alarm", func() (interface{}, error) {
		return ct.lastAlarm, nil
	})
}

// snapshotStream reads {stream: "main"|"sub"}, sub by default (lighter)
func snapshotStream(input interface{}) (string, error) {
	m, _ := input.(map[string]interface{})
	stream, _ := m["stream"].(string)
	switch stream {
	case "":
		return "sub", nil
	case "main", "sub":
		return stream, nil
	}
	return "", &producer.DataError{Message: fmt.Sprintf("unknown stream %q, expected main or sub", stream)}
}
