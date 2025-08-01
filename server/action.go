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

package server

import (
	"container/list"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

import (
	"github.com/dubbogo/gost/log/logger"
	gxnet "github.com/dubbogo/gost/net"

	perrors "github.com/pkg/errors"

	"go.uber.org/atomic"
)

import (
	"dubbo.apache.org/dubbo-go/v3/common"
	"dubbo.apache.org/dubbo-go/v3/common/constant"
	"dubbo.apache.org/dubbo-go/v3/common/extension"
	"dubbo.apache.org/dubbo-go/v3/config"
	"dubbo.apache.org/dubbo-go/v3/global"
	"dubbo.apache.org/dubbo-go/v3/graceful_shutdown"
	"dubbo.apache.org/dubbo-go/v3/protocol/base"
	"dubbo.apache.org/dubbo-go/v3/protocol/protocolwrapper"
)

// Prefix returns dubbo.service.${InterfaceName}.
func (svcOpts *ServiceOptions) Prefix() string {
	return strings.Join([]string{constant.ServiceConfigPrefix, svcOpts.Id}, ".")
}

func (svcOpts *ServiceOptions) check() error {
	srv := svcOpts.Service
	// check if the limiter has been imported
	if srv.TpsLimiter != "" {
		_, err := extension.GetTpsLimiter(srv.TpsLimiter)
		if err != nil {
			panic(err)
		}
	}
	if srv.TpsLimitStrategy != "" {
		_, err := extension.GetTpsLimitStrategyCreator(srv.TpsLimitStrategy)
		if err != nil {
			panic(err)
		}
	}
	if srv.TpsLimitRejectedHandler != "" {
		_, err := extension.GetRejectedExecutionHandler(srv.TpsLimitRejectedHandler)
		if err != nil {
			panic(err)
		}
	}

	if srv.TpsLimitInterval != "" {
		tpsLimitInterval, err := strconv.ParseInt(srv.TpsLimitInterval, 0, 0)
		if err != nil {
			return fmt.Errorf("[ServiceConfig] Cannot parse the configuration tps.limit.interval for service %svcOpts, please check your configuration", srv.Interface)
		}
		if tpsLimitInterval < 0 {
			return fmt.Errorf("[ServiceConfig] The configuration tps.limit.interval for service %svcOpts must be positive, please check your configuration", srv.Interface)
		}
	}

	if srv.TpsLimitRate != "" {
		tpsLimitRate, err := strconv.ParseInt(srv.TpsLimitRate, 0, 0)
		if err != nil {
			return fmt.Errorf("[ServiceConfig] Cannot parse the configuration tps.limit.rate for service %svcOpts, please check your configuration", srv.Interface)
		}
		if tpsLimitRate < 0 {
			return fmt.Errorf("[ServiceConfig] The configuration tps.limit.rate for service %svcOpts must be positive, please check your configuration", srv.Interface)
		}
	}
	return nil
}

// InitExported will set exported as false atom bool
func (svcOpts *ServiceOptions) InitExported() {
	svcOpts.exported = atomic.NewBool(false)
}

// IsExport will return whether the service config is exported or not
func (svcOpts *ServiceOptions) IsExport() bool {
	return svcOpts.exported.Load()
}

// Get Random Port
func getRandomPort(protocolConfigs []*global.ProtocolConfig) *list.List {
	ports := list.New()
	for _, proto := range protocolConfigs {
		if len(proto.Port) > 0 {
			continue
		}

		tcp, err := gxnet.ListenOnTCPRandomPort(proto.Ip)
		if err != nil {
			panic(perrors.New(fmt.Sprintf("Get tcp port error, err is {%v}", err)))
		}
		defer tcp.Close()
		ports.PushBack(strings.Split(tcp.Addr().String(), ":")[1])
	}
	return ports
}

func (svcOpts *ServiceOptions) ExportWithoutInfo() error {
	return svcOpts.export(nil)
}

func (svcOpts *ServiceOptions) ExportWithInfo(info *common.ServiceInfo) error {
	return svcOpts.export(info)
}

func (svcOpts *ServiceOptions) export(info *common.ServiceInfo) error {
	svcConf := svcOpts.Service
	if info != nil {
		if svcConf.Interface == "" {
			svcConf.Interface = info.InterfaceName
		}
		svcOpts.info = info
	}

	svcOpts.Id = common.GetReference(svcOpts.rpcService)

	// TODO: delay needExport
	if svcOpts.unexported != nil && svcOpts.unexported.Load() {
		err := perrors.Errorf("The service %v has already unexported!", svcConf.Interface)
		logger.Errorf(err.Error())
		return err
	}
	if svcOpts.exported != nil && svcOpts.exported.Load() {
		logger.Warnf("The service %v has already exported!", svcConf.Interface)
		return nil
	}

	regUrls := make([]*common.URL, 0)
	if !svcConf.NotRegister {
		regUrls = config.LoadRegistries(svcConf.RegistryIDs, svcOpts.registriesCompat, common.PROVIDER)
	}

	urlMap := svcOpts.getUrlMap()
	protocolConfigs := loadProtocol(svcConf.ProtocolIDs, svcOpts.Protocols)
	if len(protocolConfigs) == 0 {
		logger.Warnf("The service %v'svcOpts '%v' protocols don't has right protocolConfigs, Please check your configuration center and transfer protocol ", svcConf.Interface, svcConf.ProtocolIDs)
		return nil
	}

	var invoker base.Invoker
	ports := getRandomPort(protocolConfigs)
	nextPort := ports.Front()
	for _, protocolConf := range protocolConfigs {
		// *important* Register should have been replaced by processing of ServiceInfo.
		// but many modules like metadata need to make use of information from ServiceMap.
		// todo(DMwangnimg): finish replacing procedure

		// registry the service reflect
		methods, err := common.ServiceMap.Register(svcConf.Interface, protocolConf.Name, svcConf.Group, svcConf.Version, svcOpts.rpcService)
		if err != nil {
			formatErr := perrors.Errorf("The service %v needExport the protocol %v error! Error message is %v.",
				svcConf.Interface, protocolConf.Name, err.Error())
			logger.Errorf(formatErr.Error())
			return formatErr
		}

		port := protocolConf.Port
		if len(protocolConf.Port) == 0 {
			port = nextPort.Value.(string)
			nextPort = nextPort.Next()
		}

		// Ensure that isIDL does not have any other invalid inputs.
		isIDL := constant.IDL
		if svcOpts.IDLMode == constant.NONIDL {
			isIDL = svcOpts.IDLMode
		}

		ivkURL := common.NewURLWithOptions(
			common.WithPath(svcConf.Interface),
			common.WithProtocol(protocolConf.Name),
			common.WithIp(protocolConf.Ip),
			common.WithPort(port),
			common.WithParams(urlMap),
			common.WithParamsValue(constant.BeanNameKey, svcOpts.Id),
			common.WithParamsValue(constant.ApplicationTagKey, svcOpts.Application.Tag),
			//common.WithParamsValue(constant.SslEnabledKey, strconv.FormatBool(config.GetSslEnabled())),
			common.WithMethods(strings.Split(methods, ",")),
			// TLSConifg
			common.WithAttribute(constant.TLSConfigKey, svcOpts.srvOpts.TLS),
			common.WithAttribute(constant.RpcServiceKey, svcOpts.rpcService),
			common.WithAttribute(constant.TripleConfigKey, protocolConf.TripleConfig),
			common.WithToken(svcConf.Token),
			common.WithParamsValue(constant.MetadataTypeKey, svcOpts.metadataType),

			// fix https://github.com/apache/dubbo-go/issues/2176
			// TODO: remove MaxServerSendMsgSize value and MaxServerRecvMsgSize value when version 4.0.0
			// use TripleConfig to transport arguments
			common.WithParamsValue(constant.MaxServerSendMsgSize, protocolConf.MaxServerSendMsgSize),
			common.WithParamsValue(constant.MaxServerRecvMsgSize, protocolConf.MaxServerRecvMsgSize),

			// TODO: remove IDL value when version 4.0.0
			common.WithParamsValue(constant.IDLMode, isIDL),
		)

		if info != nil {
			ivkURL.SetAttribute(constant.ServiceInfoKey, info)
		}

		if len(svcConf.Tag) > 0 {
			ivkURL.AddParam(constant.Tagkey, svcConf.Tag)
		}

		// post process the URL to be exported
		svcOpts.postProcessConfig(ivkURL)
		// config post processor may set "needExport" to false
		if !ivkURL.GetParamBool(constant.ExportKey, true) {
			return nil
		}

		if len(regUrls) > 0 {
			svcOpts.cacheMutex.Lock()
			if svcOpts.cacheProtocol == nil {
				logger.Debugf(fmt.Sprintf("First load the registry protocol, url is {%v}!", ivkURL))
				svcOpts.cacheProtocol = extension.GetProtocol(constant.RegistryProtocol)
			}
			svcOpts.cacheMutex.Unlock()

			for _, regUrl := range regUrls {
				setRegistrySubURL(ivkURL, regUrl)
				invoker = svcOpts.generatorInvoker(regUrl, info)
				exporter := svcOpts.cacheProtocol.Export(invoker)
				if exporter == nil {
					return perrors.New(fmt.Sprintf("Registry protocol new exporter error, registry is {%v}, url is {%v}", regUrl, ivkURL))
				}
				svcOpts.exporters = append(svcOpts.exporters, exporter)
			}
		} else {
			invoker = svcOpts.generatorInvoker(ivkURL, info)
			exporter := extension.GetProtocol(protocolwrapper.FILTER).Export(invoker)
			if exporter == nil {
				return perrors.New(fmt.Sprintf("Filter protocol without registry new exporter error, url is {%v}", ivkURL))
			}
			svcOpts.exporters = append(svcOpts.exporters, exporter)
		}
		// this protocol would be destroyed in graceful_shutdown
		// please refer to (https://github.com/apache/dubbo-go/issues/2429)
		graceful_shutdown.RegisterProtocol(protocolConf.Name)
	}
	svcOpts.exported.Store(true)
	return nil
}

func (svcOpts *ServiceOptions) generatorInvoker(url *common.URL, info *common.ServiceInfo) base.Invoker {
	proxyFactory := extension.GetProxyFactory(svcOpts.ProxyFactoryKey)
	if info != nil {
		url.SetAttribute(constant.ServiceInfoKey, info)
	}

	url.SetAttribute(constant.RpcServiceKey, svcOpts.rpcService)

	return proxyFactory.GetInvoker(url)
}

// setRegistrySubURL set registry sub url is ivkURl
func setRegistrySubURL(ivkURL *common.URL, regUrl *common.URL) {
	ivkURL.AddParam(constant.RegistryKey, regUrl.GetParam(constant.RegistryKey, ""))
	regUrl.SubURL = ivkURL
}

// loadProtocol filter protocols by ids
func loadProtocol(protocolIds []string, protocols map[string]*global.ProtocolConfig) []*global.ProtocolConfig {
	returnProtocols := make([]*global.ProtocolConfig, 0, len(protocols))
	for _, v := range protocolIds {
		for k, config := range protocols {
			if v == k {
				returnProtocols = append(returnProtocols, config)
			}
		}
	}
	return returnProtocols
}

// Unexport will call unexport of all exporters service config exported
func (svcOpts *ServiceOptions) Unexport() {
	if !svcOpts.exported.Load() {
		return
	}
	if svcOpts.unexported.Load() {
		return
	}

	func() {
		svcOpts.exportersLock.Lock()
		defer svcOpts.exportersLock.Unlock()
		for _, exporter := range svcOpts.exporters {
			exporter.UnExport()
		}
		svcOpts.exporters = nil
	}()

	svcOpts.exported.Store(false)
	svcOpts.unexported.Store(true)
}

// Implement only store the @s and return
func (svcOpts *ServiceOptions) Implement(rpcService common.RPCService) {
	svcOpts.rpcService = rpcService
}

func (svcOpts *ServiceOptions) getUrlMap() url.Values {
	svcConf := svcOpts.Service
	app := svcOpts.applicationCompat
	metrics := svcOpts.srvOpts.Metrics
	tracing := svcOpts.srvOpts.Otel.TracingConfig

	urlMap := url.Values{}
	// first set user params
	for k, v := range svcConf.Params {
		urlMap.Set(k, v)
	}
	urlMap.Set(constant.InterfaceKey, svcConf.Interface)
	urlMap.Set(constant.TimestampKey, strconv.FormatInt(time.Now().Unix(), 10))
	urlMap.Set(constant.ClusterKey, svcConf.Cluster)
	urlMap.Set(constant.LoadbalanceKey, svcConf.Loadbalance)
	urlMap.Set(constant.WarmupKey, svcConf.Warmup)
	urlMap.Set(constant.RetriesKey, svcConf.Retries)
	if svcConf.Group != "" {
		urlMap.Set(constant.GroupKey, svcConf.Group)
	}
	if svcConf.Version != "" {
		urlMap.Set(constant.VersionKey, svcConf.Version)
	}
	urlMap.Set(constant.RegistryRoleKey, strconv.Itoa(common.PROVIDER))
	urlMap.Set(constant.ReleaseKey, "dubbo-golang-"+constant.Version)
	urlMap.Set(constant.SideKey, (common.RoleType(common.PROVIDER)).Role())
	// todo: move
	urlMap.Set(constant.SerializationKey, svcConf.Serialization)
	// application config info
	urlMap.Set(constant.ApplicationKey, app.Name)
	urlMap.Set(constant.OrganizationKey, app.Organization)
	urlMap.Set(constant.NameKey, app.Name)
	urlMap.Set(constant.ModuleKey, app.Module)
	urlMap.Set(constant.AppVersionKey, app.Version)
	urlMap.Set(constant.OwnerKey, app.Owner)
	urlMap.Set(constant.EnvironmentKey, app.Environment)
	//issue #2864  nacos client add weight
	urlMap.Set(constant.WeightKey, strconv.FormatInt(svcOpts.Provider.Weight, 10))

	//filter
	var filters string
	if svcConf.Filter == "" {
		filters = constant.DefaultServiceFilters
	} else {
		filters = svcConf.Filter
	}
	if svcOpts.adaptiveService {
		filters += fmt.Sprintf(",%s", constant.AdaptiveServiceProviderFilterKey)
	}
	if metrics.Enable != nil && *metrics.Enable {
		filters += fmt.Sprintf(",%s", constant.MetricsFilterKey)
	}
	if tracing.Enable != nil && *tracing.Enable {
		filters += fmt.Sprintf(",%s", constant.OTELServerTraceKey)
	}
	urlMap.Set(constant.ServiceFilterKey, filters)

	// filter special config
	urlMap.Set(constant.AccessLogFilterKey, svcConf.AccessLog)
	// tps limiter
	urlMap.Set(constant.TPSLimitStrategyKey, svcConf.TpsLimitStrategy)
	urlMap.Set(constant.TPSLimitIntervalKey, svcConf.TpsLimitInterval)
	urlMap.Set(constant.TPSLimitRateKey, svcConf.TpsLimitRate)
	urlMap.Set(constant.TPSLimiterKey, svcConf.TpsLimiter)
	urlMap.Set(constant.TPSRejectedExecutionHandlerKey, svcConf.TpsLimitRejectedHandler)
	urlMap.Set(constant.TracingConfigKey, svcConf.TracingKey)

	// execute limit filter
	urlMap.Set(constant.ExecuteLimitKey, svcConf.ExecuteLimit)
	urlMap.Set(constant.ExecuteRejectedExecutionHandlerKey, svcConf.ExecuteLimitRejectedHandler)

	// auth filter
	urlMap.Set(constant.ServiceAuthKey, svcConf.Auth)
	urlMap.Set(constant.ParameterSignatureEnableKey, svcConf.ParamSign)

	// whether to needExport or not
	urlMap.Set(constant.ExportKey, strconv.FormatBool(svcOpts.needExport))
	urlMap.Set(constant.PIDKey, fmt.Sprintf("%d", os.Getpid()))

	for _, v := range svcConf.Methods {
		prefix := "methods." + v.Name + "."
		urlMap.Set(prefix+constant.LoadbalanceKey, v.LoadBalance)
		urlMap.Set(prefix+constant.RetriesKey, v.Retries)
		urlMap.Set(prefix+constant.WeightKey, strconv.FormatInt(v.Weight, 10))

		urlMap.Set(prefix+constant.TPSLimitStrategyKey, v.TpsLimitStrategy)
		urlMap.Set(prefix+constant.TPSLimitIntervalKey, v.TpsLimitInterval)
		urlMap.Set(prefix+constant.TPSLimitRateKey, v.TpsLimitRate)

		urlMap.Set(constant.ExecuteLimitKey, v.ExecuteLimit)
		urlMap.Set(constant.ExecuteRejectedExecutionHandlerKey, v.ExecuteLimitRejectedHandler)
	}

	return urlMap
}

// GetExportedUrls will return the url in service config's exporter
func (svcOpts *ServiceOptions) GetExportedUrls() []*common.URL {
	if svcOpts.exported.Load() {
		var urls []*common.URL
		for _, exporter := range svcOpts.exporters {
			urls = append(urls, exporter.GetInvoker().GetURL())
		}
		return urls
	}
	return nil
}

// postProcessConfig asks registered ConfigPostProcessor to post-process the current ServiceConfig.
func (svcOpts *ServiceOptions) postProcessConfig(url *common.URL) {
	for _, p := range extension.GetConfigPostProcessors() {
		p.PostProcessServiceConfig(url)
	}
}

// todo(DMwangnima): think about moving this function to a common place(e.g. /common/config)
func getRegistryIds(registries map[string]*global.RegistryConfig) []string {
	ids := make([]string, 0)
	for key := range registries {
		ids = append(ids, key)
	}
	return removeDuplicateElement(ids)
}

// removeDuplicateElement remove duplicate element
func removeDuplicateElement(items []string) []string {
	result := make([]string, 0, len(items))
	temp := map[string]struct{}{}
	for _, item := range items {
		if _, ok := temp[item]; !ok && item != "" {
			temp[item] = struct{}{}
			result = append(result, item)
		}
	}
	return result
}
