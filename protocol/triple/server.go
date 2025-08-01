/*
 * Licensed to the Apache Software Foundation (ASF) under one or more
 * contributor license agreements.  See the NOTICE file distributed with
 * this work for additional information regarding copyright ownership.
 * The ASF licenses this file to You under the Apache License, Version 2.0
 * (the "License"); you may not use this file except in compliance with
 * the License.  You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package triple

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"sync"
)

import (
	hessian "github.com/apache/dubbo-go-hessian2"

	"github.com/dubbogo/gost/log/logger"

	grpc_go "github.com/dubbogo/grpc-go"

	"github.com/dustin/go-humanize"

	"google.golang.org/grpc"
)

import (
	"dubbo.apache.org/dubbo-go/v3/common"
	"dubbo.apache.org/dubbo-go/v3/common/constant"
	"dubbo.apache.org/dubbo-go/v3/config"
	"dubbo.apache.org/dubbo-go/v3/global"
	"dubbo.apache.org/dubbo-go/v3/internal"
	"dubbo.apache.org/dubbo-go/v3/protocol/base"
	"dubbo.apache.org/dubbo-go/v3/protocol/dubbo3"
	"dubbo.apache.org/dubbo-go/v3/protocol/invocation"
	tri "dubbo.apache.org/dubbo-go/v3/protocol/triple/triple_protocol"
	dubbotls "dubbo.apache.org/dubbo-go/v3/tls"
)

// Server is TRIPLE adaptation layer representation. It makes use of tri.Server to
// provide functionality.
type Server struct {
	triServer *tri.Server
	cfg       *global.TripleConfig
	mu        sync.RWMutex
	services  map[string]grpc.ServiceInfo
}

// NewServer creates a new TRIPLE server.
func NewServer(cfg *global.TripleConfig) *Server {
	return &Server{
		cfg:      cfg,
		services: make(map[string]grpc.ServiceInfo),
	}
}

// Start TRIPLE server
func (s *Server) Start(invoker base.Invoker, info *common.ServiceInfo) {
	url := invoker.GetURL()
	addr := url.Location

	var tripleConf *global.TripleConfig

	tripleConfRaw, ok := url.GetAttribute(constant.TripleConfigKey)
	if ok {
		tripleConf = tripleConfRaw.(*global.TripleConfig)
	}

	var callProtocol string
	if tripleConf != nil && tripleConf.Http3 != nil && tripleConf.Http3.Enable {
		callProtocol = constant.CallHTTP3
	} else {
		// HTTP default type is HTTP/2.
		callProtocol = constant.CallHTTP2
	}

	// initialize tri.Server
	s.triServer = tri.NewServer(addr)

	serialization := url.GetParam(constant.SerializationKey, constant.ProtobufSerialization)
	switch serialization {
	case constant.ProtobufSerialization:
	case constant.JSONSerialization:
	case constant.Hessian2Serialization:
	case constant.MsgpackSerialization:
	default:
		panic(fmt.Sprintf("Unsupported serialization: %s", serialization))
	}
	// todo: support opentracing interceptor

	// TODO: move tls config to handleService

	var globalTlsConf *global.TLSConfig
	var tlsConf *tls.Config
	var err error

	// handle tls
	tlsConfRaw, ok := url.GetAttribute(constant.TLSConfigKey)
	if ok {
		globalTlsConf, ok = tlsConfRaw.(*global.TLSConfig)
		if !ok {
			logger.Errorf("TRIPLE Server initialized the TLSConfig configuration failed")
			return
		}
	}
	if dubbotls.IsServerTLSValid(globalTlsConf) {
		tlsConf, err = dubbotls.GetServerTlSConfig(globalTlsConf)
		if err != nil {
			logger.Errorf("TRIPLE Server initialized the TLSConfig configuration failed. err: %v", err)
			return
		}
		logger.Infof("TRIPLE Server initialized the TLSConfig configuration")
	}

	// IDLMode means that this will only be set when
	// the new triple is started in non-IDL mode.
	// TODO: remove IDLMode when config package is removed
	IDLMode := url.GetParam(constant.IDLMode, "")

	var service common.RPCService
	if IDLMode == constant.NONIDL {
		service, _ = url.GetAttribute(constant.RpcServiceKey)
	}

	hanOpts := getHanOpts(url, tripleConf)

	//Set expected codec name from serviceinfo
	hanOpts = append(hanOpts, tri.WithExpectedCodecName(serialization))

	intfName := url.Interface()
	if info != nil {
		// new triple idl mode
		s.handleServiceWithInfo(intfName, invoker, info, hanOpts...)
		s.saveServiceInfo(intfName, info)
	} else if IDLMode == constant.NONIDL {
		// new triple non-idl mode
		reflectInfo := createServiceInfoWithReflection(service)
		s.handleServiceWithInfo(intfName, invoker, reflectInfo, hanOpts...)
		s.saveServiceInfo(intfName, reflectInfo)
	} else {
		// old triple idl mode and old triple non-idl mode
		s.compatHandleService(intfName, url.Group(), url.Version(), hanOpts...)
	}
	internal.ReflectionRegister(s)

	go func() {
		if runErr := s.triServer.Run(callProtocol, tlsConf); runErr != nil {
			logger.Errorf("server serve failed with err: %v", runErr)
		}
	}()
}

// todo(DMwangnima): extract a common function
// RefreshService refreshes Triple Service
func (s *Server) RefreshService(invoker base.Invoker, info *common.ServiceInfo) {
	URL := invoker.GetURL()
	serialization := URL.GetParam(constant.SerializationKey, constant.ProtobufSerialization)
	switch serialization {
	case constant.ProtobufSerialization:
	case constant.JSONSerialization:
	case constant.Hessian2Serialization:
	case constant.MsgpackSerialization:
	default:
		panic(fmt.Sprintf("Unsupported serialization: %s", serialization))
	}
	hanOpts := getHanOpts(URL, s.cfg)
	//Set expected codec name from serviceinfo
	hanOpts = append(hanOpts, tri.WithExpectedCodecName(serialization))
	intfName := URL.Interface()
	if info != nil {
		s.handleServiceWithInfo(intfName, invoker, info, hanOpts...)
		s.saveServiceInfo(intfName, info)
	} else {
		s.compatHandleService(intfName, URL.Group(), URL.Version(), hanOpts...)
	}
}

func getHanOpts(url *common.URL, tripleConf *global.TripleConfig) (hanOpts []tri.HandlerOption) {
	group := url.GetParam(constant.GroupKey, "")
	version := url.GetParam(constant.VersionKey, "")
	hanOpts = append(hanOpts, tri.WithGroup(group), tri.WithVersion(version))

	// Deprecated：use TripleConfig
	// TODO: remove MaxServerSendMsgSize and MaxServerRecvMsgSize when version 4.0.0
	maxServerRecvMsgSize := constant.DefaultMaxServerRecvMsgSize
	if recvMsgSize, convertErr := humanize.ParseBytes(url.GetParam(constant.MaxServerRecvMsgSize, "")); convertErr == nil && recvMsgSize != 0 {
		maxServerRecvMsgSize = int(recvMsgSize)
	}
	hanOpts = append(hanOpts, tri.WithReadMaxBytes(maxServerRecvMsgSize))

	// Deprecated：use TripleConfig
	// TODO: remove MaxServerSendMsgSize and MaxServerRecvMsgSize when version 4.0.0
	maxServerSendMsgSize := constant.DefaultMaxServerSendMsgSize
	if sendMsgSize, convertErr := humanize.ParseBytes(url.GetParam(constant.MaxServerSendMsgSize, "")); convertErr == nil && sendMsgSize != 0 {
		maxServerSendMsgSize = int(sendMsgSize)
	}
	hanOpts = append(hanOpts, tri.WithSendMaxBytes(maxServerSendMsgSize))

	if tripleConf == nil {
		return hanOpts
	}

	if tripleConf.MaxServerRecvMsgSize != "" {
		logger.Debugf("MaxServerRecvMsgSize: %v", tripleConf.MaxServerRecvMsgSize)
		if recvMsgSize, convertErr := humanize.ParseBytes(tripleConf.MaxServerRecvMsgSize); convertErr == nil && recvMsgSize != 0 {
			maxServerRecvMsgSize = int(recvMsgSize)
		}
		hanOpts = append(hanOpts, tri.WithReadMaxBytes(maxServerRecvMsgSize))
	}

	if tripleConf.MaxServerSendMsgSize != "" {
		logger.Debugf("MaxServerSendMsgSize: %v", tripleConf.MaxServerSendMsgSize)
		if sendMsgSize, convertErr := humanize.ParseBytes(tripleConf.MaxServerSendMsgSize); convertErr == nil && sendMsgSize != 0 {
			maxServerSendMsgSize = int(sendMsgSize)
		}
		hanOpts = append(hanOpts, tri.WithSendMaxBytes(maxServerSendMsgSize))
	}

	// todo:// open tracing

	return hanOpts
}

// *Important*, this function is responsible for being compatible with old triple-gen code and non-idl code
// compatHandleService registers handler based on ServiceConfig and provider service.
func (s *Server) compatHandleService(interfaceName string, group, version string, opts ...tri.HandlerOption) {
	providerServices := config.GetProviderConfig().Services
	if len(providerServices) == 0 {
		logger.Info("Provider service map is null, please register ProviderServices")
		return
	}
	for key, providerService := range providerServices {
		if providerService.Interface != interfaceName || providerService.Group != group || providerService.Version != version {
			continue
		}
		// todo(DMwangnima): judge protocol type
		service := config.GetProviderService(key)
		serviceKey := common.ServiceKey(providerService.Interface, providerService.Group, providerService.Version)
		exporter, _ := tripleProtocol.ExporterMap().Load(serviceKey)
		if exporter == nil {
			logger.Warnf("no exporter found for serviceKey: %v", serviceKey)
			continue
		}
		invoker := exporter.(base.Exporter).GetInvoker()
		if invoker == nil {
			panic(fmt.Sprintf("no invoker found for servicekey: %v", serviceKey))
		}
		ds, ok := service.(dubbo3.Dubbo3GrpcService)
		if !ok {
			info := createServiceInfoWithReflection(service)
			s.handleServiceWithInfo(interfaceName, invoker, info, opts...)
			s.saveServiceInfo(interfaceName, info)
			continue
		}
		s.compatSaveServiceInfo(ds.XXX_ServiceDesc())
		// inject invoker, it has all invocation logics
		ds.XXX_SetProxyImpl(invoker)
		s.compatRegisterHandler(interfaceName, ds, opts...)
	}
}

func (s *Server) compatRegisterHandler(interfaceName string, svc dubbo3.Dubbo3GrpcService, opts ...tri.HandlerOption) {
	desc := svc.XXX_ServiceDesc()
	// init unary handlers
	for _, method := range desc.Methods {
		// please refer to protocol/triple/internal/proto/triple_gen/greettriple for procedure examples
		// error could be ignored because base is empty string
		procedure := joinProcedure(interfaceName, method.MethodName)
		_ = s.triServer.RegisterCompatUnaryHandler(procedure, method.MethodName, svc, tri.MethodHandler(method.Handler), opts...)
	}

	// init stream handlers
	for _, stream := range desc.Streams {
		// please refer to protocol/triple/internal/proto/triple_gen/greettriple for procedure examples
		// error could be ignored because base is empty string
		procedure := joinProcedure(interfaceName, stream.StreamName)
		var typ tri.StreamType
		switch {
		case stream.ClientStreams && stream.ServerStreams:
			typ = tri.StreamTypeBidi
		case stream.ClientStreams:
			typ = tri.StreamTypeClient
		case stream.ServerStreams:
			typ = tri.StreamTypeServer
		}
		_ = s.triServer.RegisterCompatStreamHandler(procedure, svc, typ, stream.Handler, opts...)
	}
}

// handleServiceWithInfo injects invoker and create handler based on ServiceInfo
func (s *Server) handleServiceWithInfo(interfaceName string, invoker base.Invoker, info *common.ServiceInfo, opts ...tri.HandlerOption) {
	for _, method := range info.Methods {
		m := method
		procedure := joinProcedure(interfaceName, method.Name)
		switch m.Type {
		case constant.CallUnary:
			_ = s.triServer.RegisterUnaryHandler(
				procedure,
				m.ReqInitFunc,
				func(ctx context.Context, req *tri.Request) (*tri.Response, error) {
					var args []any
					if argsRaw, ok := req.Msg.([]any); ok {
						// non-idl mode, req.Msg consists of many arguments
						for _, argRaw := range argsRaw {
							// refer to createServiceInfoWithReflection, in ReqInitFunc, argRaw is a pointer to real arg.
							// so we have to invoke Elem to get the real arg.
							args = append(args, reflect.ValueOf(argRaw).Elem().Interface())
						}
					} else {
						// triple idl mode and old triple idl mode
						args = append(args, req.Msg)
					}
					attachments := generateAttachments(req.Header())
					// inject attachments
					ctx = context.WithValue(ctx, constant.AttachmentKey, attachments)
					invo := invocation.NewRPCInvocation(m.Name, args, attachments)
					res := invoker.Invoke(ctx, invo)
					// todo(DMwangnima): modify InfoInvoker to get a unified processing logic
					// please refer to server/InfoInvoker.Invoke()
					var triResp *tri.Response
					if existingResp, ok := res.Result().(*tri.Response); ok {
						triResp = existingResp
					} else {
						// please refer to proxy/proxy_factory/ProxyInvoker.Invoke
						triResp = tri.NewResponse([]any{res.Result()})
					}
					for k, v := range res.Attachments() {
						switch val := v.(type) {
						case string:
							tri.AppendToOutgoingContext(ctx, k, val)
						case []string:
							for _, v := range val {
								tri.AppendToOutgoingContext(ctx, k, v)
							}
						}
					}
					return triResp, res.Error()
				},
				opts...,
			)
		case constant.CallClientStream:
			_ = s.triServer.RegisterClientStreamHandler(
				procedure,
				func(ctx context.Context, stream *tri.ClientStream) (*tri.Response, error) {
					var args []any
					args = append(args, m.StreamInitFunc(stream))
					attachments := generateAttachments(stream.RequestHeader())
					// inject attachments
					ctx = context.WithValue(ctx, constant.AttachmentKey, attachments)
					invo := invocation.NewRPCInvocation(m.Name, args, attachments)
					res := invoker.Invoke(ctx, invo)
					if triResp, ok := res.Result().(*tri.Response); ok {
						return triResp, res.Error()
					}
					// please refer to proxy/proxy_factory/ProxyInvoker.Invoke
					triResp := tri.NewResponse([]any{res.Result()})
					return triResp, res.Error()
				},
				opts...,
			)
		case constant.CallServerStream:
			_ = s.triServer.RegisterServerStreamHandler(
				procedure,
				m.ReqInitFunc,
				func(ctx context.Context, req *tri.Request, stream *tri.ServerStream) error {
					var args []any
					args = append(args, req.Msg, m.StreamInitFunc(stream))
					attachments := generateAttachments(req.Header())
					// inject attachments
					ctx = context.WithValue(ctx, constant.AttachmentKey, attachments)
					invo := invocation.NewRPCInvocation(m.Name, args, attachments)
					res := invoker.Invoke(ctx, invo)
					return res.Error()
				},
				opts...,
			)
		case constant.CallBidiStream:
			_ = s.triServer.RegisterBidiStreamHandler(
				procedure,
				func(ctx context.Context, stream *tri.BidiStream) error {
					var args []any
					args = append(args, m.StreamInitFunc(stream))
					attachments := generateAttachments(stream.RequestHeader())
					// inject attachments
					ctx = context.WithValue(ctx, constant.AttachmentKey, attachments)
					invo := invocation.NewRPCInvocation(m.Name, args, attachments)
					res := invoker.Invoke(ctx, invo)
					return res.Error()
				},
				opts...,
			)
		}
	}
}

func (s *Server) saveServiceInfo(interfaceName string, info *common.ServiceInfo) {
	ret := grpc.ServiceInfo{}
	ret.Methods = make([]grpc.MethodInfo, 0, len(info.Methods))
	for _, method := range info.Methods {
		md := grpc.MethodInfo{}
		md.Name = method.Name
		switch method.Type {
		case constant.CallUnary:
			md.IsClientStream = false
			md.IsServerStream = false
		case constant.CallBidiStream:
			md.IsClientStream = true
			md.IsServerStream = true
		case constant.CallClientStream:
			md.IsClientStream = true
			md.IsServerStream = false
		case constant.CallServerStream:
			md.IsClientStream = false
			md.IsServerStream = true
		}
		ret.Methods = append(ret.Methods, md)
	}
	ret.Metadata = info
	s.mu.Lock()
	defer s.mu.Unlock()
	// todo(DMwangnima): using interfaceName is not enough, we need to consider group and version
	s.services[interfaceName] = ret
}

func (s *Server) compatSaveServiceInfo(desc *grpc_go.ServiceDesc) {
	ret := grpc.ServiceInfo{}
	ret.Methods = make([]grpc.MethodInfo, 0, len(desc.Streams)+len(desc.Methods))
	for _, method := range desc.Methods {
		md := grpc.MethodInfo{
			Name:           method.MethodName,
			IsClientStream: false,
			IsServerStream: false,
		}
		ret.Methods = append(ret.Methods, md)
	}
	for _, stream := range desc.Streams {
		md := grpc.MethodInfo{
			Name:           stream.StreamName,
			IsClientStream: stream.ClientStreams,
			IsServerStream: stream.ServerStreams,
		}
		ret.Methods = append(ret.Methods, md)
	}
	ret.Metadata = desc.Metadata
	s.mu.Lock()
	defer s.mu.Unlock()
	s.services[desc.ServiceName] = ret
}

func (s *Server) GetServiceInfo() map[string]grpc.ServiceInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	res := make(map[string]grpc.ServiceInfo, len(s.services))
	for k, v := range s.services {
		res[k] = v
	}
	return res
}

// Stop TRIPLE server
func (s *Server) Stop() {
	_ = s.triServer.Stop()
}

// GracefulStop TRIPLE server
func (s *Server) GracefulStop() {
	shutdownCtx, cancel := context.WithTimeout(context.Background(), constant.DefaultGracefulShutdownTimeout)
	defer cancel()

	if err := s.triServer.GracefulStop(shutdownCtx); err != nil {
		logger.Errorf("Triple server shutdown error: %v", err)
	}
}

// createServiceInfoWithReflection is for non-idl scenario.
// It makes use of reflection to extract method parameters information and create ServiceInfo.
// As a result, Server could use this ServiceInfo to register.
func createServiceInfoWithReflection(svc common.RPCService) *common.ServiceInfo {
	var info common.ServiceInfo
	svcType := reflect.TypeOf(svc)
	methodNum := svcType.NumMethod()

	// +1 for generic call method
	methodInfos := make([]common.MethodInfo, 0, methodNum+1)

	for i := range methodNum {
		methodType := svcType.Method(i)
		if methodType.Name == "Reference" {
			continue
		}
		paramsNum := methodType.Type.NumIn()
		// the first param is receiver itself, the second param is ctx
		// just ignore them
		if paramsNum < 2 {
			logger.Fatalf("TRIPLE does not support %s method that does not have any parameter", methodType.Name)
			continue
		}
		paramsTypes := make([]reflect.Type, paramsNum-2)
		for j := 2; j < paramsNum; j++ {
			paramsTypes[j-2] = methodType.Type.In(j)
		}
		methodInfo := common.MethodInfo{
			Name: methodType.Name,
			// only support Unary invocation now
			Type: constant.CallUnary,
			ReqInitFunc: func() any {
				params := make([]any, len(paramsTypes))
				for k, paramType := range paramsTypes {
					params[k] = reflect.New(paramType).Interface()
				}
				return params
			},
		}
		methodInfos = append(methodInfos, methodInfo)
	}

	// only support no-idl mod call unary
	genericMethodInfo := common.MethodInfo{
		Name: "$invoke",
		Type: constant.CallUnary,
		ReqInitFunc: func() any {
			params := make([]any, 3)
			// params must be pointer
			params[0] = func(s string) *string { return &s }("methodName") // methodName *string
			params[1] = &[]string{}                                        // argv type  *[]string
			params[2] = &[]hessian.Object{}                                // argv       *[]hessian.Object
			return params
		},
	}

	methodInfos = append(methodInfos, genericMethodInfo)

	info.Methods = methodInfos

	return &info
}

// generateAttachments transfer http.Header to map[string]any and make all keys lowercase
func generateAttachments(header http.Header) map[string]any {
	attachments := make(map[string]any, len(header))
	for key, val := range header {
		lowerKey := strings.ToLower(key)
		attachments[lowerKey] = val
	}

	return attachments
}
