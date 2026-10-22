// Copyright 2026 Microsoft Corporation
// Licensed under the Apache License, Version 2.0.
package performance

import (
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"strings"

	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/startup"
)

// PopulationImageBinding authorizes only an expectation change for one catalog
// container in a new run. It neither updates live images nor rewrites history.
type PopulationImageBinding struct {
	Kind          string `json:"kind"`
	Component     string `json:"component"`
	Container     string `json:"container"`
	Init          bool   `json:"init,omitempty"`
	PreviousImage string `json:"previousImage"`
	Image         string `json:"image"`
}

var populationImage = regexp.MustCompile(`^[a-z0-9][a-z0-9._:/-]*@sha256:[0-9a-f]{64}$`)

func validatePopulationImages(target startup.Spec, bindings []PopulationImageBinding) error {
	if len(bindings) > 6 {
		return errors.New("population image bindings exceed catalog inventory")
	}
	seen := map[string]bool{}
	for _, b := range bindings {
		if b.Kind != "Deployment" || (b.Component != "certified-operators-catalog" && b.Component != "community-operators-catalog" && b.Component != "redhat-operators-catalog") || (!b.Init && b.Container != "registry") || (b.Init && b.Container != "extract-content") {
			return errors.New("population image binding requires catalog Deployment registry or init extract-content")
		}
		for _, image := range []string{b.PreviousImage, b.Image} {
			repository, _, _ := strings.Cut(image, "@")
			if !populationImage.MatchString(image) || strings.Contains(repository[strings.LastIndex(repository, "/")+1:], ":") {
				return errors.New("population image binding requires exact untagged sha256 images")
			}
		}
		key := fmt.Sprintf("%s/%s/%t/%s", b.Kind, b.Component, b.Init, b.Container)
		if seen[key] || b.PreviousImage == b.Image {
			return errors.New("duplicate or unchanged population image binding")
		}
		seen[key] = true
		matches := 0
		for _, component := range target.Components {
			if component.Kind != b.Kind || component.Name != b.Component {
				continue
			}
			containers := component.Containers
			if b.Init {
				containers = component.InitContainers
			}
			for _, container := range containers {
				if container.Name == b.Container && container.Image == b.Image {
					matches++
				}
			}
		}
		if matches != 1 {
			return errors.New("population image binding must match exactly one target container/image")
		}
	}
	return nil
}

func populationStartup(source, target startup.Spec, bindings []PopulationImageBinding) error {
	if err := validatePopulationImages(target, bindings); err != nil {
		return err
	}
	if len(bindings) != 0 {
		// Copy only the slices we edit; all other startup fields remain exact.
		source.Components = append([]startup.Component(nil), source.Components...)
		for i := range source.Components {
			c := &source.Components[i]
			c.Containers = append(c.Containers[:0:0], c.Containers...)
			c.InitContainers = append(c.InitContainers[:0:0], c.InitContainers...)
		}
	}
	for _, b := range bindings {
		matches := 0
		for _, component := range source.Components {
			if component.Kind != b.Kind || component.Name != b.Component {
				continue
			}
			containers := component.Containers
			if b.Init {
				containers = component.InitContainers
			}
			for i := range containers {
				if containers[i].Name == b.Container && containers[i].Image == b.PreviousImage {
					containers[i].Image = b.Image
					matches++
				}
			}
		}
		if matches != 1 {
			return errors.New("population previous image binding mismatch")
		}
	}
	if !reflect.DeepEqual(source, target) {
		return errors.New("population policy changed")
	}
	return nil
}
