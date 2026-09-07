// gRPC authentication (docs/DESIGN.md): every call carries a
// device's bearer token in `authorization` metadata, except the
// explicit unauthenticated set -- Hello (mutual discovery) and
// Enroll (the QR secret is the authority).  Forget, a device
// revoking its own registration, is authenticated like everything
// else: the "sign out always works" affordance lives client-side.
// Auth ARMS when the first device enrolls; until then the registry
// answers yes to everything, so a fresh install keeps working before
// any QR has ever been minted.
package server

import (
	"context"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	curtilagev1 "github.com/jeffbstewart/curtilage/gen/curtilage/v1"
	"github.com/jeffbstewart/curtilage/internal/devices"
)

// ProtocolVersion is the wire protocol Hello negotiates.  Bump it
// when a client could conclude the other side is too old.
const ProtocolVersion = 1

// unauthenticated is the explicit no-credential set, by full method.
// Deliberately just these two: even Forget is authenticated -- with
// no valid credential there is nothing to forget.
var unauthenticated = map[string]bool{
	"/curtilage.v1.CurtilageService/Hello":  true,
	"/curtilage.v1.CurtilageService/Enroll": true,
}

// Hello implements the unauthenticated discovery door: versions and
// auth methods, nothing that names the house.
func (s *Server) Hello(ctx context.Context, req *curtilagev1.HelloRequest) (*curtilagev1.HelloResponse, error) {
	return &curtilagev1.HelloResponse{
		ProtocolVersions: []uint32{ProtocolVersion},
		AuthMethods:      []string{"bearer"},
	}, nil
}

// Enroll trades a one-use enrollment secret for this device's bearer
// token.
func (s *Server) Enroll(ctx context.Context, req *curtilagev1.EnrollRequest) (*curtilagev1.EnrollResponse, error) {
	if s.Devices == nil {
		return nil, status.Error(codes.Unavailable, "enrollment is not configured on this server")
	}
	name := strings.TrimSpace(req.GetDeviceName())
	if name == "" || len(name) > 64 {
		return nil, status.Error(codes.InvalidArgument, "device_name must be 1-64 characters")
	}
	d, token, err := s.Devices.Enroll(req.GetSecret(), name, time.Now())
	if err != nil {
		return nil, status.Error(codes.PermissionDenied, "enrollment secret is not valid")
	}
	return &curtilagev1.EnrollResponse{Token: token, DeviceId: d.ID}, nil
}

// Forget revokes the calling device's own registration.  The
// interceptor has already authenticated the bearer; the request must
// ALSO name the registration by its token hash -- an innocent
// misdirected message must never delete a credential, so an empty or
// mismatched body is refused, not honoured.  Unarmed (the
// interceptor waved the call through with no bearer), there is
// nothing to forget and nothing to confirm: a quiet no-op.
func (s *Server) Forget(ctx context.Context, req *curtilagev1.ForgetRequest) (*curtilagev1.ForgetResponse, error) {
	if s.Devices == nil {
		return &curtilagev1.ForgetResponse{}, nil
	}
	token, ok := bearer(ctx)
	if !ok {
		return &curtilagev1.ForgetResponse{}, nil // unarmed pass-through
	}
	if req.GetTokenSha256() != devices.HashToken(token) {
		return nil, status.Error(codes.InvalidArgument,
			"forget must name the registration: token_sha256 is the hex SHA-256 of the calling device's own token")
	}
	s.Devices.Forget(token, time.Now())
	return &curtilagev1.ForgetResponse{}, nil
}

// UnaryAuth and StreamAuth enforce the bearer on everything outside
// the unauthenticated set, once armed.
func (s *Server) UnaryAuth() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if err := s.authorize(ctx, info.FullMethod); err != nil {
			return nil, err
		}
		return handler(ctx, req)
	}
}

func (s *Server) StreamAuth() grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		if err := s.authorize(ss.Context(), info.FullMethod); err != nil {
			return err
		}
		return handler(srv, ss)
	}
}

func (s *Server) authorize(ctx context.Context, method string) error {
	if s.Devices == nil || !s.Devices.Armed() || unauthenticated[method] {
		return nil
	}
	token, ok := bearer(ctx)
	if !ok {
		return status.Error(codes.Unauthenticated, "a device token is required (authorization: Bearer <token>)")
	}
	if _, ok := s.Devices.Authenticate(token, time.Now()); !ok {
		return status.Error(codes.Unauthenticated, "the device token is not valid")
	}
	return nil
}

// bearer extracts the token from `authorization: Bearer <token>`.
func bearer(ctx context.Context) (string, bool) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return "", false
	}
	for _, v := range md.Get("authorization") {
		if t, ok := strings.CutPrefix(v, "Bearer "); ok && t != "" {
			return t, true
		}
	}
	return "", false
}
