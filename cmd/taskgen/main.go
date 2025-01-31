/*
Copyright 2022 The Tekton Authors
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

package main

import (
	"bytes"
	"flag"
	tektonapi "github.com/tektoncd/pipeline/pkg/apis/pipeline/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/cli-runtime/pkg/printers"
	"k8s.io/klog/v2"
	"os"
	"path/filepath"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"strings"
)

func main() {
	var buildahTask string
	var buildahRemoteTask string

	flag.StringVar(&buildahTask, "buildah-task", "", "The location of the buildah task")
	flag.StringVar(&buildahRemoteTask, "remote-task", "", "The location of the buildah-remote task to overwrite")

	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(flag.CommandLine)
	klog.InitFlags(flag.CommandLine)
	flag.Parse()
	if buildahTask == "" || buildahRemoteTask == "" {
		println("Must specify both buildah-task and remote-task params")
		os.Exit(1)
	}

	task := tektonapi.Task{}
	streamFileYamlToTektonObj(buildahTask, &task)

	decodingScheme := runtime.NewScheme()
	utilruntime.Must(tektonapi.AddToScheme(decodingScheme))
	convertToSsh(&task)
	y := printers.YAMLPrinter{}
	b := bytes.Buffer{}
	_ = y.PrintObj(&task, &b)
	err := os.WriteFile(buildahRemoteTask, b.Bytes(), 0660)
	if err != nil {
		panic(err)
	}
}

func decodeBytesToTektonObjbytes(bytes []byte, obj runtime.Object) runtime.Object {
	decodingScheme := runtime.NewScheme()
	utilruntime.Must(tektonapi.AddToScheme(decodingScheme))
	decoderCodecFactory := serializer.NewCodecFactory(decodingScheme)
	decoder := decoderCodecFactory.UniversalDecoder(tektonapi.SchemeGroupVersion)
	err := runtime.DecodeInto(decoder, bytes, obj)
	if err != nil {
		panic(err)
	}
	return obj
}

func streamFileYamlToTektonObj(path string, obj runtime.Object) runtime.Object {
	bytes, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		panic(err)
	}
	return decodeBytesToTektonObjbytes(bytes, obj)
}

//script
//set 1 sets up the ssh server

func convertToSsh(task *tektonapi.Task) {

	for stepPod := range task.Spec.Steps {
		step := &task.Spec.Steps[stepPod]
		if step.Name != "build" {
			continue
		}
		podmanArgs := ""

		ret := `set -o verbose
mkdir -p ~/.ssh
if [ -e "/ssh/error" ]; then
  #no server could be provisioned
  cat /ssh/error
  exit 1
elif [ -e "/ssh/otp" ]; then
 curl --cacert /ssh/otp-ca -XPOST -d @/ssh/otp $(cat /ssh/otp-server) >~/.ssh/id_rsa
 echo "" >> ~/.ssh/id_rsa
else
  cp /ssh/id_rsa ~/.ssh
fi
chmod 0400 ~/.ssh/id_rsa
export SSH_HOST=$(cat /ssh/host)
export BUILD_DIR=$(cat /ssh/user-dir)
export SSH_ARGS="-o StrictHostKeyChecking=no"
mkdir -p scripts
echo "$BUILD_DIR"
ssh $SSH_ARGS "$SSH_HOST"  mkdir -p "$BUILD_DIR/workspaces" "$BUILD_DIR/scripts"

PORT_FORWARD=""
PODMAN_PORT_FORWARD=""
if [ -n "$JVM_BUILD_WORKSPACE_ARTIFACT_CACHE_PORT_80_TCP_ADDR" ] ; then
PORT_FORWARD=" -L 80:$JVM_BUILD_WORKSPACE_ARTIFACT_CACHE_PORT_80_TCP_ADDR:80"
PODMAN_PORT_FORWARD=" -e JVM_BUILD_WORKSPACE_ARTIFACT_CACHE_PORT_80_TCP_ADDR=localhost"
fi
`

		env := "$PODMAN_PORT_FORWARD"
		//before the build we sync the contents of the workspace to the remote host
		for _, workspace := range task.Spec.Workspaces {
			ret += "\nrsync -ra $(workspaces." + workspace.Name + ".path)/ \"$SSH_HOST:$BUILD_DIR/workspaces/" + workspace.Name + "/\""
			podmanArgs += " -v \"$BUILD_DIR/workspaces/" + workspace.Name + ":$(workspaces." + workspace.Name + ".path):Z\" "
		}
		script := "scripts/script-" + step.Name + ".sh"

		ret += "\ncat >" + script + " <<'REMOTESSHEOF'\n"
		if !strings.HasPrefix(step.Script, "#!") {
			ret += "#!/bin/sh\nset -o verbose\n"
		}
		if step.WorkingDir != "" {
			ret += "cd " + step.WorkingDir + "\n"

		}

		ret += step.Script
		ret += "\nbuildah push \"$IMAGE\" oci:rhtap-final-image"
		ret += "\nREMOTESSHEOF"
		ret += "\nchmod +x " + script

		if task.Spec.StepTemplate != nil {
			for _, e := range task.Spec.StepTemplate.Env {
				env += " -e " + e.Name + "=\"$" + e.Name + "\""
			}
		}
		ret += "\nrsync -ra scripts \"$SSH_HOST:$BUILD_DIR\""
		containerScript := "/script/script-" + step.Name + ".sh"
		for _, e := range step.Env {
			env += " -e " + e.Name + "=\"$" + e.Name + "\" "
		}

		ret += "\nssh $SSH_ARGS \"$SSH_HOST\" $PORT_FORWARD podman  run " + env + " --rm " + podmanArgs + " -v $BUILD_DIR/scripts:/script:Z --user=0  \"$BUILDER_IMAGE\" " + containerScript

		//sync the contents of the workspaces back so subsequent tasks can use them
		for _, workspace := range task.Spec.Workspaces {
			ret += "\nrsync -ra \"$SSH_HOST:$BUILD_DIR/workspaces/" + workspace.Name + "/\" \"$(workspaces." + workspace.Name + ".path)/\""
		}
		ret += "\nbuildah pull oci:rhtap-final-image"
		ret += "\nbuildah images"
		ret += "\nbuildah tag localhost/rhtap-final-image \"$IMAGE\""
		ret += "\ncontainer=$(buildah from --pull-never \"$IMAGE\")\nbuildah mount \"$container\" | tee /workspace/container_path\necho $container > /workspace/container_name"

		for _, i := range strings.Split(ret, "\n") {
			if strings.HasSuffix(i, " ") {
				panic(i)
			}
		}
		step.Script = ret
		step.Image = "quay.io/redhat-appstudio/multi-platform-runner:01c7670e81d5120347cf0ad13372742489985e5f@sha256:246adeaaba600e207131d63a7f706cffdcdc37d8f600c56187123ec62823ff44"
		step.ImagePullPolicy = v1.PullAlways
		step.VolumeMounts = append(step.VolumeMounts, v1.VolumeMount{
			Name:      "ssh",
			ReadOnly:  true,
			MountPath: "/ssh",
		})
	}

	task.Name = "buildah-remote"
	task.Spec.Params = append(task.Spec.Params, tektonapi.ParamSpec{Name: "PLATFORM", Type: tektonapi.ParamTypeString, Description: "The platform to build on"})

	faleVar := false
	task.Spec.Volumes = append(task.Spec.Volumes, v1.Volume{
		Name: "ssh",
		VolumeSource: v1.VolumeSource{
			Secret: &v1.SecretVolumeSource{
				SecretName: "multi-platform-ssh-$(context.taskRun.name)",
				Optional:   &faleVar,
			},
		},
	})
	task.Spec.StepTemplate.Env = append(task.Spec.StepTemplate.Env, v1.EnvVar{Name: "BUILDER_IMAGE", Value: "$(params.BUILDER_IMAGE)"})
}
