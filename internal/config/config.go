// Package config loads the server's protobuf text-format configuration
// (proto/curtilage/v1/config.proto) and applies defaults.
package config

import (
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"google.golang.org/protobuf/encoding/prototext"
	"google.golang.org/protobuf/types/known/durationpb"

	curtilagev1 "github.com/jeffbstewart/curtilage/gen/curtilage/v1"
	"github.com/jeffbstewart/curtilage/internal/captoken"
)

// Defaults applied where the file is silent.
const (
	DefaultPort          = 1883
	DefaultClientID      = "curtilage"
	DefaultPublishPrefix = "curtilage"
	DefaultKeepalive     = 30 * time.Second
	DefaultRotateEvery   = 24 * time.Hour
	DefaultRetention     = 7 * 24 * time.Hour
	DefaultListen        = ":9118"
	DefaultDisplayName   = "curtilage"
	DefaultLinkTTL       = 4 * time.Hour
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
	if r.Retention == nil {
		r.Retention = durationpb.New(DefaultRetention)
	} else if d := r.Retention.AsDuration(); d < time.Hour {
		return fmt.Errorf("recording.retention %v too short (>= 1h)", d)
	}
	if cfg.Listen == "" {
		cfg.Listen = DefaultListen
	}
	if cfg.DisplayName == "" {
		cfg.DisplayName = DefaultDisplayName
	}
	if _, err := ParseCIDRs(cfg.GetHouse().GetAllowCidrs()); err != nil {
		return fmt.Errorf("house.allow_cidrs: %w", err)
	}
	if _, err := ParseCIDRs(cfg.GetHouse().GetTrustedProxies()); err != nil {
		return fmt.Errorf("house.trusted_proxies: %w", err)
	}
	if cfg.Links == nil {
		cfg.Links = &curtilagev1.Links{}
	}
	if cfg.Links.Ttl == nil {
		cfg.Links.Ttl = durationpb.New(DefaultLinkTTL)
	} else if d := cfg.Links.Ttl.AsDuration(); d < time.Minute || d > 7*24*time.Hour {
		return fmt.Errorf("links.ttl %v out of range (1m..7d)", d)
	}
	return nil
}

// ParseCIDRs parses "a.b.c.d/n" (or v6) entries; a bare address is
// taken as a /32 (/128).
func ParseCIDRs(entries []string) ([]*net.IPNet, error) {
	var out []*net.IPNet
	for _, e := range entries {
		if !strings.Contains(e, "/") {
			ip := net.ParseIP(e)
			if ip == nil {
				return nil, fmt.Errorf("%q is not an address or CIDR", e)
			}
			bits := 32
			if ip.To4() == nil {
				bits = 128
			}
			e = fmt.Sprintf("%s/%d", ip, bits)
		}
		_, n, err := net.ParseCIDR(e)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, nil
}

// Credentials come from the environment, never the file.
type Credentials struct {
	User, Password string
	// Media link signing keys, decoded (config.proto Media): the
	// current key, and the prior one during a rotation or nil.
	MediaKey, MediaKeyPrior []byte
}

// CredentialsFromEnv reads CURTILAGE_MQTT_USER / CURTILAGE_MQTT_PASSWORD
// (both empty means an anonymous connection; the broker decides) and
// CURTILAGE_MEDIA_KEY / CURTILAGE_MEDIA_KEY_PRIOR (base64, 32+ bytes;
// empty means no media links can be minted).
func CredentialsFromEnv() (Credentials, error) {
	u, p := os.Getenv("CURTILAGE_MQTT_USER"), os.Getenv("CURTILAGE_MQTT_PASSWORD")
	if (u == "") != (p == "") {
		return Credentials{}, fmt.Errorf("CURTILAGE_MQTT_USER and CURTILAGE_MQTT_PASSWORD must be set together")
	}
	c := Credentials{User: u, Password: p}
	var err error
	if c.MediaKey, err = captoken.ParseKey(os.Getenv("CURTILAGE_MEDIA_KEY")); err != nil {
		return Credentials{}, fmt.Errorf("CURTILAGE_MEDIA_KEY: %w", err)
	}
	if c.MediaKeyPrior, err = captoken.ParseKey(os.Getenv("CURTILAGE_MEDIA_KEY_PRIOR")); err != nil {
		return Credentials{}, fmt.Errorf("CURTILAGE_MEDIA_KEY_PRIOR: %w", err)
	}
	if c.MediaKey == nil && c.MediaKeyPrior != nil {
		return Credentials{}, fmt.Errorf("CURTILAGE_MEDIA_KEY_PRIOR is set but CURTILAGE_MEDIA_KEY is not")
	}
	return c, nil
}
