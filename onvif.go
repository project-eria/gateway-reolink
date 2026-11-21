package main

import (
	"bytes"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	zlog "github.com/rs/zerolog/log"
)

// ONVIF PullPoint subscription, hand-written SOAP: three requests do not
// justify an ONVIF library and its dependencies

const (
	subscriptionTTL = 10 * time.Minute
	pullTimeout     = 30 * time.Second // long polling: the camera answers as soon as an event comes
	retryDelay      = 10 * time.Second
	maxRetryDelay   = 10 * time.Minute // wrong credentials must not lock the camera account
)

var onvifHTTP = &http.Client{Timeout: pullTimeout + 15*time.Second}

// WatchEvents subscribes to the camera events and calls onAlarm with the rule
// name (PeopleDetect, MotionAlarm…) each time a detection starts. It never returns.
func (c *Camera) WatchEvents(onAlarm func(rule string)) {
	delay := retryDelay
	for {
		start := time.Now()
		err := c.pullLoop(onAlarm)
		if time.Since(start) > subscriptionTTL {
			delay = retryDelay // the subscription worked for a while
		}
		zlog.Warn().Err(err).Str("camera", c.base).Stringer("retryIn", delay).Msg("[onvif:WatchEvents] Subscription lost")
		time.Sleep(delay)
		delay = min(delay*2, maxRetryDelay)
	}
}

func (c *Camera) pullLoop(onAlarm func(rule string)) error {
	var created struct {
		Address string `xml:"Body>CreatePullPointSubscriptionResponse>SubscriptionReference>Address"`
	}
	err := c.soap(c.onvifURL, "http://www.onvif.org/ver10/events/wsdl/EventPortType/CreatePullPointSubscriptionRequest",
		`<CreatePullPointSubscription xmlns="http://www.onvif.org/ver10/events/wsdl">`+
			`<InitialTerminationTime>`+duration(subscriptionTTL)+`</InitialTerminationTime>`+
			`</CreatePullPointSubscription>`, &created)
	if err != nil {
		return err
	}
	if created.Address == "" {
		return fmt.Errorf("no subscription address")
	}
	zlog.Info().Str("camera", c.base).Msg("[onvif:pullLoop] Subscribed to events")
	defer c.soap(created.Address, "http://docs.oasis-open.org/wsn/bw-2/SubscriptionManager/UnsubscribeRequest",
		`<Unsubscribe xmlns="http://docs.oasis-open.org/wsn/b-2"/>`, nil)

	renewed := time.Now()
	for {
		if time.Since(renewed) > subscriptionTTL/2 {
			err := c.soap(created.Address, "http://docs.oasis-open.org/wsn/bw-2/SubscriptionManager/RenewRequest",
				`<Renew xmlns="http://docs.oasis-open.org/wsn/b-2"><TerminationTime>`+duration(subscriptionTTL)+`</TerminationTime></Renew>`, nil)
			if err != nil {
				return err
			}
			renewed = time.Now()
		}
		var pulled pullResponse
		err := c.soap(created.Address, "http://www.onvif.org/ver10/events/wsdl/PullPointSubscription/PullMessagesRequest",
			`<PullMessages xmlns="http://www.onvif.org/ver10/events/wsdl">`+
				`<Timeout>`+duration(pullTimeout)+`</Timeout><MessageLimit>20</MessageLimit>`+
				`</PullMessages>`, &pulled)
		if err != nil {
			return err
		}
		for _, rule := range pulled.alarms() {
			onAlarm(rule)
		}
	}
}

type pullResponse struct {
	Messages []struct {
		Topic   string `xml:"Topic"`
		Message struct {
			PropertyOperation string `xml:"PropertyOperation,attr"`
			Items             []struct {
				Name  string `xml:"Name,attr"`
				Value string `xml:"Value,attr"`
			} `xml:"Data>SimpleItem"`
		} `xml:"Message>Message"`
	} `xml:"Body>PullMessagesResponse>NotificationMessage"`
}

// alarms returns the rules whose state turned true. "Initialized" messages
// only report the current state at subscription time.
func (r pullResponse) alarms() []string {
	var rules []string
	for _, m := range r.Messages {
		if m.Message.PropertyOperation != "Changed" {
			continue
		}
		topic := strings.TrimSpace(m.Topic)
		rule := topic[strings.LastIndex(topic, "/")+1:]
		if rule == "Motion" {
			continue // duplicate of MotionAlarm, sent for the same detection
		}
		for _, item := range m.Message.Items {
			if item.Value == "true" {
				rules = append(rules, rule)
				break
			}
		}
	}
	return rules
}

// soap posts a request with WS-Security and WS-Addressing headers and decodes
// the response envelope into out (if not nil)
func (c *Camera) soap(to, action, body string, out interface{}) error {
	envelope := `<?xml version="1.0" encoding="UTF-8"?>` +
		`<s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope" xmlns:a="http://www.w3.org/2005/08/addressing">` +
		`<s:Header>` + c.securityHeader() +
		`<a:Action>` + action + `</a:Action><a:To>` + xmlEscape(to) + `</a:To>` +
		`</s:Header><s:Body>` + body + `</s:Body></s:Envelope>`
	resp, err := onvifHTTP.Post(to, `application/soap+xml; charset=utf-8; action="`+action+`"`, strings.NewReader(envelope))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		var fault struct {
			Reason string `xml:"Body>Fault>Reason>Text"`
		}
		xml.Unmarshal(data, &fault)
		return fmt.Errorf("onvif %s: HTTP %d %s", action[strings.LastIndex(action, "/")+1:], resp.StatusCode, fault.Reason)
	}
	if out == nil {
		return nil
	}
	return xml.Unmarshal(data, out)
}

// securityHeader builds a WS-Security UsernameToken with a password digest
func (c *Camera) securityHeader() string {
	nonce := make([]byte, 16)
	rand.Read(nonce)
	created := time.Now().UTC().Format(time.RFC3339)
	digest := sha1.Sum(bytes.Join([][]byte{nonce, []byte(created), []byte(c.password)}, nil))
	return `<Security s:mustUnderstand="1" xmlns="http://docs.oasis-open.org/wss/2004/01/oasis-200401-wss-wssecurity-secext-1.0.xsd">` +
		`<UsernameToken><Username>` + xmlEscape(c.user) + `</Username>` +
		`<Password Type="http://docs.oasis-open.org/wss/2004/01/oasis-200401-wss-username-token-profile-1.0#PasswordDigest">` +
		base64.StdEncoding.EncodeToString(digest[:]) + `</Password>` +
		`<Nonce EncodingType="http://docs.oasis-open.org/wss/2004/01/oasis-200401-wss-soap-message-security-1.0#Base64Binary">` +
		base64.StdEncoding.EncodeToString(nonce) + `</Nonce>` +
		`<Created xmlns="http://docs.oasis-open.org/wss/2004/01/oasis-200401-wss-wssecurity-utility-1.0.xsd">` + created + `</Created>` +
		`</UsernameToken></Security>`
}

func xmlEscape(s string) string {
	var b strings.Builder
	xml.EscapeText(&b, []byte(s))
	return b.String()
}

// duration formats an xs:duration in seconds (PT600S)
func duration(d time.Duration) string {
	return fmt.Sprintf("PT%dS", int(d.Seconds()))
}
