// Copyright 2019 Istio Authors
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

package helm

import (
	"bytes"
	"errors"
	"fmt"
	"html/template"
	"io/ioutil"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"k8s.io/helm/pkg/chartutil"
	"k8s.io/helm/pkg/engine"
	"k8s.io/helm/pkg/proto/hapi/chart"
	"k8s.io/helm/pkg/timeconv"

	"istio.io/istio/operator/pkg/util"
	"istio.io/istio/operator/pkg/vfs"
	"istio.io/pkg/log"
)

const (
	// YAMLSeparator is a separator for multi-document YAML files.
	YAMLSeparator = "\n---\n"

	// DefaultProfileString is the name of the default profile.
	DefaultProfileString = "default"

	// notes file name suffix for the helm chart.
	NotesFileNameSuffix = ".txt"
)

var (
	scope = log.RegisterScope("installer", "installer", 0)
)

// TemplateRenderer defines a helm template renderer interface.
type TemplateRenderer interface {
	// Run starts the renderer and should be called before using it.
	Run() error
	// RenderManifest renders the associated helm charts with the given values YAML string and returns the resulting
	// string.
	RenderManifest(values string) (string, error)
}

// NewHelmRenderer creates a new helm renderer with the given parameters and returns an interface to it.
// The format of helmBaseDir and profile strings determines the type of helm renderer returned (compiled-in, file,
// HTTP etc.)
func NewHelmRenderer(operatorDataDir, helmSubdir, componentName, namespace string) (TemplateRenderer, error) {
	dir := filepath.Join(ChartsSubdirName, helmSubdir)
	switch {
	case operatorDataDir == "":
		return NewVFSRenderer(dir, componentName, namespace), nil
	case util.IsFilePath(operatorDataDir):
		return NewFileTemplateRenderer(filepath.Join(operatorDataDir, dir), componentName, namespace), nil
	default:
		return nil, fmt.Errorf("unknown helm renderer with ChartsSubdirName=%s", operatorDataDir)
	}
}

// ReadProfileYAML reads the YAML values associated with the given profile. It uses an appropriate reader for the
// profile format (compiled-in, file, HTTP, etc.).
func ReadProfileYAML(profile string) (string, error) {
	var err error
	var globalValues string
	if profile == "" {
		scope.Infof("ReadProfileYAML for profile name: [Empty]")
	} else {
		scope.Infof("ReadProfileYAML for profile name: %s", profile)
	}

	// Get global values from profile.
	switch {
	case IsBuiltinProfileName(profile):
		if globalValues, err = LoadValuesVFS(profile); err != nil {
			return "", err
		}
	case util.IsFilePath(profile):
		scope.Infof("Loading values from local filesystem at path %s", profile)
		if globalValues, err = readFile(profile); err != nil {
			return "", err
		}
	default:
		return "", fmt.Errorf("unsupported Profile type: %s", profile)
	}

	return globalValues, nil
}

// renderChart renders the given chart with the given values and returns the resulting YAML manifest string.
func renderChart(namespace, values string, chrt *chart.Chart) (string, error) {
	config := &chart.Config{Raw: values, Values: map[string]*chart.Value{}}
	options := chartutil.ReleaseOptions{
		Name:      "istio",
		Time:      timeconv.Now(),
		Namespace: namespace,
	}

	vals, err := chartutil.ToRenderValuesCaps(chrt, config, options, nil)
	if err != nil {
		return "", err
	}

	files, err := engine.New().Render(chrt, vals)
	if err != nil {
		return "", err
	}

	// Create sorted array of keys to iterate over, to stabilize the order of the rendered templates
	keys := make([]string, 0, len(files))
	for k := range files {
		if strings.HasSuffix(k, NotesFileNameSuffix) {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var sb strings.Builder
	for i := 0; i < len(keys); i++ {
		f := files[keys[i]]
		// add yaml separator if the rendered file doesn't have one at the end
		f = strings.TrimSpace(f) + "\n"
		if !strings.HasSuffix(f, YAMLSeparator) {
			f += YAMLSeparator
		}
		_, err := sb.WriteString(f)
		if err != nil {
			return "", err
		}
	}

	return sb.String(), nil
}

// GenerateHubTagOverlay creates an IstioOperatorSpec overlay YAML for hub and tag.
func GenerateHubTagOverlay(hub, tag string) (string, error) {
	hubTagYAMLTemplate := `
spec:
  hub: {{.Hub}}
  tag: {{.Tag}}
`
	ts := struct {
		Hub string
		Tag string
	}{
		Hub: hub,
		Tag: tag,
	}
	return renderTemplate(hubTagYAMLTemplate, ts)
}

// helper method to render template
func renderTemplate(tmpl string, ts interface{}) (string, error) {
	t, err := template.New("").Parse(tmpl)
	if err != nil {
		return "", err
	}
	buf := new(bytes.Buffer)
	err = t.Execute(buf, ts)
	if err != nil {
		return "", err
	}
	return buf.String(), nil
}

// DefaultFilenameForProfile returns the profile name of the default profile for the given profile.
func DefaultFilenameForProfile(profile string) (string, error) {
	switch {
	case util.IsFilePath(profile):
		return filepath.Join(filepath.Dir(profile), DefaultProfileFilename), nil
	default:
		if _, ok := ProfileNames[profile]; ok || profile == "" {
			return DefaultProfileString, nil
		}
		return "", fmt.Errorf("bad profile string %s", profile)
	}
}

// IsDefaultProfile reports whether the given profile is the default profile.
func IsDefaultProfile(profile string) bool {
	return profile == "" || profile == DefaultProfileString || filepath.Base(profile) == DefaultProfileFilename
}

func readFile(path string) (string, error) {
	b, err := ioutil.ReadFile(path)
	return string(b), err
}

// GetAddonNamesFromCharts scans the charts directory for addon-components
func GetAddonNamesFromCharts(chartsRootDir string, capitalize bool) (addonChartNames []string, err error) {
	if chartsRootDir == "" {
		// VFS
		fnames, err := vfs.GetFilesRecursive(ChartsSubdirName)
		if err != nil {
			return nil, err
		}

		for _, fname := range fnames {
			basename := filepath.Base(fname)
			if basename == "Chart.yaml" {
				b, err := vfs.ReadFile(fname)
				if err != nil {
					return nil, err
				}
				bf := &chartutil.BufferedFile{
					Name: basename,
					Data: b,
				}
				bfs := []*chartutil.BufferedFile{bf}
				scope.Debugf("Chart loaded: %s", bf.Name)
				chart, err := chartutil.LoadFiles(bfs)
				if err != nil {
					return nil, err
				} else if addonName := getAddonName(chart.Metadata); addonName != nil {
					addonChartNames = append(addonChartNames, *addonName)
				}
			}
		}
	} else {
		// filesystem
		var chartFilenames []string
		err = filepath.Walk(chartsRootDir, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() {
				if ok, err := chartutil.IsChartDir(path); ok && err == nil {
					chartFilenames = append(chartFilenames, filepath.Join(path, chartutil.ChartfileName))
				}
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
		for _, filename := range chartFilenames {
			metadata, err := chartutil.LoadChartfile(filename)
			if err != nil {
				continue
			}
			if addonName := getAddonName(metadata); addonName != nil {
				addonChartNames = append(addonChartNames, *addonName)
			}
		}
	}
	// sort for consistent results
	sort.Strings(addonChartNames)
	// check for duplicates
	seen := make(map[string]bool)
	for i, name := range addonChartNames {
		if capitalize {
			name = strings.ToUpper(name[:1]) + name[1:]
			addonChartNames[i] = name
		}
		if seen[name] {
			return nil, errors.New("Duplicate AddonComponent defined: " + name)
		}
		seen[name] = true
	}
	return addonChartNames, nil
}

func getAddonName(metadata *chart.Metadata) *string {
	for _, str := range metadata.Keywords {
		if str == "istio-addon" {
			return &metadata.Name
		}
	}
	return nil
}

// GetProfileYAML returns the YAML for the given profile name, using the given profileOrPath string, which may be either
// a profile label or a file path.
func GetProfileYAML(installPackagePath, profileOrPath string) (string, error) {
	if profileOrPath == "" {
		profileOrPath = "default"
	}
	// If charts are a file path and profile is a name like default, transform it to the file path.
	if installPackagePath != "" && IsBuiltinProfileName(profileOrPath) {
		profileOrPath = filepath.Join(installPackagePath, "profiles", profileOrPath+".yaml")
	}
	// This contains the IstioOperator CR.
	baseCRYAML, err := ReadProfileYAML(profileOrPath)
	if err != nil {
		return "", err
	}

	if !IsDefaultProfile(profileOrPath) {
		// Profile definitions are relative to the default profileOrPath, so read that first.
		dfn, err := DefaultFilenameForProfile(profileOrPath)
		if err != nil {
			return "", err
		}
		defaultYAML, err := ReadProfileYAML(dfn)
		if err != nil {
			return "", err
		}
		baseCRYAML, err = util.OverlayYAML(defaultYAML, baseCRYAML)
		if err != nil {
			return "", err
		}
	}

	return baseCRYAML, nil
}
