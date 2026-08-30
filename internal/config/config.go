// Package config loads the server's protobuf text-format configuration
// (proto/curtilage/v1/config.proto) and applies defaults.
package config

import (
	"fmt"
	"os"
	"time"

	"google.golang.org/protobuf/encoding/prototext"
	"google.golang.org/protobuf/types/known/durationpb"

	curtilagev1 "github.com/jeffbstewart/curtilage/gen/curtilage/v1"
)

// Defaults applied where the file is silent.
const (
	DefaultPort          = 1883
	DefaultClientID      = "curtilage"
	DefaultPublishPrefix = "curtilage"
	DefaultKeepalive     = 30 * time.Second
	DefaultRotateEvery   = 24 * time.Hour
	DefaultListen        = ":9118"
)

// Load reads a textproto file, validates it, and fills defaults.
func Load(path string) (*curtilagev1.Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(b, path)
}

// Parse is Load on bytes; name is used in error messages.
func Parse(b []byte, name string) (*curtilagev1.Config, error) {
	cfg := &curtilagev1.Config{}
	if err := prototext.Unmarshal(b, cfg); err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	if err := applyDefaults(cfg); err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return cfg, nil
}

func applyDefaults(cfg *curtilagev1.Config) error {
	if cfg.Mqtt == nil {
		return fmt.Errorf("mqtt is required")
	}
	m := cfg.Mqtt
	if m.Host == "" {
		return fmt.Errorf("mqtt.host is required")
	}
	if m.Port == 0 {
		m.Port = DefaultPort
	}
	if m.Port > 65535 {
		return fmt.Errorf("mqtt.port %d out of range", m.Port)
	}
	if m.ClientId == "" {
		m.ClientId = DefaultClientID
	}
	if len(m.Subscriptions) == 0 {
		return fmt.Errorf("mqtt.subscriptions is required (e.g. \"frigate/#\")")
	}
	if m.PublishPrefix == "" {
		m.PublishPrefix = DefaultPublishPrefix
	}
	if m.Keepalive == nil {
		m.Keepalive = durationpb.New(DefaultKeepalive)
	} else if d := m.Keepalive.AsDuration(); d < time.Second || d > time.Hour {
		return fmt.Errorf("mqtt.keepalive %v out of range (1s..1h)", d)
	}
	if cfg.Recording == nil {
		cfg.Recording = &curtilagev1.Recording{}
	}
	r := cfg.Recording
	if r.RotateEvery == nil {
		r.RotateEvery = durationpb.New(DefaultRotateEvery)
	} else if d := r.RotateEvery.AsDuration(); d < time.Minute {
		return fmt.Errorf("recording.rotate_every %v too short (>= 1m)", d)
	}
	if cfg.Listen == "" {
		cfg.Listen = DefaultListen
	}
	return nil
}

// Credentials come from the environment, never the file.
type Credentials struct {
	User, Password string
}

// CredentialsFromEnv reads CURTILAGE_MQTT_USER / CURTILAGE_MQTT_PASSWORD;
// both empty means an anonymous connection (the broker decides).
func CredentialsFromEnv() (Credentials, error) {
	u, p := os.Getenv("CURTILAGE_MQTT_USER"), os.Getenv("CURTILAGE_MQTT_PASSWORD")
	if (u == "") != (p == "") {
		return Credentials{}, fmt.Errorf("CURTILAGE_MQTT_USER and CURTILAGE_MQTT_PASSWORD must be set together")
	}
	return Credentials{User: u, Password: p}, nil
}
