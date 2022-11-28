/*
Copyright 2019 The Fission Authors.

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

package _package

import (
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"time"

	"github.com/dchest/uniuri"
	"github.com/pkg/errors"
	uuid "github.com/satori/go.uuid"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	fv1 "github.com/fission/fission/pkg/apis/core/v1"
	"github.com/fission/fission/pkg/fission-cli/cliwrapper/cli"
	"github.com/fission/fission/pkg/fission-cli/cmd"
	"github.com/fission/fission/pkg/fission-cli/cmd/spec"
	"github.com/fission/fission/pkg/fission-cli/console"
	flagkey "github.com/fission/fission/pkg/fission-cli/flag/key"
	"github.com/fission/fission/pkg/fission-cli/util"
	"github.com/fission/fission/pkg/utils"
)

type CreateSubCommand struct {
	cmd.CommandActioner
}

func Create(input cli.Input) error {
	return (&CreateSubCommand{}).do(input)
}

func (opts *CreateSubCommand) do(input cli.Input) error {
	err := opts.run(input)
	if err != nil {
		return err
	}
	return nil
}

func (opts *CreateSubCommand) run(input cli.Input) error {
	pkgName := input.String(flagkey.PkgName)
	if len(pkgName) == 0 {
		if input.Bool(flagkey.SpecSave) && len(input.String(flagkey.PkgName)) == 0 {
			return errors.Errorf("--%v is necessary when creating spec file", flagkey.PkgName)
		} else {
			console.Warn(fmt.Sprintf("--%v will be soon marked as required flag, see 'help' for details", flagkey.HtName))
		}
	}

	envName := input.String(flagkey.PkgEnvironment)

	userProvidedNS, pkgNamespace, err := opts.GetResourceNamespace(input, flagkey.NamespacePackage)
	if err != nil {
		return fv1.AggregateValidationErrors("Environment", err)
	}

	srcArchiveFiles := input.StringSlice(flagkey.PkgSrcArchive)
	deployArchiveFiles := input.StringSlice(flagkey.PkgDeployArchive)
	buildcmd := input.String(flagkey.PkgBuildCmd)

	noZip := false
	code := input.String(flagkey.PkgCode)
	if len(code) == 0 {
		deployArchiveFiles = input.StringSlice(flagkey.PkgDeployArchive)
	} else {
		deployArchiveFiles = append(deployArchiveFiles, input.String(flagkey.PkgCode))
		noZip = true
	}

	if len(srcArchiveFiles) == 0 && len(deployArchiveFiles) == 0 {
		return errors.Errorf("need --%v or --%v or --%v argument", flagkey.PkgCode, flagkey.PkgSrcArchive, flagkey.PkgDeployArchive)
	}

	var specDir, specFile string

	if input.Bool(flagkey.SpecSave) {
		// since package CRD created using --spec, not validate by k8s. So we need to validate it and make sure package name is not more than 63 characters.
		if len(pkgName) > 63 {
			return errors.Errorf("error creating package: package name %v, must be no more than 63 characters", pkgName)
		}

		specDir = util.GetSpecDir(input)
		specIgnore := util.GetSpecIgnore(input)
		fr, err := spec.ReadSpecs(specDir, specIgnore, false)
		if err != nil {
			return errors.Wrap(err, fmt.Sprintf("error reading spec in '%v'", specDir))
		}
		exists, err := fr.ExistsInSpecs(fv1.Environment{
			ObjectMeta: metav1.ObjectMeta{
				Name:      envName,
				Namespace: userProvidedNS,
			},
		})
		if err != nil {
			return err
		}
		if !exists {
			console.Warn(fmt.Sprintf("Package '%s' references unknown Environment '%s' in Namespace '%s', please create it before applying spec",
				pkgName, envName, userProvidedNS))
		}

		specDir = util.GetSpecDir(input)
		specFile = fmt.Sprintf("package-%s.yaml", pkgName)
	}

	_, err = CreatePackage(input, opts.Client(), pkgName, pkgNamespace, envName,
		srcArchiveFiles, deployArchiveFiles, buildcmd, specDir, specFile, noZip, userProvidedNS)

	return err
}

// TODO: get all necessary value from CLI input directly
func CreatePackage(input cli.Input, client cmd.Client, pkgName string, pkgNamespace string, envName string,
	srcArchiveFiles []string, deployArchiveFiles []string, buildcmd string, specDir string, specFile string, noZip bool, userProvidedNS string) (*metav1.ObjectMeta, error) {

	insecure := input.Bool(flagkey.PkgInsecure)
	deployChecksum := input.String(flagkey.PkgDeployChecksum)
	srcChecksum := input.String(flagkey.PkgSrcChecksum)

	pkgSpec := fv1.PackageSpec{
		Environment: fv1.EnvironmentReference{
			Namespace: pkgNamespace,
			Name:      envName,
		},
	}
	if input.Bool(flagkey.SpecSave) || input.Bool(flagkey.SpecDry) {
		pkgSpec = fv1.PackageSpec{
			Environment: fv1.EnvironmentReference{
				Namespace: userProvidedNS,
				Name:      envName,
			},
		}
	}

	var pkgStatus fv1.BuildStatus = fv1.BuildStatusSucceeded

	if len(deployArchiveFiles) > 0 {
		if len(specFile) > 0 { // we should do this in all cases, i think
			pkgStatus = fv1.BuildStatusNone
		}
		deployment, err := CreateArchive(client, input, deployArchiveFiles, noZip, insecure, deployChecksum, specDir, specFile)
		if err != nil {
			return nil, errors.Wrap(err, "error creating source archive")
		}
		pkgSpec.Deployment = *deployment
		if len(pkgName) == 0 {
			pkgName = util.KubifyName(fmt.Sprintf("%v-%v", path.Base(deployArchiveFiles[0]), uniuri.NewLen(4)))
		}
	}
	if len(srcArchiveFiles) > 0 {
		source, err := CreateArchive(client, input, srcArchiveFiles, false, insecure, srcChecksum, specDir, specFile)
		if err != nil {
			return nil, errors.Wrap(err, "error creating deploy archive")
		}
		pkgSpec.Source = *source
		pkgStatus = fv1.BuildStatusPending // set package build status to pending
		if len(pkgName) == 0 {
			pkgName = util.KubifyName(fmt.Sprintf("%v-%v", path.Base(srcArchiveFiles[0]), uniuri.NewLen(4)))
		}
	}

	if len(buildcmd) > 0 {
		pkgSpec.BuildCommand = buildcmd
	}

	if len(pkgName) == 0 {
		id, err := uuid.NewV4()
		if err != nil {
			return nil, errors.Wrap(err, "error generating UUID")
		}
		pkgName = strings.ToLower(id.String())
	}

	pkg := &fv1.Package{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pkgName,
			Namespace: userProvidedNS,
		},
		Spec: pkgSpec,
		Status: fv1.PackageStatus{
			BuildStatus:         pkgStatus,
			LastUpdateTimestamp: metav1.Time{Time: time.Now().UTC()},
		},
	}

	if input.Bool(flagkey.SpecDry) {
		return &pkg.ObjectMeta, spec.SpecDry(*pkg)
	}

	if input.Bool(flagkey.SpecSave) {
		// if a package with the same spec exists, don't create a new spec file
		fr, err := spec.ReadSpecs(util.GetSpecDir(input), util.GetSpecIgnore(input), false)
		if err != nil {
			return nil, errors.Wrap(err, "error reading specs")
		}

		obj := fr.SpecExists(pkg, true, true)
		if obj != nil {
			pkg := obj.(*fv1.Package)
			fmt.Printf("Re-using previously created package %v\n", pkg.ObjectMeta.Name)
			return &pkg.ObjectMeta, nil
		}

		err = spec.SpecSave(*pkg, specFile)
		if err != nil {
			return nil, errors.Wrap(err, "error saving package spec")
		}
		return &pkg.ObjectMeta, nil
	} else if !input.Bool(flagkey.SpecSave) {
		pkg.ObjectMeta.Namespace = pkgNamespace

		pkgMetadata, err := client.FissionClientSet.CoreV1().Packages(pkgNamespace).Create(input.Context(), pkg, metav1.CreateOptions{})
		if err != nil {
			return nil, errors.Wrap(err, "error creating package")
		}
		fmt.Printf("Package '%v' created\n", pkgMetadata.GetName())
		pkg.ObjectMeta = pkgMetadata.ObjectMeta
	}

	if input.Bool(flagkey.StreamLog) {
		ns := "fission"
		// Get function Pods first
		selector := map[string]string{
			utils.Service: utils.BuilMgr,
		}
		pod, err := client.KubernetesClient.CoreV1().Pods(ns).List(input.Context(), metav1.ListOptions{
			LabelSelector: labels.Set(selector).AsSelector().String(),
		})
		if err != nil {
			return &pkg.ObjectMeta, err
		}
		cTime := time.Now().UTC()
		currentTime := metav1.NewTime(cTime)
		req := client.KubernetesClient.CoreV1().Pods(ns).GetLogs(pod.Items[0].Name, &v1.PodLogOptions{
			SinceTime: &currentTime,
			Follow:    true,
		})
		podLogs, err := req.Stream(input.Context())
		if err != nil {
			return &pkg.ObjectMeta, errors.Errorf("error in opening stream %s", err.Error())
		}
		defer podLogs.Close()

		_, err = io.Copy(os.Stdout, podLogs)
		if err != nil {
			return &pkg.ObjectMeta, errors.Errorf("error in copy information from podLogs to buf")
		}
	}
	return &pkg.ObjectMeta, nil
}
