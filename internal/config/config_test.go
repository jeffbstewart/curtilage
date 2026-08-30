package config

import (
	"strings"
	"testing"
	"time"
)

const minimal = `
mqtt {
  host: "mosquitto.cameras.svc.cluster.local"
  subscriptions: "frigate/#"
}
`

func TestParseAppliesDefaults(t *testing.T) {
	cfg, err := Parse([]byte(minimal), "test")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Mqtt.Port != DefaultPort || cfg.Mqtt.ClientId != DefaultClientID || cfg.Mqtt.PublishPrefix != DefaultPublishPrefix {
		t.Errorf("mqtt defaults not applied: %+v", cfg.Mqtt)
	}
	if cfg.Mqtt.Keepalive.AsDuration() != DefaultKeepalive {
		t.Errorf("keepalive = %v", cfg.Mqtt.Keepalive.AsDuration())
	}
	if cfg.Recording == nil || cfg.Recording.RotateEvery.AsDuration() != DefaultRotateEvery {
		t.Errorf("recording defaults not applied: %+v", cfg.Recording)
	}
	if cfg.Listen != DefaultListen {
		t.Errorf("listen = %q", cfg.Listen)
	}
}

func TestParseKeepsExplicitValues(t *testing.T) {
	src := `
# a comment, because the format allows them
mqtt {
  host: "broker"
  port: 1884
  client_id: "curtilage-test"
  subscriptions: "frigate/#"
  subscriptions: "other/#"
  keepalive { seconds: 60 }
}
recording {
  dir: "/var/lib/curtilage/recordings"
  rotate_every { seconds: 3600 }
}
listen: ":9999"
`
	cfg, err := Parse([]byte(src), "test")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Mqtt.Port != 1884 || len(cfg.Mqtt.Subscriptions) != 2 || cfg.Mqtt.Keepalive.AsDuration() != time.Minute {
		t.Errorf("explicit mqtt values lost: %+v", cfg.Mqtt)
	}
	if cfg.Recording.Dir != "/var/lib/curtilage/recordings" || cfg.Recording.RotateEvery.AsDuration() != time.Hour {
		t.Errorf("explicit recording values lost: %+v", cfg.Recording)
	}
	if cfg.Listen != ":9999" {
		t.Errorf("listen = %q", cfg.Listen)
	}
}

func TestParseRejects(t *testing.T) {
	cases := map[string]string{
		"unknown field":        `mqtt { host: "b" subscriptions: "x/#" hots: "typo" }`,
		"missing host":         `mqtt { subscriptions: "x/#" }`,
		"missing subscription": `mqtt { host: "b" }`,
		"bad port":             `mqtt { host: "b" subscriptions: "x/#" port: 70000 }`,
		"short rotation":       `mqtt { host: "b" subscriptions: "x/#" } recording { rotate_every { seconds: 5 } }`,
		"no mqtt at all":       `listen: ":1"`,
	}
	for name, src := range cases {
		if _, err := Parse([]byte(src), name); err == nil {
			t.Errorf("%s: expected an error", name)
		} else if !strings.Contains(err.Error(), name) && !strings.Contains(err.Error(), "mqtt") && !strings.Contains(err.Error(), "recording") {
			t.Errorf("%s: unhelpful error %v", name, err)
		}
	}
}

func TestCredentialsFromEnv(t *testing.T) {
	t.Setenv("CURTILAGE_MQTT_USER", "curtilage")
	t.Setenv("CURTILAGE_MQTT_PASSWORD", "")
	if _, err := CredentialsFromEnv(); err == nil {
		t.Error("user without password should be rejected")
	}
	t.Setenv("CURTILAGE_MQTT_PASSWORD", "pw")
	c, err := CredentialsFromEnv()
	if err != nil || c.User != "curtilage" || c.Password != "pw" {
		t.Errorf("got %+v, %v", c, err)
	}
}
