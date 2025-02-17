package executehelpers

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/concourse/concourse/atc"
	"github.com/concourse/concourse/fly/commands/internal/flaghelpers"
	"github.com/concourse/concourse/fly/ui/progress"
	"github.com/concourse/concourse/go-concourse/concourse"
	"github.com/vbauerster/mpb/v4"
)

type Input struct {
	Name string
	Path string

	Plan atc.Plan
}

func DetermineInputs(
	fact atc.PlanFactory,
	team concourse.Team,
	taskInputs []atc.TaskInputConfig,
	localInputMappings []flaghelpers.InputPairFlag,
	userInputMappings []flaghelpers.InputMappingPairFlag,
	jobInputImage string,
	inputsFrom flaghelpers.JobFlag,
	includeIgnored bool,
	platform string,
	tags []string,
) ([]Input, map[string]string, *atc.ImageResource, atc.ResourceTypes, error) {
	inputMappings := ConvertInputMappings(userInputMappings)

	err := CheckForUnknownInputMappings(localInputMappings, taskInputs)
	if err != nil {
		return nil, nil, nil, nil, err
	}

	err = CheckForInputType(localInputMappings)
	if err != nil {
		return nil, nil, nil, nil, err
	}

	if inputsFrom.PipelineRef.Name == "" && inputsFrom.JobName == "" {
		wd, err := os.Getwd()
		if err != nil {
			return nil, nil, nil, nil, err
		}

		required := false
		for _, input := range taskInputs {
			if input.Name == filepath.Base(wd) {
				required = true
				break
			}
		}

		provided := false
		for _, input := range localInputMappings {
			if input.Name == filepath.Base(wd) {
				provided = true
				break
			}
		}

		if required && !provided {
			localInputMappings = append(localInputMappings, flaghelpers.InputPairFlag{
				Name: filepath.Base(wd),
				Path: ".",
			})
		}
	}

	inputsFromLocal, err := GenerateLocalInputs(fact, team, localInputMappings, includeIgnored, platform, tags)
	if err != nil {
		return nil, nil, nil, nil, err
	}

	inputsFromJob, imageResourceFromJob, resourceTypes, err := FetchInputsFromJob(fact, team, inputsFrom, jobInputImage)
	if err != nil {
		return nil, nil, nil, nil, err
	}

	inputs := []Input{}
	for _, taskInput := range taskInputs {
		input, found := inputsFromLocal[taskInput.Name]
		if !found {

			jobInputName := taskInput.Name
			if name, ok := inputMappings[taskInput.Name]; ok {
				jobInputName = name
			}

			input, found = inputsFromJob[jobInputName]
			if !found {
				if taskInput.Optional {
					continue
				} else {
					return nil, nil, nil, nil, fmt.Errorf("missing required input `%s`", taskInput.Name)
				}
			}
		}

		inputs = append(inputs, input)
	}

	return inputs, inputMappings, imageResourceFromJob, resourceTypes, nil
}

func ConvertInputMappings(variables []flaghelpers.InputMappingPairFlag) map[string]string {
	inputMappings := map[string]string{}
	for _, flag := range variables {
		inputMappings[flag.Name] = flag.Value
	}
	return inputMappings
}

func CheckForInputType(inputMaps []flaghelpers.InputPairFlag) error {
	for _, i := range inputMaps {
		if i.Path != "" {
			fi, err := os.Stat(i.Path)
			if err != nil {
				return err
			}
			switch mode := fi.Mode(); {
			case mode.IsRegular():
				return errors.New(i.Path + " not a folder")
			}
		}
	}
	return nil
}

func CheckForUnknownInputMappings(inputMappings []flaghelpers.InputPairFlag, validInputs []atc.TaskInputConfig) error {
	for _, inputMapping := range inputMappings {
		if !TaskInputsContainsName(validInputs, inputMapping.Name) {
			return fmt.Errorf("unknown input `%s`", inputMapping.Name)
		}
	}
	return nil
}

func TaskInputsContainsName(inputs []atc.TaskInputConfig, name string) bool {
	for _, input := range inputs {
		if input.Name == name {
			return true
		}
	}
	return false
}

func GenerateLocalInputs(
	fact atc.PlanFactory,
	team concourse.Team,
	inputMappings []flaghelpers.InputPairFlag,
	includeIgnored bool,
	platform string,
	tags []string,
) (map[string]Input, error) {
	inputs := map[string]Input{}

	artifacts := new(sync.Map)

	prog := progress.New()

	for _, mapping := range inputMappings {
		name := mapping.Name
		path := mapping.Path

		prog.Go("uploading "+name, func(bar *mpb.Bar) error {
			artifact, err := Upload(bar, team, path, includeIgnored, platform, tags)
			if err != nil {
				return err
			}

			artifacts.Store(name, artifact)

			return nil
		})
	}

	err := prog.Wait()
	if err != nil {
		return nil, err
	}

	for _, mapping := range inputMappings {
		val, _ := artifacts.Load(mapping.Name)

		inputs[mapping.Name] = Input{
			Name: mapping.Name,
			Path: mapping.Path,
			Plan: fact.NewPlan(atc.ArtifactInputPlan{
				ArtifactID: val.(atc.WorkerArtifact).ID,
				Name:       mapping.Name,
			}),
		}
	}

	return inputs, nil
}

func FetchInputsFromJob(fact atc.PlanFactory, team concourse.Team, inputsFrom flaghelpers.JobFlag, imageName string) (map[string]Input, *atc.ImageResource, atc.ResourceTypes, error) {
	kvMap := map[string]Input{}

	if inputsFrom.PipelineRef.Name == "" && inputsFrom.JobName == "" {
		return kvMap, nil, nil, nil
	}

	buildInputs, found, err := team.BuildInputsForJob(inputsFrom.PipelineRef, inputsFrom.JobName)
	if err != nil {
		return nil, nil, nil, err
	}

	if !found {
		return nil, nil, nil, fmt.Errorf("build inputs for %s/%s not found", inputsFrom.PipelineRef.String(), inputsFrom.JobName)
	}

	resourceTypes, found, err := team.ResourceTypes(inputsFrom.PipelineRef)
	if err != nil {
		return nil, nil, nil, err
	}

	if !found {
		return nil, nil, nil, fmt.Errorf("resource types of %s not found", inputsFrom.PipelineRef.String())
	}

	var imageResource *atc.ImageResource
	if imageName != "" {
		imageResource, found, err = FetchImageResourceFromJobInputs(buildInputs, imageName)
		if err != nil {
			return nil, nil, nil, err
		}

		if !found {
			return nil, nil, nil, fmt.Errorf("image resource %s not found", imageName)
		}
	}

	for _, buildInput := range buildInputs {
		version := buildInput.Version

		plan := fact.NewPlan(atc.GetPlan{
			Name:    buildInput.Name,
			Type:    buildInput.Type,
			Source:  buildInput.Source,
			Version: &version,
			Params:  buildInput.Params,
			Tags:    buildInput.Tags,
		})
		plan.Get.TypeImage = resourceTypes.ImageForType(plan.ID, buildInput.Type, buildInput.Tags, false)
		kvMap[buildInput.Name] = Input{
			Name: buildInput.Name,

			Plan: plan,
		}
	}

	return kvMap, imageResource, resourceTypes, nil
}

func FetchImageResourceFromJobInputs(inputs []atc.BuildInput, imageName string) (*atc.ImageResource, bool, error) {
	for _, input := range inputs {
		if input.Name == imageName {
			version := input.Version
			imageResource := atc.ImageResource{
				Type:    input.Type,
				Source:  input.Source,
				Version: version,
				Params:  input.Params,
			}
			return &imageResource, true, nil
		}
	}

	return nil, false, nil
}
