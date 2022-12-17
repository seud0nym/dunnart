// Copyright © 2019 Kent Gibson <warthog618@gmail.com>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package main

import (
	"context"
	"net"
	"time"

	"github.com/warthog618/config"
	"github.com/warthog618/config/dict"
)

func init() {
	RegisterModule("wan", newWAN)
}

func onlineString(online bool) string {
	if online {
		return "online"
	}
	return "offline"
}

type WAN struct {
	entities   map[string]bool
	online     bool
	ip         string
	linkPoller *PolledSensor
	ipPoller   *PolledSensor
	ps         PubSub
}

func (w *WAN) RefreshLink(forced bool) {
	online := getLink()
	if w.online != online || forced {
		w.online = online
		w.ps.Publish("", onlineString(w.online))
	}
}

func (w *WAN) RefreshIP(forced bool) {
	ip := getIP()
	if w.ip != ip || forced {
		w.ip = ip
		w.ps.Publish("/ip", w.ip)
	}
}

func (w *WAN) Close() {
	w.linkPoller.Close()
	w.ipPoller.Close()
}

func (w *WAN) Sync(ps PubSub) {
	w.ps = ps
	w.linkPoller.Sync(ps)
	w.ipPoller.Sync(ps)
}

func newWAN(cfg *config.Config) SyncCloser {
	defCfg := dict.New(dict.WithMap(
		map[string]interface{}{
			"link.period": "1m",
			"ip.period":   "15m",
			"entities":    []string{"link", "ip"},
		},
	))
	cfg.Append(defCfg)
	entities := map[string]bool{}
	for _, e := range cfg.MustGet("entities").StringSlice() {
		entities[e] = true
	}
	wan := WAN{
		entities: entities,
		online:   getLink(),
		ps:       StubPubSub{},
	}
	if wan.entities["link"] {
		wan.linkPoller = &PolledSensor{
			topic:  "",
			poller: NewPoller(cfg.MustGet("link.period").Duration(), wan.RefreshLink),
			ps:     StubPubSub{},
		}
	}
	if wan.entities["ip"] {
		wan.ipPoller = &PolledSensor{
			topic:  "/ip",
			poller: NewPoller(cfg.MustGet("ip.period").Duration(), wan.RefreshIP),
			ps:     StubPubSub{},
		}
	}
	return &wan
}

func (w *WAN) Config() []EntityConfig {
	var config []EntityConfig
	if w.entities["link"] {
		cfg := map[string]interface{}{
			"name":         "WAN",
			"state_topic":  "~/wan",
			"device_class": "connectivity",
			"payload_on":   "online",
			"payload_off":  "offline",
		}
		config = append(config, EntityConfig{"link", "binary_sensor", cfg})
	}
	if w.entities["ip"] {
		cfg := map[string]interface{}{
			"name":        "WAN IP",
			"state_topic": "~/wan/ip"}
		if w.entities["link"] {
			cfg["availability"] = []map[string]string{
				{"topic": "~"},
				{"topic": "~/wan"},
			}
			cfg["availability_mode"] = "all"
		}
		config = append(config, EntityConfig{"ip", "sensor", cfg})
	}
	return config
}

func getLink() bool {
	r := net.Resolver{
		PreferGo: true,
		Dial:     CloudFlareDNSDialer,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := r.LookupHost(ctx, "www.google.com")
	return err == nil
}

func getIP() string {
	r := net.Resolver{
		PreferGo: true,
		Dial:     OpenDNSDialer,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	addr, err := r.LookupHost(ctx, "myip.opendns.com")
	if err != nil {
		return "unknown"
	}
	return addr[0]
}

// CloudFlareDNSDialer connects to a CloudFlare DNS server
func CloudFlareDNSDialer(ctx context.Context, network, address string) (net.Conn, error) {
	d := net.Dialer{}
	return d.DialContext(ctx, "udp", "1.1.1.1:53")
}

// GoogleDNSDialer connects to a Google DNS server
func GoogleDNSDialer(ctx context.Context, network, address string) (net.Conn, error) {
	d := net.Dialer{}
	return d.DialContext(ctx, "udp", "8.8.8.8:53")
}

// OpenDNSDialer connects to an OpenDNS DNS server
// Note that this assumes the default DNS lookup is functional.
func OpenDNSDialer(ctx context.Context, network, address string) (net.Conn, error) {
	addrs, err := net.LookupHost("resolver1.opendns.com")
	if err != nil {
		return nil, err
	}
	d := net.Dialer{}
	return d.DialContext(ctx, "udp", addrs[0]+":53")
}
