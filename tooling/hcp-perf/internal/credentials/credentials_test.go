// Copyright 2026 Microsoft Corporation
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at http://www.apache.org/licenses/LICENSE-2.0
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package credentials

import (
	"errors"
	"strings"
	"testing"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	clienttesting "k8s.io/client-go/testing"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

func TestMintCredentialsNeverCopiesAdminAuthority(t *testing.T) {
	client := fake.NewClientset()
	client.PrependReactor("create", "serviceaccounts", func(action clienttesting.Action) (bool, runtime.Object, error) {
		if action.GetSubresource() != "token" {
			t.Fatal("unexpected mutation")
		}
		return true, &authenticationv1.TokenRequest{Status: authenticationv1.TokenRequestStatus{Token: "scoped-only", ExpirationTimestamp: metav1.NewTime(time.Now().Add(time.Hour))}}, nil
	})
	admin := &rest.Config{Host: "https://guest", Username: "admin", Password: "secret", BearerToken: "admin-token", BearerTokenFile: "/admin/token", ExecProvider: &clientcmdapi.ExecConfig{Command: "admin-plugin"}, TLSClientConfig: rest.TLSClientConfig{CAData: []byte("ca"), CertData: []byte("cert"), KeyData: []byte("key")}}
	config, expires, err := MintCredentials(t.Context(), client, admin, "scope", "benchmark", time.Hour)
	if err != nil || !expires.After(time.Now()) {
		t.Fatal(err)
	}
	if config.BearerToken != "scoped-only" || config.Username != "" || config.Password != "" || config.BearerTokenFile != "" || config.ExecProvider != nil || len(config.CertData) != 0 || len(config.KeyData) != 0 || config.Timeout != 0 {
		t.Fatal("admin authority copied into measured config")
	}
	config.CAData[0] = 'x'
	if string(admin.CAData) != "ca" {
		t.Fatal("CA aliases admin config")
	}
	client.PrependReactor("create", "serviceaccounts", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("denied private-server-token")
	})
	config, _, err = MintCredentials(t.Context(), client, admin, "scope", "benchmark", time.Hour)
	if err == nil || config != nil || strings.Contains(err.Error(), "private-server-token") {
		t.Fatal("denial leaked secret or fell back to admin")
	}
}
