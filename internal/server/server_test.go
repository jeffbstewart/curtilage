package server

import (
	"context"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/timestamppb"

	curtilagev1 "github.com/jeffbstewart/curtilage/gen/curtilage/v1"
	"github.com/jeffbstewart/curtilage/internal/fixture"
	"github.com/jeffbstewart/curtilage/internal/policy"
	"github.com/jeffbstewart/curtilage/internal/store"
)

// client serves s over an in-memory connection and returns a client.
func client(t *testing.T, s *Server) curtilagev1.CurtilageServiceClient {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	gs := grpc.NewServer()
	Register(gs, s)
	go gs.Serve(lis)
	t.Cleanup(gs.Stop)
	conn, err := grpc.NewClient("passthrough:///bufconn",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return curtilagev1.NewCurtilageServiceClient(conn)
}

// loaded is a store fed the whole fixture.
func loaded(t *testing.T) *store.Store {
	t.Helper()
	st := store.New(7 * 24 * time.Hour)
	eng := policy.NewPassthrough()
	for _, r := range fixture.Records(t, fixture.Driveway) {
		store.Feed(st, eng, r)
	}
	return st
}

func TestHandshake(t *testing.T) {
	c := client(t, &Server{Version: "test", DisplayName: "Stewart house", Store: store.New(48 * time.Hour)})
	ctx := context.Background()
	info, err := c.GetServerInfo(ctx, &curtilagev1.GetServerInfoRequest{ApiVersion: 1, Platform: curtilagev1.Platform_PLATFORM_IOS, Build: "1.0 (1)"})
	if err != nil {
		t.Fatal(err)
	}
	if info.GetVersion() != "test" || info.GetDisplayName() != "Stewart house" || info.GetApiVersion() != APIVersion ||
		info.GetMinApiVersion() != MinAPIVersion || info.GetRetention().AsDuration() != 48*time.Hour {
		t.Errorf("info = %v", info)
	}
	// A client that sends no version is a client from before versions
	// existed; it is not refused.
	if _, err := c.GetServerInfo(ctx, &curtilagev1.GetServerInfoRequest{}); err != nil {
		t.Errorf("unversioned client refused: %v", err)
	}
	// One below the floor is.  (MinAPIVersion is 1, so 0 is "unset";
	// exercise the check by lowering the floor's counterpart.)
	if MinAPIVersion > 1 {
		_, err := c.GetServerInfo(ctx, &curtilagev1.GetServerInfoRequest{ApiVersion: MinAPIVersion - 1})
		if status.Code(err) != codes.FailedPrecondition {
			t.Errorf("old client -> %v", err)
		}
	}
}

func TestListCamerasAndEvents(t *testing.T) {
	c := client(t, &Server{Version: "test", Store: loaded(t)})
	ctx := context.Background()
	cams, err := c.ListCameras(ctx, &curtilagev1.ListCamerasRequest{})
	if err != nil || len(cams.GetCameras()) < 4 {
		t.Fatalf("cameras = %v, %v", cams, err)
	}
	var all []*curtilagev1.Event
	token := ""
	for {
		resp, err := c.ListEvents(ctx, &curtilagev1.ListEventsRequest{PageSize: 30, ContinuationToken: token})
		if err != nil {
			t.Fatal(err)
		}
		if len(resp.GetEvents()) == 0 {
			break
		}
		all = append(all, resp.GetEvents()...)
		token = resp.GetContinuationToken()
	}
	if len(all) != 84 {
		t.Fatalf("listed %d events, fixture has 84", len(all))
	}
	for i, e := range all {
		if e.GetId() == "" || e.GetCamera() == "" || e.GetStartedAt() == nil || e.GetKind() != curtilagev1.EventKind_EVENT_KIND_DETECTION ||
			e.GetClip() == curtilagev1.ClipState_CLIP_STATE_UNKNOWN || e.GetDebug().GetSourceId() == "" {
			t.Errorf("event %d malformed: %v", i, e)
		}
		if i > 0 && e.GetStartedAt().AsTime().After(all[i-1].GetStartedAt().AsTime()) {
			t.Errorf("not newest first at %d", i)
		}
	}
	// Camera filter and page cap.
	cam := cams.GetCameras()[0].GetName()
	resp, err := c.ListEvents(ctx, &curtilagev1.ListEventsRequest{Cameras: []string{cam}, PageSize: 10000})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range resp.GetEvents() {
		if e.GetCamera() != cam {
			t.Errorf("filter leaked %s", e.GetCamera())
		}
	}
	if _, err := c.ListEvents(ctx, &curtilagev1.ListEventsRequest{ContinuationToken: "garbage"}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("bad token -> %v", err)
	}
}

func TestWatchEvents(t *testing.T) {
	st := store.New(time.Hour)
	c := client(t, &Server{Version: "test", Store: st})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	t0 := time.Now()
	st.Apply(t0, policy.Change{Op: policy.OpStarted, Event: policy.Event{ID: "old", Camera: "a", Label: "car", Kind: policy.KindDetection, StartedAt: t0.Add(-time.Minute)}})

	stream, err := c.WatchEvents(ctx, &curtilagev1.WatchEventsRequest{Since: timestamppb.New(t0.Add(-time.Hour))})
	if err != nil {
		t.Fatal(err)
	}
	first, err := stream.Recv()
	if err != nil || first.GetEvent().GetId() != "old" || first.GetChange() != curtilagev1.EventChange_EVENT_CHANGE_STARTED {
		t.Fatalf("replay = %v, %v", first, err)
	}
	e := policy.Event{ID: "new", Camera: "a", Label: "person", Kind: policy.KindDetection, StartedAt: t0}
	st.Apply(t0, policy.Change{Op: policy.OpStarted, Event: e})
	e.EndedAt = t0.Add(time.Second)
	e.Clip = policy.ClipFinal
	st.Apply(t0, policy.Change{Op: policy.OpEnded, Event: e})
	live, err := stream.Recv()
	if err != nil || live.GetEvent().GetId() != "new" || live.GetChange() != curtilagev1.EventChange_EVENT_CHANGE_STARTED {
		t.Fatalf("live = %v, %v", live, err)
	}
	ended, err := stream.Recv()
	if err != nil || ended.GetChange() != curtilagev1.EventChange_EVENT_CHANGE_ENDED || ended.GetEvent().GetEndedAt() == nil ||
		ended.GetEvent().GetClip() != curtilagev1.ClipState_CLIP_STATE_FINAL {
		t.Fatalf("ended = %v, %v", ended, err)
	}
	cancel()
	if _, err := stream.Recv(); status.Code(err) != codes.Canceled {
		t.Errorf("after cancel: %v", err)
	}
}
