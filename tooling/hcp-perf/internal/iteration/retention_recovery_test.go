package iteration

import (
	"fmt"
	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/retention"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type recoveryTransport func(*http.Request) (*http.Response, error)

func (f recoveryTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestRetentionRecoveryReadOnly(t *testing.T) {
	for _, scenario := range []string{"verified", "changed", "missing", "incomplete", "wrong-id"} {
		t.Run(scenario, func(t *testing.T) {
			token := filepath.Join(t.TempDir(), "token")
			if err := os.WriteFile(token, []byte("test"), 0600); err != nil {
				t.Fatal(err)
			}
			id := "/subscriptions/11111111-1111-1111-1111-111111111111/resourceGroups/personal"
			stamp := "2026-09-15T01:47:14Z"
			spec := retention.NativeSpec{Required: retention.Spec{Version: "v1", ResourceGroups: []string{id}}, TokenFile: token, Interval: time.Minute, Duration: time.Hour}
			result := retention.Result{Status: "verified", CreatedAt: stamp, ResourceGroups: []retention.GroupResult{{ID: id, TagKey: "createdAt", NewCreatedAt: stamp, Verified: true}}}
			if scenario == "incomplete" {
				result.ResourceGroups[0].PossiblyUpdated = true
			}
			transport := recoveryTransport(func(r *http.Request) (*http.Response, error) {
				if r.Method != http.MethodGet {
					t.Fatal("mutation in recovery")
				}
				value := stamp
				actualID := id
				code := 200
				if scenario == "changed" {
					value = "other"
				}
				if scenario == "missing" {
					code = 404
				}
				if scenario == "wrong-id" {
					actualID += "-other"
				}
				return &http.Response{StatusCode: code, Body: io.NopCloser(strings.NewReader(fmt.Sprintf(`{"id":%q,"tags":{"createdAt":%q}}`, actualID, value)))}, nil
			})
			err := verifyRetentionCycle(t.Context(), spec, result, transport)
			if (err == nil) != (scenario == "verified") {
				t.Fatalf("unexpected verification: %v", err)
			}
		})
	}
}
