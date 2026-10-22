// Copyright 2026 Microsoft Corporation
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at http://www.apache.org/licenses/LICENSE-2.0
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package iteration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/spf13/pflag"

	controller "github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/iteration"
)

type CommandMode uint8

const (
	Submit CommandMode = iota + 1
	Status
	List
	Watch
	Stop
	Reconcile
	Serve
	Worker
	RetryObservation
	ReconcileOperation
)

const maxInputBytes = 16 << 20

var runIDPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`)
var attemptPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

type RawOptions struct {
	Timeout       time.Duration
	ImageBindings string
	Action        string
	Reason        string
	Mode          CommandMode
	Root          string
	Input         string
	ID            string
	Attempt       string
	Interval      time.Duration
	Out           io.Writer
}

type ValidatedOptions struct {
	raw RawOptions
}

type Options struct {
	timeout       time.Duration
	imageBindings []controller.ObservationImageBinding
	action        string
	reason        string
	mode          CommandMode
	root          string
	id            string
	attempt       string
	interval      time.Duration
	out           io.Writer
	spec          controller.Spec
}

func NewRawOptions(mode CommandMode) *RawOptions {
	return &RawOptions{Mode: mode, Interval: time.Second, Timeout: 5 * time.Minute, Out: io.Discard}
}

func (o *RawOptions) BindOptions(flags *pflag.FlagSet) {
	flags.StringVar(&o.Root, "root", o.Root, "Private persistent state directory (default ~/.local/state/hcp-perf/go-iterations)")
	switch o.Mode {
	case Submit:
		flags.StringVar(&o.Input, "input", o.Input, "Reviewed iteration spec JSON (required)")
		flags.StringVar(&o.ID, "id", o.ID, "New run ID (required)")
	case Status, Reconcile:
		flags.StringVar(&o.ID, "id", o.ID, "Run ID; omit for all runs")
	case Watch, Stop, RetryObservation, ReconcileOperation:
		flags.StringVar(&o.ID, "id", o.ID, "Run ID (required)")
	case Worker:
		flags.StringVar(&o.Attempt, "attempt", o.Attempt, "Controller-issued attempt token (required)")
	}
	if o.Mode == RetryObservation {
		flags.StringVar(&o.ImageBindings, "image-bindings", "", "Reviewed image-only expectation corrections JSON")
		flags.StringVar(&o.Action, "action", o.Action, "Startup observer action ID (required)")
		flags.StringVar(&o.Reason, "reason", o.Reason, "Audited reason for replacing the observer binary (required)")
	}
	if o.Mode == ReconcileOperation {
		flags.StringVar(&o.Action, "action", o.Action, "RP create/delete action with an accepted operation (required)")
		flags.DurationVar(&o.Timeout, "timeout", o.Timeout, "Bound for this observation-only attempt (maximum 30m)")
	}
	if o.Mode == Watch || o.Mode == Serve {
		flags.DurationVar(&o.Interval, "interval", o.Interval, "Polling interval")
	}
}

// Validate checks flag values only, without filesystem access or controller setup.
func (o *RawOptions) Validate(ctx context.Context) (*ValidatedOptions, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if o.Mode < Submit || o.Mode > ReconcileOperation {
		return nil, errors.New("invalid iteration command mode")
	}
	if o.Out == nil {
		return nil, errors.New("output writer is required")
	}
	if o.Mode == Submit && strings.TrimSpace(o.Input) == "" {
		return nil, errors.New("--input is required")
	}
	if o.Input != "" && o.Mode != Submit {
		return nil, errors.New("--input is only supported by submit")
	}
	if o.Mode == Submit || o.Mode == Watch || o.Mode == Stop || o.Mode == RetryObservation || o.Mode == ReconcileOperation {
		if o.ID == "" {
			return nil, errors.New("--id is required")
		}
	}
	if o.Mode == RetryObservation {
		if !runIDPattern.MatchString(o.Action) || strings.TrimSpace(o.Reason) == "" || len(o.Reason) > 1024 {
			return nil, errors.New("retry-observation requires --action and --reason (1-1024 bytes)")
		}
	} else if o.Mode == ReconcileOperation {
		if !runIDPattern.MatchString(o.Action) || o.Timeout <= 0 || o.Timeout > 30*time.Minute || o.Reason != "" || o.ImageBindings != "" {
			return nil, errors.New("reconcile-operation requires --action and --timeout in (0,30m]")
		}
	} else if o.Action != "" || o.Reason != "" || o.ImageBindings != "" {
		return nil, errors.New("--action and --reason require retry-observation")
	}
	if o.ID != "" {
		if o.Mode == List || o.Mode == Serve || o.Mode == Worker {
			return nil, errors.New("--id is not supported by this command")
		}
		if !runIDPattern.MatchString(o.ID) {
			return nil, errors.New("--id must be a safe path segment of 1-64 letters, digits, underscores or hyphens, starting with a letter or digit")
		}
	}
	if o.Mode == Worker {
		if strings.TrimSpace(o.Root) == "" || !attemptPattern.MatchString(o.Attempt) {
			return nil, errors.New("worker requires --root and a 32-character lowercase hexadecimal --attempt")
		}
	} else if o.Attempt != "" {
		return nil, errors.New("--attempt is only supported by worker")
	}
	if (o.Mode == Watch || o.Mode == Serve) && o.Interval <= 0 {
		return nil, errors.New("--interval must be positive")
	}
	if o.Root != "" && strings.TrimSpace(o.Root) == "" {
		return nil, errors.New("--root must not be whitespace")
	}
	return &ValidatedOptions{raw: *o}, nil
}

// Complete reads and resolves inputs, but does not create state or contact services.
// Opening the controller is deferred to Run so malformed input leaves no root.
func (v *ValidatedOptions) Complete(ctx context.Context) (*Options, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	raw := v.raw
	root := raw.Root
	if root == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("resolve default state directory: %w", err)
		}
		root = filepath.Join(home, ".local", "state", "hcp-perf", "go-iterations")
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve state directory: %w", err)
	}
	o := &Options{mode: raw.Mode, root: root, id: raw.ID, attempt: raw.Attempt, interval: raw.Interval, out: raw.Out}
	o.action, o.reason = raw.Action, raw.Reason
	o.timeout = raw.Timeout
	if raw.ImageBindings != "" {
		data, err := os.ReadFile(raw.ImageBindings)
		if err != nil {
			return nil, err
		}
		if len(data) > maxInputBytes {
			return nil, errors.New("image bindings too large")
		}
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&o.imageBindings); err != nil {
			return nil, err
		}
		if err := decoder.Decode(new(any)); err != io.EOF {
			return nil, errors.New("trailing image bindings data")
		}
		if len(o.imageBindings) == 0 {
			return nil, errors.New("empty image bindings")
		}
	}
	if raw.Mode != Submit {
		return o, nil
	}
	input, err := filepath.Abs(raw.Input)
	if err != nil {
		return nil, fmt.Errorf("resolve input path: %w", err)
	}
	file, err := os.Open(input)
	if err != nil {
		return nil, fmt.Errorf("open input: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("inspect input: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("iteration input must be a regular JSON file")
	}
	data, err := io.ReadAll(io.LimitReader(file, maxInputBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read input: %w", err)
	}
	if len(data) > maxInputBytes {
		return nil, errors.New("iteration input exceeds 16 MiB")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&o.spec); err != nil {
		return nil, fmt.Errorf("decode iteration input: %w", err)
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return nil, errors.New("iteration input must contain exactly one JSON value")
	}
	// Evidence paths are provenance, not assets to copy. No other current action
	// parameter is a local path; RP URLs and schema-free payloads stay untouched.
	for i := range o.spec.Actions {
		action := &o.spec.Actions[i]
		if action.Cost == nil {
			continue
		}
		for j := range action.Cost.DataPoints {
			for k := range action.Cost.DataPoints[j].Evidence {
				evidence := &action.Cost.DataPoints[j].Evidence[k]
				if evidence.Path != "" && !filepath.IsAbs(evidence.Path) {
					evidence.Path = filepath.Join(filepath.Dir(input), evidence.Path)
				}
			}
		}
	}
	if err := o.spec.Validate(); err != nil {
		return nil, fmt.Errorf("validate iteration input: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return o, nil
}

func (o *Options) Run(ctx context.Context) (err error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	c, err := controller.Open(o.root)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, c.Close()) }()
	encoder := json.NewEncoder(o.out)
	switch o.mode {
	case ReconcileOperation:
		if err := c.ReconcileOperation(ctx, o.id, o.action, o.timeout); err != nil {
			return err
		}
	case RetryObservation:
		if err := c.RetryObservationImages(ctx, o.id, o.action, o.reason, o.imageBindings); err != nil {
			return err
		}
	case Submit:
		run, err := c.Submit(ctx, o.id, o.spec)
		if err != nil {
			return err
		}
		return encoder.Encode(run)
	case Stop:
		if err := c.Stop(ctx, o.id); err != nil {
			return err
		}
	case Reconcile:
		if o.id != "" {
			if err := c.Reconcile(ctx, o.id); err != nil {
				return err
			}
		} else {
			runs, err := c.List(ctx)
			if err != nil {
				return err
			}
			for _, run := range runs {
				if err := c.Reconcile(ctx, run.ID); err != nil {
					return err
				}
			}
		}
	case Serve:
		return c.Serve(ctx, o.interval)
	case Worker:
		return c.Work(ctx, o.attempt)
	case Watch:
		ticker := time.NewTicker(o.interval)
		defer ticker.Stop()
		for {
			run, err := c.Status(ctx, o.id)
			if err != nil {
				return err
			}
			if err := encoder.Encode(run); err != nil {
				return err
			}
			switch run.State {
			case controller.Succeeded:
				return nil
			case controller.Failed, controller.Stopped, controller.Blocked:
				return fmt.Errorf("iteration %q is %s", run.ID, run.State)
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-ticker.C:
			}
		}
	case Status, List:
		// These commands only read journal state, without reconciling workers.
	default:
		return errors.New("invalid iteration command mode")
	}
	if o.id == "" {
		runs, err := c.List(ctx)
		if err != nil {
			return err
		}
		return encoder.Encode(runs)
	}
	run, err := c.Status(ctx, o.id)
	if err != nil {
		return err
	}
	return encoder.Encode(run)
}
