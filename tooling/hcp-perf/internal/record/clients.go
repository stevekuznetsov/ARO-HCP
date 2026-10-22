// Copyright 2026 Microsoft Corporation
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package record

import (
	"errors"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/flowcontrol"
)

// NewClients creates independently throttled observation and log clients without
// mutating config. All typed clients within each clientset share one limiter.
// Per worker the default combined budget is 30 QPS / 60 burst, not per follower.
// Request contexts bound calls; HTTP timeouts must not cut off watch/log bodies.
func NewClients(config *rest.Config, spec Spec) (observation, logs kubernetes.Interface, err error) {
	if err := spec.Validate(); err != nil {
		return nil, nil, err
	}
	if config == nil {
		return nil, nil, errors.New("REST config is required")
	}
	spec = spec.withDefaults()
	observationConfig := rest.CopyConfig(config)
	observationConfig.Timeout = 0
	observationConfig.QPS, observationConfig.Burst = 10, 20
	observationConfig.RateLimiter = flowcontrol.NewTokenBucketRateLimiter(observationConfig.QPS, observationConfig.Burst)
	logConfig := rest.CopyConfig(observationConfig)
	logConfig.QPS, logConfig.Burst = float32(spec.LogRequestQPS), int(spec.LogRequestBurst)
	logConfig.RateLimiter = flowcontrol.NewTokenBucketRateLimiter(logConfig.QPS, logConfig.Burst)
	observation, err = kubernetes.NewForConfig(observationConfig)
	if err != nil {
		return nil, nil, err
	}
	logs, err = kubernetes.NewForConfig(logConfig)
	return observation, logs, err
}
