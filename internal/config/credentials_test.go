package config

import (
	"bytes"
	"encoding/base64"
	"testing"
)

func TestCredentialsFromEnvMediaKeys(t *testing.T) {
	k1 := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32))
	k2 := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{2}, 32))
	cases := []struct {
		name, key, prior string
		wantErr          bool
		wantKey, wantOld bool
	}{
		{name: "none"},
		{name: "current only", key: k1, wantKey: true},
		{name: "rotation", key: k2, prior: k1, wantKey: true, wantOld: true},
		{name: "prior without current", prior: k1, wantErr: true},
		{name: "short key", key: base64.StdEncoding.EncodeToString([]byte("short")), wantErr: true},
		{name: "not base64", key: "***", wantErr: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("CURTILAGE_MQTT_USER", "")
			t.Setenv("CURTILAGE_MQTT_PASSWORD", "")
			t.Setenv("CURTILAGE_MEDIA_KEY", c.key)
			t.Setenv("CURTILAGE_MEDIA_KEY_PRIOR", c.prior)
			got, err := CredentialsFromEnv()
			if (err != nil) != c.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, c.wantErr)
			}
			if (got.MediaKey != nil) != c.wantKey || (got.MediaKeyPrior != nil) != c.wantOld {
				t.Errorf("keys = %v/%v, want %v/%v", got.MediaKey != nil, got.MediaKeyPrior != nil, c.wantKey, c.wantOld)
			}
		})
	}
}
