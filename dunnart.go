// SPDX-FileCopyrightText: 2019 Kent Gibson <warthog618@gmail.com>
//
// SPDX-License-Identifier: MIT

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	"github.com/warthog618/config"
	"github.com/warthog618/config/blob"
	cfgyaml "github.com/warthog618/config/blob/decoder/yaml"
	"github.com/warthog618/config/dict"
	"github.com/warthog618/config/env"
	"github.com/warthog618/config/pflag"
)

const (
	mustQos byte = 1
)

var (
	version = "undefined"
)

func loadConfig() *config.Config {
	defCfg := dict.New()
	defCfg.Set("config-file", "dunnart.yaml")
	defCfg.Set("homeassistant.birth_message_topic", "homeassistant/status")
	defCfg.Set("homeassistant.discovery.status_delay", "15s")
	defCfg.Set("homeassistant.discovery.prefix", "homeassistant")
	defCfg.Set("homeassistant.discovery.mac_source", []string{"eth0", "enp3s0", "wlan0"})
	// no meaningful defaults....
	//"mqtt.broker":         "",
	//"mqtt.username":       "",
	//"mqtt.password":       "",

	host, err := os.Hostname()
	if err == nil {
		defCfg.Set("mqtt.base_topic", "dunnart/"+host)
		defCfg.Set("homeassistant.discovery.node_id", host)
	}
	s := config.NewStack(pflag.New(pflag.WithFlags(
		[]pflag.Flag{{Short: 'c', Name: "config-file"}})),
		env.New(env.WithEnvPrefix("DUNNART_")),
	)
	cfg := config.New(s, config.WithDefault(defCfg))
	s.Append(blob.NewConfigFile(
		cfg, "config.file", "dunnart.yaml", cfgyaml.NewDecoder()))
	s.Append(defCfg)
	return config.New(s)
}

func newMQTTOpts(cfg *config.Config) *mqtt.ClientOptions {
	// OrderMatters defaults to true - required for QoS1 ordering
	opts := mqtt.NewClientOptions().AddBroker(cfg.MustGet("broker").String())
	if username, err := cfg.Get("username"); err == nil {
		opts = opts.SetUsername(username.String())
	}
	if password, err := cfg.Get("password"); err == nil {
		opts = opts.SetPassword(password.String())
	}
	return opts
}

type Dunnart struct {
	ps PubSub
}

func (d *Dunnart) Publish() {
	d.ps.Publish("", "online")
	d.ps.Publish("/version", version)
}

func (d *Dunnart) Sync(ps PubSub) {
	d.ps = ps
	d.Publish()
}

func (m *Dunnart) Config() []EntityConfig {
	var config []EntityConfig
	cfg := map[string]interface{}{
		"name":         "status",
		"object_id":    "{{.NodeId}}_status",
		"state_topic":  "~",
		"device_class": "connectivity",
		"payload_on":   "online",
		"payload_off":  "offline",
	}
	config = append(config, EntityConfig{"status", "binary_sensor", cfg})
	return config
}

func connect(mc mqtt.Client, done <-chan struct{}) error {
	tok := mc.Connect()
	select {
	case <-tok.Done():
		return tok.Error()
	case <-done:
		return nil
	}
}

func initialConnect(mc mqtt.Client, done <-chan struct{}) {
	err := connect(mc, done)
	if err == nil {
		return
	}
	log.Printf("connect error: %s", err)
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for range t.C {
		err = connect(mc, done)
		if err != nil {
			log.Printf("connect error: %s", err)
		} else {
			return
		}
	}
}

type ModuleFactory func(cfg *config.Config) SyncCloser

var moduleFactories = map[string]ModuleFactory{}

func RegisterModule(name string, mf ModuleFactory) {
	moduleFactories[name] = mf
}

func main() {
	log.SetFlags(0)

	cfg := loadConfig()

	// capture exit signals to ensure defers are called on the way out.
	sigdone := make(chan os.Signal, 1)
	signal.Notify(sigdone, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigdone)
	done := make(chan struct{})
	go func() {
		select {
		case <-sigdone:
			close(done)
		case <-done:
		}
	}()

	ss := map[string]Syncer{
		"": &Dunnart{},
	}

	mm := cfg.MustGet("modules").StringSlice()
	var defCfg *dict.Getter
	v, err := cfg.Get("period")
	period := v.String()
	if err == nil && len(period) > 0 {
		defCfg = dict.New()
		defCfg.Set("period", period)
	}

	for _, modName := range mm {
		factory := moduleFactories[modName]
		if factory == nil {
			log.Fatalf("unsupported sensor: %s", modName)
		}
		modCfg := cfg.GetConfig(modName)
		if defCfg != nil {
			modCfg.Append(defCfg)
		}
		mod := factory(modCfg)
		ss[modName] = mod
		defer mod.Close()
	}

	connect := make(chan int)
	mqttCfg := cfg.GetConfig("mqtt")
	baseTopic := mqttCfg.MustGet("base_topic").String()
	mOpts := newMQTTOpts(mqttCfg).
		SetWill(baseTopic, "offline", mustQos, false).
		SetOnConnectHandler(func(mc mqtt.Client) {
			select {
			case connect <- 0:
			case <-done:
			}
		})

	mc := mqtt.NewClient(mOpts)
	initialConnect(mc, done)
	defer mc.Disconnect(0)

	disco := newDiscovery(cfg.GetConfig("homeassistant.discovery"), ss, baseTopic)
	habmTopic := cfg.MustGet("homeassistant.birth_message_topic").String()
	// delay for when ha sees the ads for the first time and is slow subscribing
	sdelay := cfg.MustGet("homeassistant.discovery.status_delay").Duration()
	go func() {
		for {
			select {
			case <-done:
				return
			case <-connect:
				log.Print("mqtt connect")
				disco.advertise(mc)
				for modName, s := range ss {
					t := baseTopic
					if len(modName) > 0 {
						t += "/" + modName
					}
					ps := MQTT{mc, t}
					s.Sync(ps)
				}
				mc.Subscribe(habmTopic, mustQos,
					func(mc mqtt.Client, msg mqtt.Message) {
						if string(msg.Payload()) == "online" {
							disco.advertise(mc)
							time.Sleep(sdelay)
							for _, s := range ss {
								s.Publish()
							}
						}
					})
				time.Sleep(sdelay)
				for _, s := range ss {
					s.Publish()
				}

			}
		}
	}()
	<-done
}

type Discovery struct {
	// map from topic to config for discoverable entities
	ents map[string]string
}

func newDiscovery(cfg *config.Config, ss map[string]Syncer, baseTopic string) Discovery {
	ents := map[string]string{}
	prefix := cfg.MustGet("prefix").String()
	if len(prefix) > 0 {
		mac, err := get_mac(cfg)
		if err != nil {
			log.Fatalf("discovery: %v", err)
		}
		uid := cfg.MustGet("unique_id",
			config.WithDefaultValue("dnrt-"+strings.Replace(mac, ":", "", -1))).String()
		nodeId := cfg.MustGet("node_id").String()
		baseCfg := map[string]interface{}{
			"~": baseTopic,
			"device": map[string]interface{}{
				"name":        nodeId,
				"connections": [][]string{{"mac", mac}},
			},
		}
		for modName, s := range ss {
			if a, ok := s.(Discoverable); ok {
				for _, entity := range a.Config() {
					euid := uid
					if len(modName) > 0 {
						euid += "-" + modName
					}
					euid += "-" + entity.name
					topic := strings.Join(
						[]string{prefix,
							entity.class,
							euid,
							"config"},
						"/")
					baseCfg["unique_id"] = euid
					baseCfg["object_id"] = strings.Join([]string{nodeId, modName, entity.name}, "_")
					config := normalise_config(entity.config, baseCfg)
					config = strings.ReplaceAll(config, "{{.NodeId}}", nodeId)
					ents[topic] = config
				}
			}
		}
	}
	return Discovery{ents: ents}
}

func (d *Discovery) advertise(mc mqtt.Client) {
	log.Print("advertise for ha discovery")
	for topic, config := range d.ents {
		mc.Publish(topic, mustQos, false, config)
	}

}

func get_mac(cfg *config.Config) (string, error) {
	ss := cfg.MustGet("mac_source").StringSlice()
	for _, source := range ss {
		v, err := ioutil.ReadFile(fmt.Sprintf("/sys/class/net/%s/address", source))
		if err == nil {
			return strings.TrimSpace(string(v)), nil
		}
	}
	return "", errors.New("can't find mac")
}

func normalise_config(cfg, baseCfg map[string]interface{}) string {
	for k, v := range baseCfg {
		if _, exists := cfg[k]; !exists {
			cfg[k] = v
		}
	}

	if !config_contains(cfg, "availability_topic") && !config_contains(cfg, "availability") {
		cfg["availability_topic"] = "~"
	}
	if cfg["state_topic"] == "~" {
		delete(cfg, "availability_topic")
	}

	config, err := json.Marshal(cfg)
	if err != nil {
		log.Fatalf("failed to marshal JSON: %v", err)
	}

	return string(config)
}

func config_contains(cfg map[string]interface{}, key string) bool {
	_, ok := cfg[key]
	return ok
}

type Syncer interface {
	// Check the current state of contained entities and publish any state changes.
	Sync(PubSub)
	// Publish the current state of the contained entities - no updates.
	Publish()
}

type SyncCloser interface {
	Syncer
	Close()
}

type EntityConfig struct {
	// The name of the entity within the module
	name string
	// The class of the entity, e.g. sensor, binary_sensor, etc
	class string
	// The config message for the entity.
	// This is the base message that is normalised, adding in default fields
	// and performing template substitution, and converted to JSON and sent
	// to the broker.
	config map[string]interface{}
}

type BaseConfig struct {
	BaseTopic string
	NodeId    string
	Mac       string
	UniqueId  string
	ObjectId  string
}

type Discoverable interface {
	Config() []EntityConfig
}

type Pub interface {
	Publish(string, interface{})
}

type PubSub interface {
	Publish(string, interface{})
	Subscribe(string, func([]byte))
}

type MQTT struct {
	mc        mqtt.Client
	baseTopic string
}

func (m MQTT) Publish(topic string, value interface{}) {
	log.Printf("publish %s '%s'", m.baseTopic+topic, fmt.Sprint(value))
	m.mc.Publish(m.baseTopic+topic, mustQos, false, fmt.Sprint(value))
}

func (m MQTT) Subscribe(topic string, callback func([]byte)) {
	wrap := func(m mqtt.Client, msg mqtt.Message) {
		callback(msg.Payload())
	}
	log.Printf("subscribe %s", m.baseTopic+topic)
	m.mc.Subscribe(m.baseTopic+topic, mustQos, wrap)
}

type StubPubSub struct{}

func (s StubPubSub) Publish(topic string, value interface{}) {
}

func (s StubPubSub) Subscribe(topic string, callback func([]byte)) {
}
