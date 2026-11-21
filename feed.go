package main

import (
	"sync"
	"time"

	"github.com/project-eria/go-wot/producer"
	zlog "github.com/rs/zerolog/log"
)

const (
	watchedPeriod = time.Second      // refresh while a consumer observes the image
	freshEnough   = 15 * time.Second // a kept image older than that isn't pushed to a new observer
)

// feed keeps the last image of one stream of a camera, as an observable
// property: refreshed every second while observed, every idle otherwise (0:
// not at all), and pushed to the observers at each refresh.
type feed struct {
	cam      *Camera
	thing    producer.ExposedThing
	property string
	stream   string // main | sub
	idle     time.Duration

	mu       sync.Mutex
	image    []byte
	taken    time.Time
	watchers int
	wake     chan struct{}
}

func newFeed(cam *Camera, thing producer.ExposedThing, property, stream string, idle time.Duration) *feed {
	f := &feed{cam: cam, thing: thing, property: property, stream: stream, idle: idle, wake: make(chan struct{}, 1)}
	thing.SetPropertyReadHandler(property, func(producer.ExposedThing, string, map[string]interface{}) (interface{}, error) {
		if image := f.last(); image != nil {
			return image, nil
		}
		return cam.Snapshot(stream) // nothing kept yet
	})
	thing.SetPropertyObserversHandler(property, f.observers)
	go f.loop()
	return f
}

func (f *feed) last() []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.image
}

// observers starts the fast refresh at the first observer, which gets the
// kept image at once if it is recent, and slows down after the last one
func (f *feed) observers(count int) {
	f.mu.Lock()
	first := f.watchers == 0 && count > 0
	f.watchers = count
	image, recent := f.image, time.Since(f.taken) < freshEnough
	f.mu.Unlock()
	if first {
		if image != nil && recent {
			f.thing.EmitPropertyChange(f.property, image, nil)
		}
		f.refreshNow()
	}
}

// refreshNow takes a new image without waiting for the next period
func (f *feed) refreshNow() {
	select {
	case f.wake <- struct{}{}:
	default: // a refresh is already pending
	}
}

func (f *feed) period() time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.watchers > 0 {
		return watchedPeriod
	}
	return f.idle
}

func (f *feed) loop() {
	for {
		if f.period() <= 0 {
			<-f.wake // not refreshed while nobody watches
		}
		start := time.Now()
		if image, err := f.cam.Snapshot(f.stream); err != nil {
			zlog.Debug().Err(err).Str("camera", f.cam.base).Str("stream", f.stream).Msg("[feed:loop] Snapshot failed, keeping the last image")
		} else {
			f.mu.Lock()
			f.image, f.taken = image, time.Now()
			f.mu.Unlock()
			f.thing.EmitPropertyChange(f.property, image, nil)
		}
		period := f.period()
		if period <= 0 {
			continue
		}
		// A slow camera (1.3 s for a 4K image) is refreshed as fast as it answers
		select {
		case <-time.After(period - time.Since(start)):
		case <-f.wake:
		}
	}
}
