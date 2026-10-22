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

package scrape

import (
	"fmt"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"
)

// EndpointCredentials replaces only one endpoint's client authentication, never
// its server trust or discovery identity. The caller supplies a guest-issued
// token where needed; Generate never mints tokens or reads credential contents.
type EndpointCredentials struct {
	MonitorKind       string        `json:"monitorKind"`
	Namespace         string        `json:"namespace"`
	Name              string        `json:"name"`
	EndpointIndex     int           `json:"endpointIndex"`
	BearerTokenSecret FileReference `json:"bearerTokenSecret"`
}

func (e EndpointCredentials) job() string {
	return fmt.Sprintf("%s/%s/%s/%d", e.MonitorKind, e.Namespace, e.Name, e.EndpointIndex)
}

func (e EndpointCredentials) validate() error {
	if (e.MonitorKind != "ServiceMonitor" && e.MonitorKind != "PodMonitor") || len(validation.IsDNS1123Label(e.Namespace)) != 0 || len(validation.IsDNS1123Subdomain(e.Name)) != 0 || e.EndpointIndex < 0 {
		return fmt.Errorf("endpointCredentials requires an exact monitor kind, namespace, name and nonnegative endpointIndex")
	}
	if e.BearerTokenSecret.Kind != "Secret" {
		return fmt.Errorf("endpointCredentials bearerTokenSecret must reference a Secret")
	}
	return e.BearerTokenSecret.validate()
}

type authorizationSpec struct {
	Type            string          `json:"type"`
	Credentials     *tlsKeySelector `json:"credentials"`
	CredentialsFile string          `json:"credentialsFile"`
}

func convertCredentials(e endpointSpec, spec Spec, namespace, job string) (map[string]any, []FileMountRequirement, bool, error) {
	out := map[string]any{}
	selector := e.BearerTokenSecret
	// Operator APIs serialize an absent legacy selector as {name:"",key:""}.
	if selector != nil && selector.Name == "" && selector.Key == "" && selector.Optional == nil {
		selector = nil
	}
	count := 0
	for _, present := range []bool{selector != nil, e.BearerTokenFile != "", e.Authorization != nil} {
		if present {
			count++
		}
	}
	if count > 1 {
		return nil, nil, false, fmt.Errorf("conflicting bearerTokenFile, bearerTokenSecret or authorization")
	}
	file, authType := e.BearerTokenFile, ""
	if e.Authorization != nil {
		authType = e.Authorization.Type
		if authType == "" {
			authType = "Bearer"
		}
		if strings.EqualFold(authType, "Basic") || strings.ContainsAny(authType, " \t\r\n") {
			return nil, nil, false, fmt.Errorf("authorization requires a non-Basic HTTP authentication scheme")
		}
		selector, file = e.Authorization.Credentials, e.Authorization.CredentialsFile
		if (selector == nil) == (file == "") {
			return nil, nil, false, fmt.Errorf("authorization requires exactly one credentials selector or credentialsFile")
		}
	}
	var reference *FileReference
	if selector != nil {
		ref := FileReference{Kind: "Secret", Namespace: namespace, Name: selector.Name, Key: selector.Key}
		if err := ref.validate(); err != nil {
			return nil, nil, false, err
		}
		if selector.Optional != nil && *selector.Optional {
			return nil, nil, false, fmt.Errorf("optional credential keys are unsupported")
		}
		reference = &ref
	}
	overridden := false
	for _, override := range spec.EndpointCredentials {
		if override.job() == job {
			reference = &override.BearerTokenSecret
			file, authType, overridden = "", "Bearer", true
		}
	}
	var mounts []FileMountRequirement
	if reference != nil {
		if len(spec.TLSMounts) > 0 && !slices.Contains(spec.TLSMounts, *reference) {
			return nil, nil, false, fmt.Errorf("credential reference is not allowed by tlsMounts")
		}
		mount := fileMount(*reference, spec.PrometheusNamespace, job)
		file, mounts = mount.File, []FileMountRequirement{mount}
	}
	if file != "" {
		if e.Scheme != "https" {
			return nil, nil, false, fmt.Errorf("credentials require HTTPS")
		}
		if e.FollowRedirects != nil && *e.FollowRedirects {
			return nil, nil, false, fmt.Errorf("credential-bearing endpoints must not follow redirects")
		}
		out["follow_redirects"] = false
		if authType != "" {
			out["authorization"] = map[string]any{"type": authType, "credentials_file": file}
		} else {
			out["bearer_token_file"] = file
		}
	}
	return out, mounts, overridden, nil
}
