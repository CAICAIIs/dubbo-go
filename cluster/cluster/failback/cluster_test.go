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

package failback

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

import (
	"github.com/golang/mock/gomock"

	perrors "github.com/pkg/errors"

	"github.com/stretchr/testify/assert"
)

import (
	clusterpkg "dubbo.apache.org/dubbo-go/v3/cluster/cluster"
	"dubbo.apache.org/dubbo-go/v3/cluster/directory/static"
	"dubbo.apache.org/dubbo-go/v3/cluster/loadbalance/random"
	"dubbo.apache.org/dubbo-go/v3/common"
	"dubbo.apache.org/dubbo-go/v3/common/constant"
	"dubbo.apache.org/dubbo-go/v3/common/extension"
	"dubbo.apache.org/dubbo-go/v3/protocol/base"
	"dubbo.apache.org/dubbo-go/v3/protocol/invocation"
	"dubbo.apache.org/dubbo-go/v3/protocol/mock"
	"dubbo.apache.org/dubbo-go/v3/protocol/result"
)

var failbackUrl, _ = common.NewURL(
	fmt.Sprintf("dubbo://%s:%d/com.ikurento.user.UserProvider", constant.LocalHostValue, constant.DefaultPort))

// registerFailback register failbackCluster to failbackCluster extension.
func registerFailback(invoker *mock.MockInvoker) base.Invoker {
	extension.SetLoadbalance("random", random.NewRandomLoadBalance)
	failbackCluster := newFailbackCluster()

	var invokers []base.Invoker
	invokers = append(invokers, invoker)

	invoker.EXPECT().GetURL().Return(failbackUrl).AnyTimes()

	staticDir := static.NewDirectory(invokers)
	clusterInvoker := failbackCluster.Join(staticDir)
	return clusterInvoker
}

// success firstly, failback should return origin invoke result.
func TestFailbackSuceess(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	invoker := mock.NewMockInvoker(ctrl)
	clusterInvoker := registerFailback(invoker).(*failbackClusterInvoker)

	invoker.EXPECT().GetURL().Return(failbackUrl).AnyTimes()

	invoker.EXPECT().IsAvailable().Return(true).AnyTimes()

	mockResult := &result.RPCResult{Rest: clusterpkg.Rest{Tried: 0, Success: true}}
	invoker.EXPECT().Invoke(gomock.Any(), gomock.Any()).Return(mockResult).AnyTimes()

	result := clusterInvoker.Invoke(context.Background(), &invocation.RPCInvocation{})
	assert.Equal(t, mockResult, result)
}

// failed firstly, success later after one retry.
func TestFailbackRetryOneSuccess(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	invoker := mock.NewMockInvoker(ctrl)
	clusterInvoker := registerFailback(invoker).(*failbackClusterInvoker)

	invoker.EXPECT().GetURL().Return(failbackUrl).AnyTimes()
	invoker.EXPECT().IsAvailable().Return(true)

	// failed at first
	mockFailedResult := &result.RPCResult{Err: perrors.New("error")}
	invoker.EXPECT().Invoke(gomock.Any(), gomock.Any()).Return(mockFailedResult)

	// success second
	var wg sync.WaitGroup
	wg.Add(1)
	now := time.Now()
	mockSuccResult := &result.RPCResult{Rest: clusterpkg.Rest{Tried: 0, Success: true}}
	invoker.EXPECT().IsAvailable().Return(true)
	invoker.EXPECT().Invoke(gomock.Any(), gomock.Any()).DoAndReturn(func(context.Context, base.Invocation) result.Result {
		delta := time.Since(now).Nanoseconds() / int64(time.Second)
		assert.True(t, delta >= 5)
		wg.Done()
		return mockSuccResult
	})

	result := clusterInvoker.Invoke(context.Background(), &invocation.RPCInvocation{})
	assert.Nil(t, result.Error())
	assert.Nil(t, result.Result())
	assert.Equal(t, 0, len(result.Attachments()))

	// ensure the retry task has been executed
	assert.Equal(t, int64(1), clusterInvoker.taskList.Len())
	// wait until the retry task is executed, the taskList will be empty.
	wg.Wait()
	assert.Equal(t, int64(0), clusterInvoker.taskList.Len())

	invoker.EXPECT().Destroy().Return().AnyTimes()
	clusterInvoker.Destroy()

	assert.Equal(t, int64(0), clusterInvoker.taskList.Len())
}

// failed firstly, and failed again after ech retry time.
func TestFailbackRetryFailed(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	invoker := mock.NewMockInvoker(ctrl)
	clusterInvoker := registerFailback(invoker).(*failbackClusterInvoker)

	invoker.EXPECT().GetURL().Return(failbackUrl).AnyTimes()
	invoker.EXPECT().IsAvailable().Return(true).AnyTimes()

	mockFailedResult := &result.RPCResult{Err: perrors.New("error")}
	invoker.EXPECT().Invoke(gomock.Any(), gomock.Any()).Return(mockFailedResult)

	//
	var wg sync.WaitGroup
	retries := 2
	wg.Add(retries)
	now := time.Now()

	// add retry call that eventually failed.
	for i := 0; i < retries; i++ {
		j := i + 1
		invoker.EXPECT().Invoke(gomock.Any(), gomock.Any()).DoAndReturn(func(context.Context, base.Invocation) result.Result {
			delta := time.Since(now).Nanoseconds() / int64(time.Second)
			assert.True(t, delta >= int64(5*j))
			wg.Done()
			return mockFailedResult
		})
	}

	// first call should failed.
	result := clusterInvoker.Invoke(context.Background(), &invocation.RPCInvocation{})
	assert.Nil(t, result.Error())
	assert.Nil(t, result.Result())
	assert.Equal(t, 0, len(result.Attachments()))

	wg.Wait()
	time.Sleep(time.Second)
	assert.Equal(t, int64(1), clusterInvoker.taskList.Len())

	invoker.EXPECT().Destroy().Return()
	clusterInvoker.Destroy()
	// after destroy, the taskList will be empty
	assert.Equal(t, int64(0), clusterInvoker.taskList.Len())
}

// add 10 tasks but all failed firstly, and failed again with one retry.
func TestFailbackRetryFailed10Times(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	invoker := mock.NewMockInvoker(ctrl)
	clusterInvoker := registerFailback(invoker).(*failbackClusterInvoker)
	clusterInvoker.maxRetries = 10

	invoker.EXPECT().IsAvailable().Return(true).AnyTimes()
	invoker.EXPECT().GetURL().Return(failbackUrl).AnyTimes()

	// 10 task should failed firstly.
	mockFailedResult := &result.RPCResult{Err: perrors.New("error")}
	invoker.EXPECT().Invoke(gomock.Any(), gomock.Any()).Return(mockFailedResult).Times(10)

	// 10 task should retry and failed.
	var wg sync.WaitGroup
	wg.Add(10)
	now := time.Now()
	invoker.EXPECT().Invoke(gomock.Any(), gomock.Any()).DoAndReturn(func(context.Context, base.Invocation) result.Result {
		delta := time.Since(now).Nanoseconds() / int64(time.Second)
		assert.True(t, delta >= 5)
		wg.Done()
		return mockFailedResult
	}).Times(10)

	for i := 0; i < 10; i++ {
		result := clusterInvoker.Invoke(context.Background(), &invocation.RPCInvocation{})
		assert.Nil(t, result.Error())
		assert.Nil(t, result.Result())
		assert.Equal(t, 0, len(result.Attachments()))
	}

	wg.Wait()
	time.Sleep(time.Second) // in order to ensure checkRetry have done
	assert.Equal(t, int64(10), clusterInvoker.taskList.Len())

	invoker.EXPECT().Destroy().Return()
	clusterInvoker.Destroy()

	assert.Equal(t, int64(0), clusterInvoker.taskList.Len())
}

func TestFailbackOutOfLimit(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	invoker := mock.NewMockInvoker(ctrl)
	clusterInvoker := registerFailback(invoker).(*failbackClusterInvoker)
	clusterInvoker.failbackTasks = 1

	invoker.EXPECT().GetURL().Return(failbackUrl).AnyTimes()
	invoker.EXPECT().IsAvailable().Return(true).AnyTimes()

	mockFailedResult := &result.RPCResult{Err: perrors.New("error")}
	invoker.EXPECT().Invoke(gomock.Any(), gomock.Any()).Return(mockFailedResult).Times(11)

	// reached limit
	result := clusterInvoker.Invoke(context.Background(), &invocation.RPCInvocation{})
	assert.Nil(t, result.Error())
	assert.Nil(t, result.Result())
	assert.Equal(t, 0, len(result.Attachments()))

	// all will be out of limit
	for i := 0; i < 10; i++ {
		result := clusterInvoker.Invoke(context.Background(), &invocation.RPCInvocation{})
		assert.Nil(t, result.Error())
		assert.Nil(t, result.Result())
		assert.Equal(t, 0, len(result.Attachments()))

		assert.Equal(t, int64(1), clusterInvoker.taskList.Len())
	}

	invoker.EXPECT().Destroy().Return()
	clusterInvoker.Destroy()
}
