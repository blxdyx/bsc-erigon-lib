// Code generated by protoc-gen-go-grpc. DO NOT EDIT.
// versions:
// - protoc-gen-go-grpc v1.2.0
// - protoc             v3.21.12
// source: downloader/downloader.proto

package downloader

import (
	context "context"
	grpc "google.golang.org/grpc"
	codes "google.golang.org/grpc/codes"
	status "google.golang.org/grpc/status"
	emptypb "google.golang.org/protobuf/types/known/emptypb"
)

// This is a compile-time assertion to ensure that this generated file
// is compatible with the grpc package it is being compiled against.
// Requires gRPC-Go v1.32.0 or later.
const _ = grpc.SupportPackageIsVersion7

// DownloaderClient is the client API for Downloader service.
//
// For semantics around ctx use and closing/ending streaming RPCs, please refer to https://pkg.go.dev/google.golang.org/grpc/?tab=doc#ClientConn.NewStream.
type DownloaderClient interface {
	Download(ctx context.Context, in *DownloadRequest, opts ...grpc.CallOption) (*emptypb.Empty, error)
	Verify(ctx context.Context, in *VerifyRequest, opts ...grpc.CallOption) (*emptypb.Empty, error)
	Stats(ctx context.Context, in *StatsRequest, opts ...grpc.CallOption) (*StatsReply, error)
}

type downloaderClient struct {
	cc grpc.ClientConnInterface
}

func NewDownloaderClient(cc grpc.ClientConnInterface) DownloaderClient {
	return &downloaderClient{cc}
}

func (c *downloaderClient) Download(ctx context.Context, in *DownloadRequest, opts ...grpc.CallOption) (*emptypb.Empty, error) {
	out := new(emptypb.Empty)
	err := c.cc.Invoke(ctx, "/downloader.Downloader/Download", in, out, opts...)
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (c *downloaderClient) Verify(ctx context.Context, in *VerifyRequest, opts ...grpc.CallOption) (*emptypb.Empty, error) {
	out := new(emptypb.Empty)
	err := c.cc.Invoke(ctx, "/downloader.Downloader/Verify", in, out, opts...)
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (c *downloaderClient) Stats(ctx context.Context, in *StatsRequest, opts ...grpc.CallOption) (*StatsReply, error) {
	out := new(StatsReply)
	err := c.cc.Invoke(ctx, "/downloader.Downloader/Stats", in, out, opts...)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// DownloaderServer is the server API for Downloader service.
// All implementations must embed UnimplementedDownloaderServer
// for forward compatibility
type DownloaderServer interface {
	Download(context.Context, *DownloadRequest) (*emptypb.Empty, error)
	Verify(context.Context, *VerifyRequest) (*emptypb.Empty, error)
	Stats(context.Context, *StatsRequest) (*StatsReply, error)
	mustEmbedUnimplementedDownloaderServer()
}

// UnimplementedDownloaderServer must be embedded to have forward compatible implementations.
type UnimplementedDownloaderServer struct {
}

func (UnimplementedDownloaderServer) Download(context.Context, *DownloadRequest) (*emptypb.Empty, error) {
	return nil, status.Errorf(codes.Unimplemented, "method Download not implemented")
}
func (UnimplementedDownloaderServer) Verify(context.Context, *VerifyRequest) (*emptypb.Empty, error) {
	return nil, status.Errorf(codes.Unimplemented, "method Verify not implemented")
}
func (UnimplementedDownloaderServer) Stats(context.Context, *StatsRequest) (*StatsReply, error) {
	return nil, status.Errorf(codes.Unimplemented, "method Stats not implemented")
}
func (UnimplementedDownloaderServer) mustEmbedUnimplementedDownloaderServer() {}

// UnsafeDownloaderServer may be embedded to opt out of forward compatibility for this service.
// Use of this interface is not recommended, as added methods to DownloaderServer will
// result in compilation errors.
type UnsafeDownloaderServer interface {
	mustEmbedUnimplementedDownloaderServer()
}

func RegisterDownloaderServer(s grpc.ServiceRegistrar, srv DownloaderServer) {
	s.RegisterService(&Downloader_ServiceDesc, srv)
}

func _Downloader_Download_Handler(srv interface{}, ctx context.Context, dec func(interface{}) error, interceptor grpc.UnaryServerInterceptor) (interface{}, error) {
	in := new(DownloadRequest)
	if err := dec(in); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return srv.(DownloaderServer).Download(ctx, in)
	}
	info := &grpc.UnaryServerInfo{
		Server:     srv,
		FullMethod: "/downloader.Downloader/Download",
	}
	handler := func(ctx context.Context, req interface{}) (interface{}, error) {
		return srv.(DownloaderServer).Download(ctx, req.(*DownloadRequest))
	}
	return interceptor(ctx, in, info, handler)
}

func _Downloader_Verify_Handler(srv interface{}, ctx context.Context, dec func(interface{}) error, interceptor grpc.UnaryServerInterceptor) (interface{}, error) {
	in := new(VerifyRequest)
	if err := dec(in); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return srv.(DownloaderServer).Verify(ctx, in)
	}
	info := &grpc.UnaryServerInfo{
		Server:     srv,
		FullMethod: "/downloader.Downloader/Verify",
	}
	handler := func(ctx context.Context, req interface{}) (interface{}, error) {
		return srv.(DownloaderServer).Verify(ctx, req.(*VerifyRequest))
	}
	return interceptor(ctx, in, info, handler)
}

func _Downloader_Stats_Handler(srv interface{}, ctx context.Context, dec func(interface{}) error, interceptor grpc.UnaryServerInterceptor) (interface{}, error) {
	in := new(StatsRequest)
	if err := dec(in); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return srv.(DownloaderServer).Stats(ctx, in)
	}
	info := &grpc.UnaryServerInfo{
		Server:     srv,
		FullMethod: "/downloader.Downloader/Stats",
	}
	handler := func(ctx context.Context, req interface{}) (interface{}, error) {
		return srv.(DownloaderServer).Stats(ctx, req.(*StatsRequest))
	}
	return interceptor(ctx, in, info, handler)
}

// Downloader_ServiceDesc is the grpc.ServiceDesc for Downloader service.
// It's only intended for direct use with grpc.RegisterService,
// and not to be introspected or modified (even as a copy)
var Downloader_ServiceDesc = grpc.ServiceDesc{
	ServiceName: "downloader.Downloader",
	HandlerType: (*DownloaderServer)(nil),
	Methods: []grpc.MethodDesc{
		{
			MethodName: "Download",
			Handler:    _Downloader_Download_Handler,
		},
		{
			MethodName: "Verify",
			Handler:    _Downloader_Verify_Handler,
		},
		{
			MethodName: "Stats",
			Handler:    _Downloader_Stats_Handler,
		},
	},
	Streams:  []grpc.StreamDesc{},
	Metadata: "downloader/downloader.proto",
}
