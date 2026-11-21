# ERIA Project - Reolink gateway

One Thing per Reolink camera (standalone, no NVR), exposed as
`eria:gateway:reolink:<ref>`, of the eria-core model `Camera` (`latest`,
`latestMain`, `snapshot`, `alarm`) plus `streams`:

- **`streams`** (property): main and sub streams with codec, size, fps and
  their RTSP / RTMP / FLV URLs, read live from the camera (`GetEnc`). The URLs
  carry no credentials.
- **`latest`** (property): sub stream image (640 px), pushed every second to
  its observers while at least one observes it; otherwise refreshed every
  `snapshotRefresh` seconds (default 10) and on each detection. A new observer
  gets the kept image at once if it is less than 15 s old.
- **`latestMain`** (property): main stream image (4K, 1 to 2 MB), pushed every
  second (or as fast as the camera answers, ~1.3 s for the RLC-8xxA) while
  observed only, never captured otherwise.
- **`snapshot`** (action): image taken now, `{"stream": "main"|"sub"}`
  (default `sub`). Takes 0.5 to 3 s depending on the camera.

All three are served over HTTP as raw `image/jpeg` (`GET /<ref>/latest` works
in an `<img>`), and as base64 strings over JSON/WebSocket (TD
`contentMediaType`). Through eria-ui-server, observe them: its HTTP reads
expect JSON of at most 64 KB.
- **`alarm`** (event): start of a detection, received by ONVIF PullPoint. The
  data is the ONVIF rule: `MotionAlarm`, `PeopleDetect`, `VehicleDetect`,
  `DogCatDetect`…

## Camera setup

Settings > Network > Advanced > Server Settings: enable ONVIF (port 8000),
and RTSP / RTMP for the streams. The configured user must be valid for both
the HTTP API and ONVIF.

The gateway keeps one API session and one ONVIF subscription per camera. On
failure it retries with a growing delay (up to 10 min), so wrong credentials
don't lock the account.
See `config.sample.yml`.
