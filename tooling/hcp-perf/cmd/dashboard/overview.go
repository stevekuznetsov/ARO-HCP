// Copyright 2026 Microsoft Corporation
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at http://www.apache.org/licenses/LICENSE-2.0
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package dashboard

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	core "github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/dashboard"
)

func readOverview(ctx context.Context, input string) (core.Overview, error) {
	var result core.Overview
	file, err := os.Open(input)
	if err != nil {
		return result, err
	}
	var spec core.OverviewSpec
	err = readJSON(file, &spec)
	file.Close()
	if err != nil {
		return result, err
	}
	if err := spec.Validate(); err != nil {
		return result, err
	}
	root, err := os.OpenRoot(filepath.Dir(input))
	if err != nil {
		return result, err
	}
	defer root.Close()
	datasets := make([]core.Dataset, len(spec.Groups))
	for i, group := range spec.Groups {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		// Root confinement also rejects symlinks that escape the public directory.
		info, err := root.Stat(group.Input)
		if err != nil {
			return result, fmt.Errorf("input %s: %w", group.Input, err)
		}
		if !info.Mode().IsRegular() {
			return result, fmt.Errorf("input %s must be regular", group.Input)
		}
		file, err := root.Open(group.Input)
		if err != nil {
			return result, err
		}
		err = readJSON(file, &datasets[i])
		file.Close()
		if err != nil {
			return result, fmt.Errorf("input %s: %w", group.Input, err)
		}
		if err := validateDataset(datasets[i]); err != nil {
			return result, fmt.Errorf("input %s: %w", group.Input, err)
		}
	}
	return core.BuildOverview(spec, datasets)
}
