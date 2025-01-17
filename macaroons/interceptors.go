package macaroons

import (
	"context"
	"encoding/hex"
	"errors"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"strconv"
)

func (service *Service) UnaryServerInterceptor() grpc.UnaryServerInterceptor {
	return func(
		ctx context.Context,
		req interface{},
		info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler,
	) (interface{}, error) {
		if err := service.validateRequest(ctx, info.FullMethod); err != nil {
			return nil, err
		}

		return handler(ctx, req)
	}
}

func (service *Service) StreamServerInterceptor() grpc.StreamServerInterceptor {
	return func(
		srv interface{},
		ss grpc.ServerStream,
		info *grpc.StreamServerInfo,
		handler grpc.StreamHandler,
	) error {
		if err := service.validateRequest(ss.Context(), info.FullMethod); err != nil {
			return err
		}

		return handler(srv, ss)
	}
}

func (service *Service) validateRequest(ctx context.Context, fullMethod string) error {
	requiredPermissions, foundPermissions := RPCServerPermissions[fullMethod]

	if !foundPermissions {
		return errors.New("could not find permissions requires for method: " + fullMethod)
	}

	md, foundMetadata := metadata.FromIncomingContext(ctx)

	if !foundMetadata {
		return errors.New("could not get metadata from context")
	}

	if len(md["macaroon"]) != 1 {
		return errors.New("expected 1 macaroon, got " + strconv.Itoa(len(md["macaroon"])))
	}

	macBytes, err := hex.DecodeString(md["macaroon"][0])

	if err != nil {
		return err
	}

	return service.ValidateMacaroon(macBytes, requiredPermissions)
}
