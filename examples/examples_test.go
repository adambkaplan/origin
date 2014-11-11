package examples

import (
	"io/ioutil"
	"os"
	"path/filepath"

	"testing"

	kapi "github.com/GoogleCloudPlatform/kubernetes/pkg/api"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/runtime"

	"github.com/openshift/origin/pkg/api/latest"
	buildapi "github.com/openshift/origin/pkg/build/api"
	configapi "github.com/openshift/origin/pkg/config/api"
	deployapi "github.com/openshift/origin/pkg/deploy/api"
	imageapi "github.com/openshift/origin/pkg/image/api"
	projectapi "github.com/openshift/origin/pkg/project/api"
	templateapi "github.com/openshift/origin/pkg/template/api"
)

func TestExamples(t *testing.T) {
	expected := map[string]runtime.Object{

		"guestbook/template.json":                              &templateapi.Template{},

		"hello-openshift/hello-pod.json":                       &kapi.Pod{},
		"hello-openshift/hello-project.json":                   &projectapi.Project{},

		"sample-app/application-buildconfig.json":              &buildapi.BuildConfig{},
		"sample-app/github-webhook-example.json":               nil, // Skip.
		"sample-app/docker-registry-config.json":               &configapi.Config{},
		"sample-app/application-template-stibuild.json":        &templateapi.Template{},
		"sample-app/application-template-dockerbuild.json":     &templateapi.Template{},

		"../api/examples/build.json":                           &buildapi.Build{},
		"../api/examples/build-list.json":                      &buildapi.BuildList{},
		"../api/examples/build-results.json":                   &buildapi.Build{},
		"../api/examples/build-config.json":                    &buildapi.BuildConfig{},
		"../api/examples/build-config-list.json":               &buildapi.BuildConfigList{},

		"../api/examples/config.json":                          &configapi.Config{},

		"../api/examples/replication-controller.json":          &kapi.ReplicationController{},
		"../api/examples/replication-controller-list.json":     &kapi.ReplicationControllerList{},

		"../api/examples/deployment-config.json":               &deployapi.DeploymentConfig{},
		"../api/examples/deployment-config-list.json":          &deployapi.DeploymentConfigList{},
		"../api/examples/deployment.json":                      &deployapi.Deployment{},
		"../api/examples/deployment-list.json":                 &deployapi.DeploymentList{},

		"../api/examples/image.json":                           &imageapi.Image{},
		"../api/examples/image-list.json":                      &imageapi.ImageList{},
		"../api/examples/image-repository.json":                &imageapi.ImageRepository{},
		"../api/examples/image-repository-list.json":           &imageapi.ImageRepositoryList{},

		"../api/examples/pod.json":                             &kapi.Pod{},
		"../api/examples/pods.json":                            &kapi.Pod{},
		"../api/examples/pod-list.json":                        &kapi.PodList{},

		"../api/examples/project.json":                         &projectapi.Project{},
		"../api/examples/project-list.json":                    &projectapi.ProjectList{},
		"../api/examples/project-post.json":                    &projectapi.Project{},
		"../api/examples/project-put.json":                     &projectapi.Project{},

		"../api/examples/service.json":                         &kapi.Service{},
		"../api/examples/service-list.json":                    &kapi.ServiceList{},

		"../api/examples/template.json":                        &templateapi.Template{},

		"../api/examples/create-build.json":                    &buildapi.Build{},
		"../api/examples/create-build-config.json":             &buildapi.BuildConfig{},

		"../api/examples/create-image.json":                    &imageapi.Image{},
		"../api/examples/create-image-repository.json":         &imageapi.ImageRepository{},
		"../api/examples/create-image-repository-mapping.json": &imageapi.ImageRepositoryMapping{},
		"../api/examples/update-image.json":                    &imageapi.ImageRepository{},

		"../api/examples/create-pod.json":                      &kapi.Pod{},

		"../api/examples/create-service.json":                  &kapi.Service{},

		"../api/examples/alias.json":                           nil,
		"../api/examples/aliases.json":                         nil,
		"../api/examples/launch-build.json":                    nil,
		"../api/examples/envvar.json":                          nil,
		"../api/examples/envvars.json":                         nil,
		"../api/examples/link.json":                            nil,
		"../api/examples/links.json":                           nil,
		"../api/examples/update-link.json":                     nil,
		"../api/examples/create-link.json":                     nil,
		"../api/examples/status-success.json":                  nil,
	}

	// Add the root directory to search for files you want to test, if is not in the list below.
	rootDirs := []string{".", "../api/examples"}
	files := []string{}

	for _, rootDir := range rootDirs {
		err := filepath.Walk(rootDir, func(path string, f os.FileInfo, err error) error {
			if filepath.Ext(path) == ".json" {
				files = append(files, path)
			}
			return err
		})

		if err != nil {
			t.Errorf("%v", err)
		}
	}

	for _, file := range files {
		expectedObject, ok := expected[file]
		if !ok {
			t.Errorf("No test case defined for example JSON file '%v'", file)
			continue
		}
		if expectedObject == nil {
			continue
		}

		jsonData, _ := ioutil.ReadFile(file)
		if err := latest.Codec.DecodeInto(jsonData, expectedObject); err != nil {
			t.Errorf("Unexpected error while decoding example JSON file '%v': %v", file, err)
		}
	}
}

func TestReadme(t *testing.T) {
	path := "../README.md"
	_, err := ioutil.ReadFile(path)
	if err != nil {
		t.Fatalf("Unable to read file: %v", err)
	}
}
