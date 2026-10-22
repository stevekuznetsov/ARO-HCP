// Copyright 2026 Microsoft Corporation
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at http://www.apache.org/licenses/LICENSE-2.0
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package dashboard prepares portable, offline performance dashboards.
package dashboard

const SchemaVersion = "hcp-perf.dashboard/v1"

const MaxInputBytes = 32 << 20

// Dataset contains reviewed summaries, never credentials or raw logs.
type Dataset struct {
	SchemaVersion string   `json:"schemaVersion"`
	Title         string   `json:"title"`
	GeneratedAt   string   `json:"generatedAt"`
	Method        Method   `json:"method"`
	Sizes         []Size   `json:"sizes"`
	Sources       []Source `json:"sources"`
}

type Method struct {
	Quantiles string `json:"quantiles"`
	Whiskers  string `json:"whiskers"`
	CPU       string `json:"cpu"`
	Memory    string `json:"memory"`
	Scope     string `json:"scope"`
}

// Source is an inert citation, not a URL to fetch in the browser.
type Source struct {
	ID     string `json:"id"`
	Label  string `json:"label"`
	SHA256 string `json:"sha256"`
}

type Size struct {
	ID            string      `json:"id"`
	Name          string      `json:"name"`
	Revision      string      `json:"revision"`
	Status        string      `json:"status"`
	Description   string      `json:"description"`
	Budget        Budget      `json:"budget"`
	DefaultRun    string      `json:"defaultRun"`
	Configuration []Fact      `json:"configuration"`
	Limitations   []string    `json:"limitations"`
	Components    []Component `json:"components"`
	Runs          []Run       `json:"runs"`
}

type Budget struct {
	FeeUSD             float64 `json:"feeUSD"`
	TargetUSD          float64 `json:"targetUSD"`
	ModeledUSD         float64 `json:"modeledUSD"`
	TargetCPUCores     float64 `json:"targetCPUCores"`
	TargetMemoryGiB    float64 `json:"targetMemoryGiB"`
	ChargedCPUCores    float64 `json:"chargedCPUCores"`
	ChargedMemoryGiB   float64 `json:"chargedMemoryGiB"`
	RequestedCPUCores  float64 `json:"requestedCPUCores"`
	RequestedMemoryGiB float64 `json:"requestedMemoryGiB"`
	PricingSKU         string  `json:"pricingSKU"`
	PricingRegion      string  `json:"pricingRegion"`
	PriceDate          string  `json:"priceDate"`
	ExecutionSKU       string  `json:"executionSKU"`
	Scope              string  `json:"scope"`
}

type Fact struct {
	Label string `json:"label"`
	Value string `json:"value"`
}

type Component struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Scope string `json:"scope"`
}

// Run is one case, not a pool of repetitions or different assessment policies.
type Run struct {
	ID                    string               `json:"id"`
	Label                 string               `json:"label"`
	Cohort                string               `json:"cohort"`
	Kind                  string               `json:"kind"`       // screen or confirmation
	Population            string               `json:"population"` // loaded or unloaded
	ApplicationPods       int                  `json:"applicationPods"`
	Applications          int                  `json:"applications"`
	GuestNodes            int                  `json:"guestNodes"`
	OfferedQPS            float64              `json:"offeredQPS"`
	Start                 string               `json:"start"`
	End                   string               `json:"end"`
	PlannedSeconds        float64              `json:"plannedSeconds"`
	ActualSeconds         float64              `json:"actualSeconds"`
	Contended             bool                 `json:"contended"`
	StabilityPolicy       string               `json:"stabilityPolicy"`
	TurnoverPeriodSeconds int                  `json:"turnoverPeriodSeconds"`
	Requests              Requests             `json:"requests"`
	Assessment            Assessment           `json:"assessment"`
	RolloutsCompleted     int                  `json:"rolloutsCompleted"`
	RolloutsScheduled     int                  `json:"rolloutsScheduled"`
	CanaryCycles          int                  `json:"canaryCycles"`
	CanarySoftDegraded    int                  `json:"canarySoftDegraded"`
	CanaryHardFailed      *int                 `json:"canaryHardFailed"` // nil when measured-cycle evidence is unknown
	Restarts              *int                 `json:"restarts"`
	OOMs                  *int                 `json:"ooms"`
	Latencies             []MetricDistribution `json:"latencies"`
	Resources             []ComponentUsage     `json:"resources"`
	WholeHCP              []MetricDistribution `json:"wholeHCP"`
	Notes                 []string             `json:"notes"`
	Sources               []string             `json:"sources"`
}

type Requests struct {
	Offered             int      `json:"offered"`
	Dispatched          int      `json:"dispatched"`
	Successful          int      `json:"successful"`
	Rejected429         int      `json:"rejected429"`
	Timeouts            int      `json:"timeouts"`
	DegradedUnion       int      `json:"degradedUnion"`
	Canceled            int      `json:"canceled"`
	OtherErrors         int      `json:"otherErrors"`
	GeneratorLimited    int      `json:"generatorLimited"`
	SuccessfulQPS       float64  `json:"successfulQPS"`
	ThroughputBasis     string   `json:"throughputBasis"`
	MaxDegradedFraction *float64 `json:"maxDegradedFraction"` // nil when no union-fraction policy was declared
}

type Assessment struct {
	Execution      string `json:"execution"`
	RequestQuality string `json:"requestQuality"`
	Health         string `json:"health"`
	Capture        string `json:"capture"`
	Summary        string `json:"summary"`
}

type MetricDistribution struct {
	ID           string        `json:"id"`
	Label        string        `json:"label"`
	Unit         string        `json:"unit"`
	Distribution *Distribution `json:"distribution"`
	Censored     int           `json:"censored"`
	Excluded     int           `json:"excluded"`
	Note         string        `json:"note"`
}

// Distribution uses the entire covered cohort; plots may show a bounded sample.
// Expected and Missing describe observation opportunities, not independent scrapes.
type Distribution struct {
	Count        int     `json:"count"`
	Expected     int     `json:"expected"`
	Missing      int     `json:"missing"`
	Min          float64 `json:"min"`
	Q1           float64 `json:"q1"`
	Median       float64 `json:"median"`
	Q3           float64 `json:"q3"`
	P95          float64 `json:"p95"`
	P99          float64 `json:"p99"`
	Max          float64 `json:"max"`
	WhiskerLow   float64 `json:"whiskerLow"`
	WhiskerHigh  float64 `json:"whiskerHigh"`
	OutlierCount int     `json:"outlierCount"`
}

type ComponentUsage struct {
	ComponentID   string        `json:"componentID"`
	Replicas      int           `json:"replicas"`
	Containers    int           `json:"containers"`
	CPU           *Distribution `json:"cpu"`    // cores, component totals
	Memory        *Distribution `json:"memory"` // MiB cgroup charge
	WorkingSet    *Distribution `json:"workingSet"`
	RSS           *Distribution `json:"rss"`
	CPURequest    *float64      `json:"cpuRequest"`
	CPULimit      *float64      `json:"cpuLimit"`
	MemoryRequest *float64      `json:"memoryRequest"`
	MemoryLimit   *float64      `json:"memoryLimit"`
	GoTarget      *float64      `json:"goTarget"`
	Notes         []string      `json:"notes"`
}
