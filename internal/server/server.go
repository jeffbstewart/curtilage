// Package server implements curtilage.v1.CurtilageService over the
// store (docs/DESIGN.md "Presentation"): the handshake, the camera
// list, the event list and stream.  Media arrives with the next
// phase.
package server

import (
	"context"
	"errors"
	"log"
	"sort"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	curtilagev1 "github.com/jeffbstewart/curtilage/gen/curtilage/v1"
	"github.com/jeffbstewart/curtilage/internal/captoken"
	"github.com/jeffbstewart/curtilage/internal/frigate"
	"github.com/jeffbstewart/curtilage/internal/policy"
	"github.com/jeffbstewart/curtilage/internal/store"
)

// The API versions this server implements and still accepts.
const (
	APIVersion    = 1
	MinAPIVersion = 1
)

// Page sizes for ListEvents.
const (
	DefaultPageSize = 50
	MaxPageSize     = 500
)

// Server is the service.  Register it with Register.
type Server struct {
	curtilagev1.UnimplementedCurtilageServiceServer
	Version     string
	DisplayName string
	Store       *store.Store
	// Media (media.go).  Frigate nil: no media at all.  Keys nil: no
	// capability links, GetMedia only.
	Frigate *frigate.Client
	Keys    *captoken.Keyring
	LinkTTL time.Duration
}

// Register attaches s to gs.
func Register(gs *grpc.Server, s *Server) { curtilagev1.RegisterCurtilageServiceServer(gs, s) }

// GetServerInfo is the handshake.
func (s *Server) GetServerInfo(_ context.Context, req *curtilagev1.GetServerInfoRequest) (*curtilagev1.GetServerInfoResponse, error) {
	if v := req.GetApiVersion(); v != 0 && v < MinAPIVersion {
		return nil, status.Errorf(codes.FailedPrecondition, "client API version %d is too old; this server needs %d or newer", v, MinAPIVersion)
	}
	log.Printf("api: hello from %s api=%d build=%q", req.GetPlatform(), req.GetApiVersion(), req.GetBuild())
	return &curtilagev1.GetServerInfoResponse{
		Version:       s.Version,
		MinApiVersion: MinAPIVersion,
		ApiVersion:    APIVersion,
		DisplayName:   s.DisplayName,
		Retention:     durationpb.New(s.Store.Retention()),
	}, nil
}

// ListCameras is every camera the broker has mentioned.
func (s *Server) ListCameras(context.Context, *curtilagev1.ListCamerasRequest) (*curtilagev1.ListCamerasResponse, error) {
	resp := &curtilagev1.ListCamerasResponse{}
	for _, name := range s.Store.Cameras() {
		resp.Cameras = append(resp.Cameras, &curtilagev1.Camera{Name: name})
	}
	return resp, nil
}

// ListEvents is one page, newest first.
func (s *Server) ListEvents(_ context.Context, req *curtilagev1.ListEventsRequest) (*curtilagev1.ListEventsResponse, error) {
	size := int(req.GetPageSize())
	switch {
	case size <= 0:
		size = DefaultPageSize
	case size > MaxPageSize:
		size = MaxPageSize
	}
	events, next, err := s.Store.List(req.GetCameras(), size, req.GetContinuationToken())
	if errors.Is(err, store.ErrBadToken) {
		return nil, status.Error(codes.InvalidArgument, "continuation_token is not one this server issued")
	}
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	resp := &curtilagev1.ListEventsResponse{ContinuationToken: next}
	for _, e := range events {
		resp.Events = append(resp.Events, ToProto(e))
	}
	return resp, nil
}

// WatchEvents streams changes until the client goes away.
func (s *Server) WatchEvents(req *curtilagev1.WatchEventsRequest, stream grpc.ServerStreamingServer[curtilagev1.WatchEventsResponse]) error {
	var since time.Time
	if req.Since != nil {
		since = req.Since.AsTime()
	}
	ch := s.Store.Watch(stream.Context(), since, req.GetCameras())
	for c := range ch {
		if err := stream.Send(&curtilagev1.WatchEventsResponse{Change: opToProto(c.Op), Event: ToProto(c.Event)}); err != nil {
			return err
		}
	}
	if stream.Context().Err() != nil {
		return nil // the client left
	}
	return status.Error(codes.ResourceExhausted, "watcher fell too far behind; watch again with since")
}

// ToProto is the wire form of an event.
func ToProto(e policy.Event) *curtilagev1.Event {
	out := &curtilagev1.Event{
		Id:          e.ID,
		Camera:      e.Camera,
		Label:       e.Label,
		SubLabel:    e.SubLabel,
		Zones:       e.Zones,
		Kind:        kindToProto(e.Kind),
		StartedAt:   timestamppb.New(e.StartedAt),
		HasSnapshot: e.HasSnapshot,
		Clip:        clipToProto(e.Clip),
		Debug:       &curtilagev1.EventDebug{SourceId: e.SourceID, SourceIds: e.SourceIDs},
		Path:        e.Path,
		Cameras:     e.Cameras,
	}
	if !e.EndedAt.IsZero() {
		out.EndedAt = timestamppb.New(e.EndedAt)
	}
	labels := make([]string, 0, len(e.Objects))
	for l := range e.Objects {
		labels = append(labels, l)
	}
	sort.Strings(labels)
	for _, l := range labels {
		out.Objects = append(out.Objects, &curtilagev1.ObjectCount{Label: l, Count: uint32(e.Objects[l])})
	}
	for _, sp := range e.Spans {
		p := &curtilagev1.CameraSpan{Camera: sp.Camera, Label: sp.Label, StartedAt: timestamppb.New(sp.Start)}
		if !sp.End.IsZero() {
			p.EndedAt = timestamppb.New(sp.End)
		}
		out.Spans = append(out.Spans, p)
	}
	return out
}

func kindToProto(k policy.Kind) curtilagev1.EventKind {
	switch k {
	case policy.KindArrival:
		return curtilagev1.EventKind_EVENT_KIND_ARRIVAL
	case policy.KindDeparture:
		return curtilagev1.EventKind_EVENT_KIND_DEPARTURE
	case policy.KindPackage:
		return curtilagev1.EventKind_EVENT_KIND_PACKAGE
	case policy.KindDetection:
		return curtilagev1.EventKind_EVENT_KIND_DETECTION
	case policy.KindActivity:
		return curtilagev1.EventKind_EVENT_KIND_ACTIVITY
	}
	return curtilagev1.EventKind_EVENT_KIND_UNKNOWN
}

func clipToProto(c policy.ClipState) curtilagev1.ClipState {
	switch c {
	case policy.ClipNone:
		return curtilagev1.ClipState_CLIP_STATE_NONE
	case policy.ClipGrowing:
		return curtilagev1.ClipState_CLIP_STATE_GROWING
	case policy.ClipFinal:
		return curtilagev1.ClipState_CLIP_STATE_FINAL
	}
	return curtilagev1.ClipState_CLIP_STATE_UNKNOWN
}

func opToProto(op policy.Op) curtilagev1.EventChange {
	switch op {
	case policy.OpStarted:
		return curtilagev1.EventChange_EVENT_CHANGE_STARTED
	case policy.OpUpdated:
		return curtilagev1.EventChange_EVENT_CHANGE_UPDATED
	case policy.OpEnded:
		return curtilagev1.EventChange_EVENT_CHANGE_ENDED
	}
	return curtilagev1.EventChange_EVENT_CHANGE_UNKNOWN
}
