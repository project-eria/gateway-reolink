package main

import (
	"encoding/xml"
	"testing"
)

func TestParseStreams(t *testing.T) {
	value := []byte(`{"Enc":{"audio":0,"channel":0,
		"mainStream":{"bitRate":6144,"frameRate":25,"size":"3840*2160","vType":"h265"},
		"subStream":{"bitRate":256,"frameRate":15,"size":"640*360","vType":"h264"}}}`)
	got, err := parseStreams(value, "10.0.0.50")
	if err != nil {
		t.Fatal(err)
	}
	main, sub := got["main"], got["sub"]
	if main.Codec != "h265" || main.Width != 3840 || main.Height != 2160 || main.FPS != 25 {
		t.Fatalf("main: %+v", main)
	}
	if main.RTSP != "rtsp://10.0.0.50:554/Preview_01_main" || sub.RTSP != "rtsp://10.0.0.50:554/Preview_01_sub" {
		t.Fatalf("rtsp: %s, %s", main.RTSP, sub.RTSP)
	}
	if sub.RTMP != "rtmp://10.0.0.50:1935/bcs/channel0_sub.bcs?channel=0&stream=1" {
		t.Fatalf("rtmp: %s", sub.RTMP)
	}
}

func TestParseStreamsWithoutCodec(t *testing.T) {
	// CX810 firmware 3.1.0.3909 sends no vType
	value := []byte(`{"Enc":{"mainStream":{"frameRate":25,"size":"3840*2160"},"subStream":{"frameRate":10,"size":"640*360"}}}`)
	got, err := parseStreams(value, "10.0.0.50")
	if err != nil {
		t.Fatal(err)
	}
	if got["sub"].Width != 640 || got["main"].Codec != "" {
		t.Fatalf("got %+v", got)
	}
}

func TestParseResponseError(t *testing.T) {
	_, err := parseResponse("GetEnc", []byte(`[{"cmd":"GetEnc","code":1,"error":{"detail":"please login first","rspCode":-6}}]`))
	if e, ok := err.(*apiError); !ok || e.rspCode != rspLoginFirst {
		t.Fatalf("got %v", err)
	}
}

func TestPullAlarms(t *testing.T) {
	// Reolink RLC-820A PullMessages response, trimmed
	data := []byte(`<?xml version="1.0" encoding="UTF-8"?>
<SOAP-ENV:Envelope xmlns:SOAP-ENV="http://www.w3.org/2003/05/soap-envelope" xmlns:wsnt="http://docs.oasis-open.org/wsn/b-2" xmlns:tev="http://www.onvif.org/ver10/events/wsdl" xmlns:tt="http://www.onvif.org/ver10/schema">
<SOAP-ENV:Body><tev:PullMessagesResponse>
<wsnt:NotificationMessage><wsnt:Topic Dialect="http://www.onvif.org/ver10/tev/topicExpression/ConcreteSet">tns1:RuleEngine/MyRuleDetector/PeopleDetect</wsnt:Topic>
<wsnt:Message><tt:Message PropertyOperation="Changed"><tt:Source><tt:SimpleItem Name="Source" Value="000"/></tt:Source>
<tt:Data><tt:SimpleItem Name="State" Value="true"/></tt:Data></tt:Message></wsnt:Message></wsnt:NotificationMessage>
<wsnt:NotificationMessage><wsnt:Topic>tns1:VideoSource/MotionAlarm</wsnt:Topic>
<wsnt:Message><tt:Message PropertyOperation="Changed"><tt:Data><tt:SimpleItem Name="State" Value="false"/></tt:Data></tt:Message></wsnt:Message></wsnt:NotificationMessage>
<wsnt:NotificationMessage><wsnt:Topic>tns1:RuleEngine/CellMotionDetector/Motion</wsnt:Topic>
<wsnt:Message><tt:Message PropertyOperation="Changed"><tt:Data><tt:SimpleItem Name="IsMotion" Value="true"/></tt:Data></tt:Message></wsnt:Message></wsnt:NotificationMessage>
<wsnt:NotificationMessage><wsnt:Topic>tns1:RuleEngine/MyRuleDetector/VehicleDetect</wsnt:Topic>
<wsnt:Message><tt:Message PropertyOperation="Initialized"><tt:Data><tt:SimpleItem Name="State" Value="true"/></tt:Data></tt:Message></wsnt:Message></wsnt:NotificationMessage>
</tev:PullMessagesResponse></SOAP-ENV:Body></SOAP-ENV:Envelope>`)
	var r pullResponse
	if err := xml.Unmarshal(data, &r); err != nil {
		t.Fatal(err)
	}
	// Only the started detection: not the ended one, the initial state, nor the Motion duplicate
	if got := r.alarms(); len(got) != 1 || got[0] != "PeopleDetect" {
		t.Fatalf("got %v", got)
	}
}

func TestSnapshotStream(t *testing.T) {
	for input, want := range map[string]string{"": "sub", "main": "main", "sub": "sub"} {
		var in interface{}
		if input != "" {
			in = map[string]interface{}{"stream": input}
		}
		if got, err := snapshotStream(in); err != nil || got != want {
			t.Fatalf("%q: got %q, %v", input, got, err)
		}
	}
	if _, err := snapshotStream(map[string]interface{}{"stream": "4k"}); err == nil {
		t.Fatal("expected an error for an unknown stream")
	}
}
