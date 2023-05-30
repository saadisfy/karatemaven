package runner

import (
	"context"
	"fmt"
	"github.com/kubeshop/testkube/contrib/executor/jmeter/pkg/parser"
	"github.com/kubeshop/testkube/pkg/executor/env"
	"os"
	"path/filepath"

	"github.com/kubeshop/testkube/pkg/executor/scraper"

	"github.com/pkg/errors"

	"github.com/kubeshop/testkube/pkg/executor/scraper/factory"

	"github.com/kubeshop/testkube/pkg/api/v1/testkube"
	"github.com/kubeshop/testkube/pkg/envs"
	"github.com/kubeshop/testkube/pkg/executor"
	"github.com/kubeshop/testkube/pkg/executor/output"
	"github.com/kubeshop/testkube/pkg/executor/runner"
	"github.com/kubeshop/testkube/pkg/ui"
)

func NewKarateRunner(ctx context.Context, dependency string, params envs.Params) (*KarateRunner, error) {
	//TODO - check if the KarateRunner even needs all three attributes

	output.PrintLogf("%s Preparing test runner", ui.IconTruck)
	var err error
	r := &KarateRunner{
		Params:     params,
		dependency: dependency,
	}
	r.Scraper, err = factory.TryGetScrapper(ctx, params)
	if err != nil {
		return nil, err
	}

	return r, nil

}

// KarateRunner - implements runner interface used in worker to start test execution
type KarateRunner struct {
	Params     envs.Params
	Scraper    scraper.Scraper //responsible for collecting and persisting the execution artifacts
	dependency string
}

func (r *KarateRunner) Run(ctx context.Context, execution testkube.Execution) (result testkube.ExecutionResult, err error) {
	if r.Scraper != nil {
		defer r.Scraper.Close() //invoke after return
	}
	output.PrintLogf("%s Preparing for test run", ui.IconTruck)

	err = r.Validate(execution)
	if err != nil {
		return result, err
	}

	// check that the datadir exists
	_, err = os.Stat(r.Params.DataDir)
	if errors.Is(err, os.ErrNotExist) {
		output.PrintLogf("%s Datadir %s does not exist", ui.IconCross, r.Params.DataDir)
		return result, errors.Errorf("datadir not exist: %v", err)
	}

	runPath := filepath.Join(r.Params.DataDir, "repo", execution.Content.Repository.Path)
	projectPath := runPath
	if execution.Content.Repository != nil && execution.Content.Repository.WorkingDir != "" {
		runPath = filepath.Join(r.Params.DataDir, "repo", execution.Content.Repository.WorkingDir)
	}
	fileInfo, err := os.Stat(projectPath)
	if err != nil {
		return result, err
	}

	if !fileInfo.IsDir() {
		output.PrintLogf("%s Using file...", ui.IconTruck)

		// TODO single file test execution through direct execution
		output.PrintLogf("%s Passing Karate test as single file not implemented yet", ui.IconCross)
		return result, errors.Errorf("passing Karate test as single file not implemented yet")
	}
	output.PrintLogf("%s Test content checked", ui.IconCheckMark)

	//TODO -  Dependency installation if needed?
	/*out, err := r.installDependencies(runPath)
	if err != nil {
		return result, errors.Errorf("cypress binary install error: %v\n\n%s", err, out)
	}*/
	//TODO - local version install needed? --> binary install

	envManager := env.NewManagerWithVars(execution.Variables)
	envManager.GetReferenceVars(envManager.Variables)
	envVars := make([]string, 0, len(envManager.Variables))
	for _, value := range envManager.Variables {
		if !value.IsSecret() {
			output.PrintLogf("%s=%s", value.Name, value.Value)
		}
		envVars = append(envVars, fmt.Sprintf("%s=%s", value.Name, value.Value))
	}
	//run tests while ignoring execution error in case of failed tests

	output.PrintLogf("%s: DebugMode projectPath = "+projectPath, ui.IconMicroscope) //todo delete this line
	output.PrintLogf("%s: DebugMode runPath = "+runPath, ui.IconMicroscope)         //todo delete this line
	out, err := executor.Run(runPath, "mvn test", envManager)
	out = envManager.ObfuscateSecrets(out)

	result = MapResultsToExecutionResults(out)

	if r.Params.ScrapperEnabled {
		if err = scrapeArtifacts(ctx, r, execution); err != nil {
			return result, err
		}
	}

	return result, err
}

func scrapeArtifacts(ctx context.Context, r *KarateRunner, execution testkube.Execution) (err error) {

	reportPath := filepath.Join(r.Params.DataDir, "repo", execution.Content.Repository.Path, "target")

	originalName := "target"
	compressedName := originalName + "-zip"

	if _, err := executor.Run(reportPath, "mkdir", nil, compressedName); err != nil {
		output.PrintLogf("%s Artifact scraping failed: making dir %s", ui.IconCross, compressedName)
		return errors.Errorf("mkdir error: %v", err)
	}

	if _, err := executor.Run(reportPath, "zip", nil, compressedName+"/"+originalName+".zip", "-r", originalName); err != nil {
		output.PrintLogf("%s Artifact scraping failed: zipping reports %s", ui.IconCross, originalName)
		return errors.Errorf("zip error: %v", err)
	}

	directories := []string{
		filepath.Join(reportPath, compressedName),
	}
	output.PrintLogf("Scraping directories: %v", directories)

	if err := r.Scraper.Scrape(ctx, directories, execution); err != nil {
		return errors.Wrap(err, "error scraping artifacts")
	}

	return nil
}

// GetType returns runner type
func (r *KarateRunner) GetType() runner.Type {
	return runner.TypeMain
}

/*
Validate checks if Execution has valid data in context of Karate executor.
validates by checking if they are nil or empty
Karate executor runs currently only based on Maven currently
*/
func (r *KarateRunner) Validate(execution testkube.Execution) error {

	if execution.Content == nil {
		output.PrintLogf("%s Invalid input: can't find any content to run in execution data", ui.IconCross)
		return errors.Errorf("can't find any content to run in execution data: %+v", execution)
	}

	if execution.Content.Repository == nil {
		output.PrintLogf("%s Karate executor handles only repository based tests, but repository is nil", ui.IconCross)
		return errors.Errorf("Karate executor handles only repository based tests, but repository is nil")
	}

	if execution.Content.Repository.Branch == "" && execution.Content.Repository.Commit == "" {
		output.PrintLogf("%s can't find branch or commit in params, repo:%+v", ui.IconCross, execution.Content.Repository)
		return errors.Errorf("can't find branch or commit in params, repo:%+v", execution.Content.Repository)
	}

	return nil
}

func (r *KarateRunner) installDependencies(runPath string) (out []byte, err error) {
	if r.dependency == "maven" {
		out, dependencyError := executor.Run(runPath, "mvn", nil, "install")
		if dependencyError != nil {
			return nil, errors.Errorf("mvn install error: %v\n\n%s", dependencyError, out)
		}
	}
	//TODO - other dependecies: npm, direct file based
	return nil, errors.Errorf("Other dependencies than Maven are not supported yet")
}

func MapResultsToExecutionResults(out []byte) (result testkube.ExecutionResult) {
	result.Status = testkube.ExecutionStatusPassed
	//TODO - copies from jmeter mapping, write your own
	/*if results.HasError {
		result.Status = testkube.ExecutionStatusFailed
		result.ErrorMessage = results.LastErrorMessage
	}

	result.Output = string(out)
	result.OutputType = "text/plain"

	for _, r := range results.Results {
		result.Steps = append(
			result.Steps,
			testkube.ExecutionStepResult{
				Name:     r.Label,
				Duration: r.Duration.String(),
				Status:   MapStatus(r),
				AssertionResults: []testkube.AssertionResult{{
					Name:   r.Label,
					Status: MapStatus(r),
				}},
			})
	}*/

	return result
}

func MapStatus(result parser.Result) string {
	if result.Success {
		return string(testkube.PASSED_ExecutionStatus)
	}

	return string(testkube.FAILED_ExecutionStatus)
}
