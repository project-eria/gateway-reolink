package main

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Reolink cameras serve HTTPS with a self-signed certificate.
// ponytail: verification skipped (LAN only); pin each camera's certificate if
// the cameras ever leave a trusted network
var reolinkHTTP = &http.Client{
	Timeout:   15 * time.Second,
	Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
}

// Reolink "please login first" response code: the token expired or was revoked
const rspLoginFirst = -6

// Camera is a client of the Reolink CGI/JSON API (/cgi-bin/api.cgi).
// It keeps a single token: the cameras only accept a few sessions.
type Camera struct {
	base     string // scheme://host[:port]
	onvifURL string // ONVIF event service
	user     string
	password string

	mu      sync.Mutex
	token   string
	expires time.Time
	// ponytail: sub stream size read once; restart the gateway after changing
	// it on the camera
	subSize Stream
}

func NewCamera(base, user, password string, onvifPort int) *Camera {
	u, _ := url.Parse(base)
	return &Camera{
		base:     strings.TrimRight(base, "/"),
		onvifURL: fmt.Sprintf("http://%s:%d/onvif/event_service", u.Hostname(), onvifPort),
		user:     user,
		password: password,
	}
}

type apiResponse struct {
	Cmd   string          `json:"cmd"`
	Code  int             `json:"code"`
	Value json.RawMessage `json:"value"`
	Error *struct {
		Detail  string `json:"detail"`
		RspCode int    `json:"rspCode"`
	} `json:"error"`
}

type apiError struct {
	cmd     string
	detail  string
	rspCode int
}

func (e *apiError) Error() string {
	return fmt.Sprintf("reolink %s: %s (%d)", e.cmd, e.detail, e.rspCode)
}

// post sends one command and returns its "value"
func (c *Camera) post(cmd, token string, param interface{}) (json.RawMessage, error) {
	q := url.Values{"cmd": {cmd}}
	if token != "" {
		q.Set("token", token)
	}
	body, _ := json.Marshal([]map[string]interface{}{{"cmd": cmd, "action": 0, "param": param}})
	resp, err := reolinkHTTP.Post(c.base+"/cgi-bin/api.cgi?"+q.Encode(), "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	return parseResponse(cmd, data)
}

func parseResponse(cmd string, data []byte) (json.RawMessage, error) {
	var res []apiResponse
	if err := json.Unmarshal(data, &res); err != nil || len(res) == 0 {
		return nil, fmt.Errorf("reolink %s: unexpected response %.100q", cmd, data)
	}
	if res[0].Code != 0 {
		e := &apiError{cmd: cmd, detail: "error"}
		if res[0].Error != nil {
			e.detail, e.rspCode = res[0].Error.Detail, res[0].Error.RspCode
		}
		return nil, e
	}
	return res[0].Value, nil
}

// login returns a valid token, renewed a minute before its lease ends
func (c *Camera) login() (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && time.Now().Before(c.expires) {
		return c.token, nil
	}
	value, err := c.post("Login", "", map[string]interface{}{
		"User": map[string]string{"Version": "0", "userName": c.user, "password": c.password},
	})
	if err != nil {
		return "", err
	}
	var v struct {
		Token struct {
			Name      string `json:"name"`
			LeaseTime int    `json:"leaseTime"`
		} `json:"Token"`
	}
	if err := json.Unmarshal(value, &v); err != nil || v.Token.Name == "" {
		return "", fmt.Errorf("reolink Login: no token in %.100q", value)
	}
	c.token = v.Token.Name
	c.expires = time.Now().Add(time.Duration(v.Token.LeaseTime-60) * time.Second)
	return c.token, nil
}

// invalidate drops a token the camera rejected
func (c *Camera) invalidate(token string) {
	c.mu.Lock()
	if c.token == token {
		c.token = ""
	}
	c.mu.Unlock()
}

// withToken runs fn with a token, logging in again once if the camera rejected it
func withToken[T any](c *Camera, fn func(token string) (T, error)) (T, error) {
	var zero T
	token, err := c.login()
	if err != nil {
		return zero, err
	}
	res, err := fn(token)
	if e, ok := err.(*apiError); ok && e.rspCode == rspLoginFirst {
		c.invalidate(token)
		if token, err = c.login(); err != nil {
			return zero, err
		}
		res, err = fn(token)
	}
	return res, err
}

// Logout frees the session on the camera
func (c *Camera) Logout() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" {
		c.post("Logout", c.token, map[string]interface{}{})
		c.token = ""
	}
}

// Stream describes one encoder output and the URLs serving it
type Stream struct {
	Codec  string `json:"codec,omitempty"` // absent on some firmwares (CX810 3.1.0.3909)
	Width  int    `json:"width"`
	Height int    `json:"height"`
	FPS    int    `json:"fps"`
	RTSP   string `json:"rtsp"`
	RTMP   string `json:"rtmp"`
	FLV    string `json:"flv"`
}

// Streams reads the encoder settings (GetEnc) of channel 0
func (c *Camera) Streams() (map[string]Stream, error) {
	value, err := withToken(c, func(token string) (json.RawMessage, error) {
		return c.post("GetEnc", token, map[string]int{"channel": 0})
	})
	if err != nil {
		return nil, err
	}
	host, _ := url.Parse(c.base)
	return parseStreams(value, host.Hostname())
}

func parseStreams(value json.RawMessage, host string) (map[string]Stream, error) {
	type enc struct {
		Size      string `json:"size"` // "3840*2160"
		VType     string `json:"vType"`
		FrameRate int    `json:"frameRate"`
	}
	var v struct {
		Enc struct {
			MainStream enc `json:"mainStream"`
			SubStream  enc `json:"subStream"`
		} `json:"Enc"`
	}
	if err := json.Unmarshal(value, &v); err != nil {
		return nil, err
	}
	streams := map[string]Stream{}
	for name, e := range map[string]enc{"main": v.Enc.MainStream, "sub": v.Enc.SubStream} {
		if e.Size == "" {
			continue
		}
		w, h, _ := strings.Cut(e.Size, "*")
		width, _ := strconv.Atoi(w)
		height, _ := strconv.Atoi(h)
		// No credentials in the URLs: anyone reading the Thing would get them
		streams[name] = Stream{
			Codec:  e.VType,
			Width:  width,
			Height: height,
			FPS:    e.FrameRate,
			// Codec-less path, the one GetRtspUrl returns: valid whatever the codec
			RTSP: fmt.Sprintf("rtsp://%s:554/Preview_01_%s", host, name),
			RTMP: fmt.Sprintf("rtmp://%s:1935/bcs/channel0_%s.bcs?channel=0&stream=%d", host, name, map[string]int{"main": 0, "sub": 1}[name]),
			FLV:  fmt.Sprintf("http://%s/flv?port=1935&app=bcs&stream=channel0_%s.bcs", host, name),
		}
	}
	if len(streams) == 0 {
		return nil, fmt.Errorf("reolink GetEnc: no stream in %.100q", value)
	}
	return streams, nil
}

// subStreamSize returns the sub stream size, read from the camera once
func (c *Camera) subStreamSize() (Stream, error) {
	c.mu.Lock()
	sub := c.subSize
	c.mu.Unlock()
	if sub.Width > 0 {
		return sub, nil
	}
	streams, err := c.Streams()
	if err != nil {
		return Stream{}, err
	}
	c.mu.Lock()
	c.subSize = streams["sub"]
	c.mu.Unlock()
	return streams["sub"], nil
}

// Snapshot returns a JPEG of the main or sub stream
func (c *Camera) Snapshot(stream string) ([]byte, error) {
	q := url.Values{
		"cmd":      {"Snap"},
		"channel":  {"0"},
		"snapType": {stream},
		"rs":       {strconv.FormatInt(time.Now().UnixNano(), 36)}, // defeats caching
	}
	if stream == "sub" {
		// The camera ignores snapType (RLC-820A fw 3.1.0.956): without a size
		// the image is always full resolution
		sub, err := c.subStreamSize()
		if err != nil {
			return nil, err
		}
		q.Set("width", strconv.Itoa(sub.Width))
		q.Set("height", strconv.Itoa(sub.Height))
	}
	return withToken(c, func(token string) ([]byte, error) {
		q.Set("token", token)
		resp, err := reolinkHTTP.Get(c.base + "/cgi-bin/api.cgi?" + q.Encode())
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		data, err := io.ReadAll(io.LimitReader(resp.Body, 20<<20))
		if err != nil {
			return nil, err
		}
		if !strings.HasPrefix(resp.Header.Get("Content-Type"), "image/") {
			// Errors come back as JSON
			_, err := parseResponse("Snap", data)
			if err == nil {
				err = fmt.Errorf("reolink Snap: not an image")
			}
			return nil, err
		}
		return data, nil
	})
}
