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

package extract

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"
	intoto "github.com/in-toto/in-toto-golang/in_toto"
	"github.com/in-toto/in-toto-golang/in_toto/slsa_provenance/common"
	"github.com/tektoncd/chains/internal/backport"
	"github.com/tektoncd/chains/pkg/artifacts"
	"github.com/tektoncd/chains/pkg/chains/formats/slsa/internal/slsaconfig"
	"github.com/tektoncd/chains/pkg/chains/objects"
	"github.com/tektoncd/pipeline/pkg/apis/pipeline/v1beta1"
	"knative.dev/pkg/logging"
)

// SubjectDigests returns software artifacts produced from the TaskRun/PipelineRun object
// in the form of standard subject field of intoto statement.
// The type hinting fields expected in results help identify the generated software artifacts.
// Valid type hinting fields must:
//   - have suffix `IMAGE_URL` & `IMAGE_DIGEST` or `ARTIFACT_URI` & `ARTIFACT_DIGEST` pair.
//   - the `*_DIGEST` field must be in the format of "<algorithm>:<actual-sha>" where the algorithm must be "sha256" and actual sha must be valid per https://github.com/opencontainers/image-spec/blob/main/descriptor.md#sha-256.
//   - the `*_URL` or `*_URI` fields cannot be empty.
//
//nolint:all
func SubjectDigests(ctx context.Context, obj objects.TektonObject, slsaconfig *slsaconfig.SlsaConfig) []intoto.Subject {
	var subjects []intoto.Subject

	switch obj.GetObject().(type) {
	case *v1beta1.PipelineRun:
		subjects = subjectsFromPipelineRun(ctx, obj, slsaconfig)
	case *v1beta1.TaskRun:
		subjects = subjectsFromTektonObject(ctx, obj)
	}

	return subjects
}

func subjectsFromPipelineRun(ctx context.Context, obj objects.TektonObject, slsaconfig *slsaconfig.SlsaConfig) []intoto.Subject {
	prSubjects := subjectsFromTektonObject(ctx, obj)

	// If deep inspection is not enabled, just return subjects observed on the pipelinerun level
	if !slsaconfig.DeepInspectionEnabled {
		return prSubjects
	}

	logger := logging.FromContext(ctx)
	// If deep inspection is enabled, collect subjects from child taskruns
	var result []intoto.Subject

	pro := obj.(*objects.PipelineRunObject)

	pSpec := pro.Status.PipelineSpec
	if pSpec != nil {
		pipelineTasks := append(pSpec.Tasks, pSpec.Finally...)
		for _, t := range pipelineTasks {
			tr := pro.GetTaskRunFromTask(t.Name)
			// Ignore Tasks that did not execute during the PipelineRun.
			if tr == nil || tr.Status.CompletionTime == nil {
				logger.Infof("taskrun status not found for task %s", t.Name)
				continue
			}

			trSubjects := subjectsFromTektonObject(ctx, tr)
			for _, s := range trSubjects {
				result = addSubject(result, s)
			}
		}
	}

	// also add subjects observed from pipelinerun level with duplication removed
	for _, s := range prSubjects {
		result = addSubject(result, s)
	}

	return result
}

// addSubject adds a new subject item to the original slice.
func addSubject(original []intoto.Subject, item intoto.Subject) []intoto.Subject {

	for i, s := range original {
		// if there is an equivalent entry in the original slice, merge item's DigestSet
		// into the existing entry's DigestSet.
		if subjectEqual(s, item) {
			mergeMaps(original[i].Digest, item.Digest)
			return original
		}
	}

	original = append(original, item)
	return original
}

// two subjects are equal if and only if they have same name and have at least
// one common algorithm and hex value.
func subjectEqual(x, y intoto.Subject) bool {
	if x.Name != y.Name {
		return false
	}
	for algo, hex := range x.Digest {
		if y.Digest[algo] == hex {
			return true
		}
	}
	return false
}

func mergeMaps(m1 map[string]string, m2 map[string]string) {
	for k, v := range m2 {
		m1[k] = v
	}
}

func subjectsFromTektonObject(ctx context.Context, obj objects.TektonObject) []intoto.Subject {
	logger := logging.FromContext(ctx)
	var subjects []intoto.Subject

	imgs := artifacts.ExtractOCIImagesFromResults(ctx, obj)
	for _, i := range imgs {
		if d, ok := i.(name.Digest); ok {
			subjects = append(subjects, intoto.Subject{
				Name: d.Repository.Name(),
				Digest: common.DigestSet{
					"sha256": strings.TrimPrefix(d.DigestStr(), "sha256:"),
				},
			})
		}
	}

	sts := artifacts.ExtractSignableTargetFromResults(ctx, obj)
	for _, obj := range sts {
		splits := strings.Split(obj.Digest, ":")
		if len(splits) != 2 {
			logger.Errorf("Digest %s should be in the format of: algorthm:abc", obj.Digest)
			continue
		}
		subjects = append(subjects, intoto.Subject{
			Name: obj.URI,
			Digest: common.DigestSet{
				splits[0]: splits[1],
			},
		})
	}

	ssts := artifacts.ExtractStructuredTargetFromResults(ctx, obj, artifacts.ArtifactsOutputsResultName)
	for _, s := range ssts {
		splits := strings.Split(s.Digest, ":")
		alg := splits[0]
		digest := splits[1]
		subjects = append(subjects, intoto.Subject{
			Name: s.URI,
			Digest: common.DigestSet{
				alg: digest,
			},
		})
	}

	// Check if object is a Taskrun, if so search for images used in PipelineResources
	// Otherwise object is a PipelineRun, where Pipelineresources are not relevant.
	// PipelineResources have been deprecated so their support has been left out of
	// the POC for TEP-84
	// More info: https://tekton.dev/docs/pipelines/resources/
	tr, ok := obj.GetObject().(*v1beta1.TaskRun)
	if !ok || tr.Spec.Resources == nil {
		return subjects
	}

	// go through resourcesResult
	for _, output := range tr.Spec.Resources.Outputs {
		name := output.Name
		if output.PipelineResourceBinding.ResourceSpec == nil {
			continue
		}
		// similarly, we could do this for other pipeline resources or whatever thing replaces them
		if output.PipelineResourceBinding.ResourceSpec.Type == backport.PipelineResourceTypeImage {
			// get the url and digest, and save as a subject
			var url, digest string
			for _, s := range tr.Status.ResourcesResult {
				if s.ResourceName == name {
					if s.Key == "url" {
						url = s.Value
					}
					if s.Key == "digest" {
						digest = s.Value
					}
				}
			}
			subjects = append(subjects, intoto.Subject{
				Name: url,
				Digest: common.DigestSet{
					"sha256": strings.TrimPrefix(digest, "sha256:"),
				},
			})
		}
	}

	return subjects
}

// RetrieveAllArtifactURIs returns all the URIs of the software artifacts produced from the run object.
// - It first extracts intoto subjects from run object results and converts the subjects
// to a slice of string URIs in the format of "NAME" + "@" + "ALGORITHM" + ":" + "DIGEST".
// - If no subjects could be extracted from results, then an empty slice is returned.
func RetrieveAllArtifactURIs(ctx context.Context, obj objects.TektonObject, deepInspectionEnabled bool) []string {
	result := []string{}
	subjects := SubjectDigests(ctx, obj, &slsaconfig.SlsaConfig{DeepInspectionEnabled: deepInspectionEnabled})

	for _, s := range subjects {
		for algo, digest := range s.Digest {
			result = append(result, fmt.Sprintf("%s@%s:%s", s.Name, algo, digest))
		}
	}
	return result
}
