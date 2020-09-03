//go:generate mapstructure-to-hcl2 -type Config
//go:generate struct-markdown

package manifest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hashicorp/hcl/v2/hcldec"
	"github.com/hashicorp/hcl/v2/hclparse"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/hashicorp/packer/common"
	hcl2shim "github.com/hashicorp/packer/hcl2template/shim"
	"github.com/hashicorp/packer/helper/config"
	"github.com/hashicorp/packer/packer"
	"github.com/hashicorp/packer/template/interpolate"
)

type Config struct {
	common.PackerConfig `mapstructure:",squash"`

	// The manifest will be written to this file. This defaults to
	// `packer-manifest.json`. When using a file that ends with ".pkrvars.hcl"
	// a hclvars version of the manifest will be generated.
	OutputPath string `mapstructure:"output"`
	// Write only filename without the path to the manifest file. This defaults
	// to false.
	StripPath bool `mapstructure:"strip_path"`
	// Don't write the `build_time` field from the output.
	StripTime bool `mapstructure:"strip_time"`
	// Arbitrary data to add to the manifest. This is a [template
	// engine](https://packer.io/docs/templates/engine.html). Therefore, you
	// may use user variables and template functions in this field.
	CustomData map[string]string `mapstructure:"custom_data"`
	ctx        interpolate.Context
}

type PostProcessor struct {
	config Config
}

type ManifestFile struct {
	Builds      []Artifact `json:"builds"`
	LastRunUUID string     `json:"last_run_uuid"`
}

func (p *PostProcessor) ConfigSpec() hcldec.ObjectSpec { return p.config.FlatMapstructure().HCL2Spec() }

func (p *PostProcessor) Configure(raws ...interface{}) error {
	err := config.Decode(&p.config, &config.DecodeOpts{
		Interpolate:        true,
		InterpolateContext: &p.config.ctx,
		InterpolateFilter: &interpolate.RenderFilter{
			Exclude: []string{},
		},
	}, raws...)
	if err != nil {
		return err
	}

	if p.config.OutputPath == "" {
		p.config.OutputPath = "packer-manifest.json"
	}

	if err = interpolate.Validate(p.config.OutputPath, &p.config.ctx); err != nil {
		return fmt.Errorf("Error parsing target template: %s", err)
	}

	return nil
}

func (p *PostProcessor) PostProcess(ctx context.Context, ui packer.Ui, source packer.Artifact) (packer.Artifact, bool, bool, error) {
	generatedData := source.State("generated_data")
	if generatedData == nil {
		// Make sure it's not a nil map so we can assign to it later.
		generatedData = make(map[string]interface{})
	}
	p.config.ctx.Data = generatedData

	for key, data := range p.config.CustomData {
		interpolatedData, err := createInterpolatedCustomData(&p.config, data)
		if err != nil {
			return nil, false, false, err
		}
		p.config.CustomData[key] = interpolatedData
	}

	artifact := &Artifact{}

	var err error
	var fi os.FileInfo
	var aFiles []ArtifactFile

	// Create the current artifact.
	for _, name := range source.Files() {
		af := ArtifactFile{}
		if fi, err = os.Stat(name); err == nil {
			af.Size = fi.Size()
		}
		if p.config.StripPath {
			af.Name = filepath.Base(name)
		} else {
			af.Name = name
		}
		aFiles = append(aFiles, af)
	}

	// Create a lock file with exclusive access. If this fails we will retry
	// after a delay.
	lockFilename := p.config.OutputPath + ".lock"
	defer lockFile(lockFilename)()

	// Read the current manifest file from disk
	var contents []byte
	if contents, err = ioutil.ReadFile(p.config.OutputPath); err != nil && !os.IsNotExist(err) {
		return source, true, true, fmt.Errorf("Unable to open %s for reading: %s", p.config.OutputPath, err)
	}

	if strings.HasSuffix(p.config.OutputPath, ".pkrvars.hcl") {
		return p.Hcl2Manifest(ctx, ui, contents, source, aFiles)
	}

	artifact.ArtifactFiles = aFiles
	artifact.ArtifactId = source.Id()
	artifact.CustomData = p.config.CustomData
	artifact.BuilderType = p.config.PackerBuilderType
	artifact.BuildName = p.config.PackerBuildName
	artifact.BuildTime = time.Now().Unix()
	if p.config.StripTime {
		artifact.BuildTime = 0
	}
	// Since each post-processor runs in a different process we need a way to
	// coordinate between various post-processors in a single packer run. We do
	// this by setting a UUID per run and tracking this in the manifest file.
	// When we detect that the UUID in the file is the same, we know that we are
	// part of the same run and we simply add our data to the list. If the UUID
	// is different we will check the -force flag and decide whether to truncate
	// the file before we proceed.
	artifact.PackerRunUUID = os.Getenv("PACKER_RUN_UUID")

	// Parse the manifest file JSON, if we have one
	manifestFile := &ManifestFile{}
	if len(contents) > 0 {
		if err = json.Unmarshal(contents, manifestFile); err != nil {
			return source, true, true, fmt.Errorf("Unable to parse content from %s: %s", p.config.OutputPath, err)
		}
	}

	// If -force is set and we are not on same run, truncate the file. Otherwise
	// we will continue to add new builds to the existing manifest file.
	if p.config.PackerForce && os.Getenv("PACKER_RUN_UUID") != manifestFile.LastRunUUID {
		manifestFile = &ManifestFile{}
	}

	// Add the current artifact to the manifest file
	manifestFile.Builds = append(manifestFile.Builds, *artifact)
	manifestFile.LastRunUUID = os.Getenv("PACKER_RUN_UUID")

	// Write JSON to disk
	if out, err := json.MarshalIndent(manifestFile, "", "  "); err == nil {
		if err = ioutil.WriteFile(p.config.OutputPath, out, 0664); err != nil {
			return source, true, true, fmt.Errorf("Unable to write %s: %s", p.config.OutputPath, err)
		}
	} else {
		return source, true, true, fmt.Errorf("Unable to marshal JSON %s", err)
	}

	// The manifest should never delete the artifacts it is set to record, so it
	// forcibly sets "keep" to true.
	return source, true, true, nil
}

// Create a lock file with exclusive access. If this fails we will retry
// after a delay.
func lockFile(lockFilename string) (cleanup func()) {
	for i := 0; i < 3; i++ {
		// The file should not be locked for very long so we'll keep this short.
		time.Sleep((time.Duration(i) * 200 * time.Millisecond))
		_, err := os.OpenFile(lockFilename, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0600)
		if err == nil {
			break
		}
		log.Printf("Error locking manifest file for reading and writing. Will sleep and retry. %s", err)
	}
	return func() { os.Remove(lockFilename) }
}

func (p *PostProcessor) Hcl2Manifest(ctx context.Context, ui packer.Ui, contents []byte, source packer.Artifact, aFiles []ArtifactFile) (packer.Artifact, bool, bool, error) {
	buildName := p.config.PackerBuildGroupName
	sourceName := p.config.PackerBuildName
	sourceType := p.config.PackerBuilderType

	file, diags := hclparse.NewParser().ParseHCL(contents, p.config.OutputPath)
	if diags.HasErrors() {
		err := fmt.Errorf("Failed to parse output file %s: %s", p.config.OutputPath, diags)
		return source, true, true, err
	}

	manifest := map[string]interface{}{}

	attrs, _ := file.Body.JustAttributes()
	buildAttr, found := attrs["manifest"]
	if found {
		buildVal, diags := buildAttr.Expr.Value(nil)
		if !diags.HasErrors() {
			manifest = hcl2shim.ConfigValueFromHCL2(buildVal).(map[string]interface{})
		}
	}
	entry := map[string]interface{}{
		"artifact_id":     source.Id(),
		"packer_run_uuid": os.Getenv("PACKER_RUN_UUID"),
	}
	if len(p.config.CustomData) > 0 {
		ct := map[string]interface{}{}
		for k, v := range p.config.CustomData {
			ct[k] = v
		}
		entry["custom_data"] = ct
	}
	if len(aFiles) > 0 {
		files := []interface{}{}
		for _, v := range aFiles {
			files = append(files, map[string]interface{}{
				"name": v.Name,
				"size": v.Size,
			})
		}
		entry["files"] = files
	}

	setMapVal(manifest, entry, buildName, sourceType, sourceName)

	wFile := hclwrite.NewEmptyFile()
	wBody := wFile.Body()
	for key, val := range attrs {
		v, _ := val.Expr.Value(nil)
		wBody.SetAttributeValue(key, v)
	}

	wBody.SetAttributeValue("manifest", hcl2shim.HCL2ValueFromConfigValue(manifest))

	var out bytes.Buffer
	_, err := wFile.WriteTo(&out)
	if err != nil {
		return source, true, true, err
	}

	return source, true, true, ioutil.WriteFile(p.config.OutputPath, out.Bytes(), 0664)
}

func setMapVal(target, toSet map[string]interface{}, keys ...string) {
	for _, key := range keys[:len(keys)-1] {
		if key == "" {
			continue
		}
		value, found := target[key]
		if !found {
			value = map[string]interface{}{}
			target[key] = value
		}
		target = value.(map[string]interface{})
	}
	target[keys[len(keys)-1]] = toSet
}

func createInterpolatedCustomData(config *Config, customData string) (string, error) {
	interpolatedCmd, err := interpolate.Render(customData, &config.ctx)
	if err != nil {
		return "", fmt.Errorf("Error interpolating custom data: %s", err)
	}
	return interpolatedCmd, nil
}
