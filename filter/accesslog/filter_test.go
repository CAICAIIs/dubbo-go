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

package accesslog

import (
	"context"
	"testing"
)

import (
	"github.com/golang/mock/gomock"

	"github.com/stretchr/testify/assert"
)

import (
	"dubbo.apache.org/dubbo-go/v3/common"
	"dubbo.apache.org/dubbo-go/v3/common/constant"
	"dubbo.apache.org/dubbo-go/v3/protocol/base"
	"dubbo.apache.org/dubbo-go/v3/protocol/invocation"
	"dubbo.apache.org/dubbo-go/v3/protocol/result"
)

func TestFilter_Invoke_Not_Config(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	url, _ := common.NewURL(
		"dubbo://:20000/UserProvider?app.version=0.0.1&application=BDTService&bean.name=UserProvider" +
			"&cluster=failover&environment=dev&group=&interface=com.ikurento.user.UserProvider&loadbalance=random&methods.GetUser." +
			"loadbalance=random&methods.GetUser.retries=1&methods.GetUser.weight=0&module=dubbogo+user-info+server&name=" +
			"BDTService&organization=ikurento.com&owner=ZX&registry.role=3&retries=&" +
			"service.filter=echo%2Ctoken%2Caccesslog&timestamp=1569153406&token=934804bf-b007-4174-94eb-96e3e1d60cc7&version=&warmup=100")
	invoker := base.NewBaseInvoker(url)

	attach := make(map[string]any, 10)
	inv := invocation.NewRPCInvocation("MethodName", []any{"OK", "Hello"}, attach)

	accessLogFilter := &Filter{}
	result := accessLogFilter.Invoke(context.Background(), invoker, inv)
	assert.Nil(t, result.Error())
}

func TestFilterInvokeDefaultConfig(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	url, _ := common.NewURL(
		"dubbo://:20000/UserProvider?app.version=0.0.1&application=BDTService&bean.name=UserProvider" +
			"&cluster=failover&accesslog=true&environment=dev&group=&interface=com.ikurento.user.UserProvider&loadbalance=random&methods.GetUser." +
			"loadbalance=random&methods.GetUser.retries=1&methods.GetUser.weight=0&module=dubbogo+user-info+server&name=" +
			"BDTService&organization=ikurento.com&owner=ZX&registry.role=3&retries=&" +
			"service.filter=echo%2Ctoken%2Caccesslog&timestamp=1569153406&token=934804bf-b007-4174-94eb-96e3e1d60cc7&version=&warmup=100")
	invoker := base.NewBaseInvoker(url)

	attach := make(map[string]any, 10)
	attach[constant.VersionKey] = "1.0"
	attach[constant.GroupKey] = "MyGroup"
	inv := invocation.NewRPCInvocation("MethodName", []any{"OK", "Hello"}, attach)

	accessLogFilter := &Filter{}
	result := accessLogFilter.Invoke(context.Background(), invoker, inv)
	assert.Nil(t, result.Error())
}

func TestFilterOnResponse(t *testing.T) {
	result := &result.RPCResult{}
	accessLogFilter := &Filter{}
	response := accessLogFilter.OnResponse(context.TODO(), result, nil, nil)
	assert.Equal(t, result, response)
}
