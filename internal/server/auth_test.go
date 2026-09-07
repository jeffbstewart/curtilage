package server

import (
	"context"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	curtilagev1 "github.com/jeffbstewart/curtilage/gen/curtilage/v1"
	"github.com/jeffbstewart/curtilage/internal/devices"
	"github.com/jeffbstewart/curtilage/internal/store"
)

func authed(ctx context.Context, token string) context.Context {
	return metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+token)
}

func TestAuthLifecycle(t *testing.T) {
	dr, err := devices.New("")
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{Version: "test", DisplayName: "the house", Store: store.New(time.Hour), Devices: dr}
	c := client(t, s)
	ctx := context.Background()

	// Unarmed: everything answers, credential or no.
	if _, err := c.ListEvents(ctx, &curtilagev1.ListEventsRequest{}); err != nil {
		t.Fatalf("unarmed ListEvents: %v", err)
	}
	// Hello never needs a credential and never names the house.
	hello, err := c.Hello(ctx, &curtilagev1.HelloRequest{ProtocolVersions: []uint32{1}})
	if err != nil || len(hello.GetProtocolVersions()) == 0 || hello.GetAuthMethods()[0] != "bearer" {
		t.Fatalf("hello: %+v %v", hello, err)
	}

	// A bad secret is refused; a minted one enrolls and ARMS.
	if _, err := c.Enroll(ctx, &curtilagev1.EnrollRequest{Secret: "junk", DeviceName: "x"}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("bad secret -> %v", err)
	}
	secret := dr.MintEnrollment(time.Now())
	enr, err := c.Enroll(ctx, &curtilagev1.EnrollRequest{Secret: secret, DeviceName: "Jeff's iPhone"})
	if err != nil || enr.GetToken() == "" || enr.GetDeviceId() == "" {
		t.Fatalf("enroll: %+v %v", enr, err)
	}

	// Armed: no credential is turned away, the token is let in, and
	// Hello still answers cold.
	if _, err := c.ListEvents(ctx, &curtilagev1.ListEventsRequest{}); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("armed, no token -> %v", err)
	}
	if _, err := c.ListEvents(authed(ctx, "wrong"), &curtilagev1.ListEventsRequest{}); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("armed, wrong token -> %v", err)
	}
	if _, err := c.ListEvents(authed(ctx, enr.GetToken()), &curtilagev1.ListEventsRequest{}); err != nil {
		t.Fatalf("armed, good token: %v", err)
	}
	if _, err := c.Hello(ctx, &curtilagev1.HelloRequest{}); err != nil {
		t.Fatalf("armed Hello: %v", err)
	}
	// GetServerInfo names the house, so it is NOT in the cold set.
	if _, err := c.GetServerInfo(ctx, &curtilagev1.GetServerInfoRequest{}); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("cold GetServerInfo -> %v", err)
	}
	// The stream side is gated by the same rule.
	w, err := c.WatchEvents(ctx, &curtilagev1.WatchEventsRequest{})
	if err == nil {
		_, err = w.Recv()
	}
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("cold WatchEvents -> %v", err)
	}

	// Logout succeeds without a credential, with a nonsense one, and
	// with the real one -- which revokes it.
	if _, err := c.Logout(ctx, &curtilagev1.LogoutRequest{}); err != nil {
		t.Fatalf("cold logout: %v", err)
	}
	if _, err := c.Logout(authed(ctx, "nonsense"), &curtilagev1.LogoutRequest{}); err != nil {
		t.Fatalf("nonsense logout: %v", err)
	}
	if _, err := c.Logout(authed(ctx, enr.GetToken()), &curtilagev1.LogoutRequest{}); err != nil {
		t.Fatalf("real logout: %v", err)
	}
	// The only device is gone: unarmed again, open again.
	if _, err := c.ListEvents(ctx, &curtilagev1.ListEventsRequest{}); err != nil {
		t.Fatalf("post-logout ListEvents: %v", err)
	}
}
