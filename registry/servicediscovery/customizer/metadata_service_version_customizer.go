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

package customizer

import (
	"encoding/json"
)

import (
	"github.com/dubbogo/gost/log/logger"
)

import (
	"dubbo.apache.org/dubbo-go/v3/common/constant"
	"dubbo.apache.org/dubbo-go/v3/common/extension"
	"dubbo.apache.org/dubbo-go/v3/registry"
)

func init() {
	extension.AddCustomizers(&MetadtaServiceVersionCustomizer{})
}

// MetadtaServiceVersionCustomizer will try to add meta-v key to instance metadata
type MetadtaServiceVersionCustomizer struct {
}

// GetPriority will return 0, which means it will be invoked at the beginning
func (p *MetadtaServiceVersionCustomizer) GetPriority() int {
	return 0
}

// Customize put the the string like [{"protocol": "dubbo", "port": 123}] into instance's metadata
func (p *MetadtaServiceVersionCustomizer) Customize(instance registry.ServiceInstance) {
	if instance.GetMetadata()[constant.MetadataStorageTypePropertyName] != constant.DefaultMetadataStorageType {
		return
	}
	// only run when storage-type == local
	metadata := instance.GetMetadata()[constant.MetadataServiceURLParamsPropertyName]
	params := make(map[string]string)
	err := json.Unmarshal([]byte(metadata), &params)
	if err != nil {
		logger.Errorf("json unmarshal error %v", err)
		return
	}
	if (params[constant.ProtocolKey]) == constant.TriProtocol {
		// triple support v1 and v2, and v2 is preferred to use
		instance.GetMetadata()[constant.MetadataVersion] = constant.MetadataServiceV2Version
	} else {
		// dubbo support only v1
		instance.GetMetadata()[constant.MetadataVersion] = constant.MetadataServiceV1Version
	}
}
