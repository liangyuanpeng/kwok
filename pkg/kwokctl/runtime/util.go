/*
Copyright 2023 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package runtime

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"golang.org/x/sync/errgroup"

	"sigs.k8s.io/kwok/pkg/apis/internalversion"
	"sigs.k8s.io/kwok/pkg/config"
	"sigs.k8s.io/kwok/pkg/kwokctl/components"
	"sigs.k8s.io/kwok/pkg/utils/maps"
	"sigs.k8s.io/kwok/pkg/utils/path"
	"sigs.k8s.io/kwok/pkg/utils/slices"
)

// ForeachComponents starts components.
func (c *Cluster) ForeachComponents(ctx context.Context, reverse, order bool, fun func(ctx context.Context, component internalversion.Component) error) error {
	config, err := c.Config(ctx)
	if err != nil {
		return err
	}

	groups, err := components.GroupByLinks(config.Components)
	if err != nil {
		return err
	}
	if reverse {
		groups = slices.Reverse(groups)
	}

	if c.IsDryRun() {
		for _, group := range groups {
			for _, component := range group {
				err := fun(ctx, component)
				if err != nil {
					return err
				}
			}
		}
		return nil
	}

	if order {
		for _, group := range groups {
			if len(group) == 1 {
				if err := fun(ctx, group[0]); err != nil {
					return err
				}
			} else {
				g, ctx := errgroup.WithContext(ctx)
				for _, component := range group {
					component := component
					g.Go(func() error {
						return fun(ctx, component)
					})
				}
				if err := g.Wait(); err != nil {
					return err
				}
			}
		}
	} else {
		g, ctx := errgroup.WithContext(ctx)
		for _, group := range groups {
			for _, component := range group {
				component := component
				g.Go(func() error {
					return fun(ctx, component)
				})
			}
		}
		if err := g.Wait(); err != nil {
			return err
		}
	}
	return nil
}

// GetComponentPatches returns the patches for a component.
func GetComponentPatches(conf *internalversion.KwokctlConfiguration, componentName string) internalversion.ComponentPatches {
	componentPatches, _ := slices.Find(conf.ComponentsPatches, func(patch internalversion.ComponentPatches) bool {
		return patch.Name == componentName
	})
	return componentPatches
}

// ApplyComponentPatches applies patches to a component.
func ApplyComponentPatches(component *internalversion.Component, patches []internalversion.ComponentPatches) {
	for _, patch := range patches {
		applyComponentPatch(component, patch)
	}
}

func applyComponentPatch(component *internalversion.Component, patch internalversion.ComponentPatches) {
	if patch.Name != component.Name {
		return
	}

	component.Volumes = append(component.Volumes, patch.ExtraVolumes...)
	component.Envs = append(component.Envs, patch.ExtraEnvs...)
	applyComponentPatchArgs(component, patch)
}

func applyComponentPatchArgs(component *internalversion.Component, patch internalversion.ComponentPatches) {
	if patch.Name != component.Name {
		return
	}
	argsmap := make(map[string][]string)
	for _, arg := range component.Args {
		k, v := getKeyValueFromArg(arg)
		if k == "" || v == "" {
			continue
		}
		values := []string{}
		if _, ok := argsmap[k]; ok {
			values = argsmap[k]
		}
		values = append(values, v)
		argsmap[k] = values
	}

	for _, a := range patch.ExtraArgs {
		_, existOldArg := argsmap[a.Key]
		values := []string{}
		if _, ok := argsmap[a.Key]; ok {
			values = argsmap[a.Key]
		}
		if existOldArg && a.Override {
			values = []string{}
		}
		values = append(values, a.Value)
		argsmap[a.Key] = values
	}

	component.Args = []string{}
	for k, v := range argsmap {
		for _, v1 := range v {
			component.Args = append(component.Args, fmt.Sprintf("--%s=%s", k, v1))
		}
	}
	sort.Strings(component.Args)
}

func getKeyValueFromArg(arg string) (string, string) {
	if !strings.Contains(arg, "=") {
		return "", ""
	}
	if !strings.HasPrefix(arg, "--") {
		return "", ""
	}
	strstmp := strings.Split(arg, "=")
	return strings.ReplaceAll(strstmp[0], "--", ""), strings.ReplaceAll(arg, strstmp[0]+"=", "")
}

// ExpandVolumesHostPaths expands relative paths specified in volumes to absolute paths
func ExpandVolumesHostPaths(volumes []internalversion.Volume) ([]internalversion.Volume, error) {
	result := make([]internalversion.Volume, 0, len(volumes))
	for _, v := range volumes {
		hostPath, err := path.Expand(v.HostPath)
		if err != nil {
			return nil, err
		}
		v.HostPath = hostPath
		result = append(result, v)
	}
	return result, nil
}

// GetLogVolumes returns volumes for Logs and ClusterLogs resource.
func GetLogVolumes(ctx context.Context) []internalversion.Volume {
	logs := config.FilterWithTypeFromContext[*internalversion.Logs](ctx)
	clusterLogs := config.FilterWithTypeFromContext[*internalversion.ClusterLogs](ctx)
	attaches := config.FilterWithTypeFromContext[*internalversion.Attach](ctx)
	clusterAttaches := config.FilterWithTypeFromContext[*internalversion.ClusterAttach](ctx)

	// Mount log dirs
	mountDirs := map[string]struct{}{}
	for _, log := range logs {
		for _, l := range log.Spec.Logs {
			mountDirs[path.Dir(l.LogsFile)] = struct{}{}
		}
	}

	for _, cl := range clusterLogs {
		for _, l := range cl.Spec.Logs {
			mountDirs[path.Dir(l.LogsFile)] = struct{}{}
		}
	}

	for _, attach := range attaches {
		for _, a := range attach.Spec.Attaches {
			mountDirs[path.Dir(a.LogsFile)] = struct{}{}
		}
	}

	for _, ca := range clusterAttaches {
		for _, a := range ca.Spec.Attaches {
			mountDirs[path.Dir(a.LogsFile)] = struct{}{}
		}
	}

	logsDirs := maps.Keys(mountDirs)
	sort.Strings(logsDirs)

	volumes := make([]internalversion.Volume, 0, len(logsDirs))
	for i, logsDir := range logsDirs {
		volumes = append(volumes, internalversion.Volume{
			Name:      fmt.Sprintf("log-volume-%d", i),
			HostPath:  logsDir,
			MountPath: logsDir,
			PathType:  internalversion.HostPathDirectoryOrCreate,
			ReadOnly:  true,
		})
	}
	return volumes
}
