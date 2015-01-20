package v1beta1

import (
	"github.com/GoogleCloudPlatform/kubernetes/pkg/api"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/conversion"
	newer "github.com/openshift/origin/pkg/build/api"
	image "github.com/openshift/origin/pkg/image/api"
)

func init() {
	api.Scheme.AddConversionFuncs(
		// Rename STIBuildStrategy.BuildImage to STIBuildStrategy.Image
		func(in *newer.STIBuildStrategy, out *STIBuildStrategy, s conversion.Scope) error {
			out.BuilderImage = in.Image
			out.Image = in.Image
			out.Scripts = in.Scripts
			out.Clean = in.Clean
			return nil
		},
		func(in *STIBuildStrategy, out *newer.STIBuildStrategy, s conversion.Scope) error {
			out.Scripts = in.Scripts
			out.Clean = in.Clean
			if len(in.Image) != 0 {
				out.Image = in.Image
			} else {
				out.Image = in.BuilderImage
			}
			return nil
		},
		// Deprecate ImageTag and Registry, replace with To / Tag / DockerImageReference
		func(in *newer.BuildOutput, out *BuildOutput, s conversion.Scope) error {
			if err := s.Convert(&in.To, &out.To, 0); err != nil {
				return err
			}
			out.Tag = in.Tag
			if len(in.DockerImageReference) > 0 {
				out.DockerImageReference = in.DockerImageReference
				registry, namespace, name, tag, _ := image.SplitDockerPullSpec(in.DockerImageReference)
				out.Registry = registry
				out.ImageTag = image.JoinDockerPullSpec("", namespace, name, tag)
			}
			return nil
		},
		func(in *BuildOutput, out *newer.BuildOutput, s conversion.Scope) error {
			if err := s.Convert(&in.To, &out.To, 0); err != nil {
				return err
			}
			out.Tag = in.Tag
			if len(in.DockerImageReference) > 0 {
				out.DockerImageReference = in.DockerImageReference
				return nil
			}
			if len(in.ImageTag) != 0 {
				_, namespace, name, tag, err := image.SplitDockerPullSpec(in.ImageTag)
				if err != nil {
					return err
				}
				out.DockerImageReference = image.JoinDockerPullSpec(in.Registry, namespace, name, tag)
			}
			return nil
		},
	)
}
