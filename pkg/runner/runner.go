package runner

import (
	"context"
	"fmt"
	"github.com/joshdk/go-junit"
	"github.com/kubeshop/testkube/pkg/executor/env"
	"os"
	"os/user"
	"path/filepath"
	"strings"

	"github.com/kubeshop/testkube/pkg/executor/scraper"

	"github.com/pkg/errors"

	outputPkg "github.com/kubeshop/testkube/pkg/executor/output"
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

func (karateRunner *KarateRunner) Run(ctx context.Context, execution testkube.Execution) (result testkube.ExecutionResult, err error) {
	if karateRunner.Scraper != nil {
		defer karateRunner.Scraper.Close() //invoke after return
	}
	output.PrintLogf("%s Preparing for test run", ui.IconTruck)

	err = karateRunner.Validate(execution)
	if err != nil {
		return result, err
	}

	// check that the datadir exists
	_, err = os.Stat(karateRunner.Params.DataDir)
	if errors.Is(err, os.ErrNotExist) {
		output.PrintLogf("%s Datadir %s does not exist", ui.IconCross, karateRunner.Params.DataDir)
		return result, errors.Errorf("datadir not exist: %v", err)
	}

	runPath := filepath.Join(karateRunner.Params.DataDir, "repo", execution.Content.Repository.Path)

	projectPath := runPath

	// check that pom.xml file exists
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

	pomXmlPath := filepath.Join(projectPath, "pom.xml")

	_, pomXmlErr := os.Stat(pomXmlPath)
	if errors.Is(pomXmlErr, os.ErrNotExist) {
		outputPkg.PrintLog(fmt.Sprintf("%s No pom.xml found", ui.IconCross))
		return *result.Err(fmt.Errorf("no pom.xml found")), nil
	}

	//TODO -  Dependency installation if needed?
	/*out, err := karateRunner.installDependencies(runPath)
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
	// pass additional executor arguments/flags to maven
	args := []string{}
	args = append(args, execution.Args...)

	if execution.VariablesFile != "" {
		outputPkg.PrintLog(fmt.Sprintf("%s Creating settings.xml file", ui.IconWorld))
		settingsXML, err := createSettingsXML(projectPath, execution.VariablesFile)
		if err != nil {
			outputPkg.PrintLog(fmt.Sprintf("%s Could not create settings.xml", ui.IconCross))
			return *result.Err(fmt.Errorf("could not create settings.xml")), nil
		}
		outputPkg.PrintLog(fmt.Sprintf("%s Successfully created settings.xml", ui.IconCheckMark))
		args = append(args, "--settings", settingsXML)
	}

	// workaround for https://github.com/eclipse/che/issues/13926
	os.Unsetenv("MAVEN_CONFIG")
	currentUser, err := user.Current()
	if err == nil && currentUser.Name == "maven" {
		args = append(args, "-Duser.home=/home/maven")
	}

	if execution.Content.Repository != nil && execution.Content.Repository.WorkingDir != "" {
		runPath = filepath.Join(karateRunner.Params.DataDir, "repo", execution.Content.Repository.WorkingDir)
	}

	mavenCommand := "mvn"
	goal := "test"
	if !strings.EqualFold(goal, "project") {
		// use the test subtype as goal or phase when != project
		// in case of project there is need to pass additional args
		args = append(args, goal)
	}

	//run tests while ignoring execution error in case of failed tests
	for i := 0; i < len(args); i++ {
		output.PrintLogf("%s: DebugMode args_%n = "+args[i], ui.IconMicroscope, i) //todo delete this line
	}

	output.PrintLogf("%s: DebugMode projectPath = "+projectPath, ui.IconMicroscope) //todo delete this line
	output.PrintLogf("%s: DebugMode runPath = "+runPath, ui.IconMicroscope)         //todo delete this line
	out, err := executor.Run(runPath, mavenCommand, envManager, args...)
	out = envManager.ObfuscateSecrets(out)

	if karateRunner.Params.ScrapperEnabled {
		if err = scrapeArtifacts(ctx, karateRunner, execution); err != nil {
			return result, err
		}
	}

	result = MapResultsToExecutionResults(out, err)

	result.Output = string(out)
	result.OutputType = "text/plain"

	junitReportPath := filepath.Join(projectPath, "target", "surefire-reports")
	err = filepath.Walk(junitReportPath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() && filepath.Ext(path) == ".xml" {
			suites, _ := junit.IngestFile(path)
			for _, suite := range suites {
				for _, test := range suite.Tests {
					result.Steps = append(
						result.Steps,
						testkube.ExecutionStepResult{
							Name:     fmt.Sprintf("%s - %s", suite.Name, test.Name),
							Duration: test.Duration.String(),
							Status:   MapStatus(test.Status),
						})
				}
			}
		}

		return nil
	})

	if err != nil {
		return *result.Err(err), nil
	}

	return result, err
}

func scrapeArtifacts(ctx context.Context, r *KarateRunner, execution testkube.Execution) (err error) {
	reportDirName := "surefire-reports"
	compressedDirName := reportDirName + "-zip"

	projectPath := filepath.Join(r.Params.DataDir, "repo", execution.Content.Repository.Path)

	surefireReportPath := filepath.Join(projectPath, "target", reportDirName)
	compressedDirPath := filepath.Join(projectPath, compressedDirName)

	if _, err := executor.Run(projectPath, "mkdir", nil, compressedDirPath); err != nil {
		output.PrintLogf("%s Artifact scraping failed: making dir %s", ui.IconCross, compressedDirPath)
		return errors.Errorf("mkdir error: %v", err)
	}

	if _, err := executor.Run(projectPath, "ls", nil); err != nil {
		output.PrintLogf("%s command ls failed", ui.IconCross)
		return errors.Errorf("ls error: %v", err)
	}

	output.PrintLogf("Zipping reportPath: %v", surefireReportPath)

	zipCommand := "zip"
	//zipArgument := "-r \"" + compressedDirPath + "/" + reportDirName + ".zip\"  \"" + surefireReportPath + "\"" //quoting both paths for arguments
	if _, err := executor.Run(projectPath, zipCommand, nil, compressedDirPath+"/"+reportDirName+".zip", "-r", surefireReportPath); err != nil {
		output.PrintLogf("%s Artifact scraping failed: zipping reports %s", ui.IconCross, surefireReportPath)
		return errors.Errorf("zip error: %v", err)
	}

	directories := []string{
		compressedDirPath,
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

func MapResultsToExecutionResults(out []byte, err error) (result testkube.ExecutionResult) {
	if err == nil {
		result.Status = testkube.ExecutionStatusPassed
		outputPkg.PrintLog(fmt.Sprintf("%s Test run successful", ui.IconCheckMark))
	} else {
		result.Status = testkube.ExecutionStatusFailed
		result.ErrorMessage = err.Error()
		outputPkg.PrintLog(fmt.Sprintf("%s Test run failed: %s", ui.IconCross, err.Error()))
		if strings.Contains(result.ErrorMessage, "exit status 1") {
			// probably some tests have failed
			result.ErrorMessage = "build failed with an exception"
		} else {
			// maven was unable to run at all
			return result
		}
	}
	return result
}

func MapStatus(in junit.Status) string {
	switch string(in) {
	case "passed":
		return string(testkube.PASSED_ExecutionStatus)
	default:
		return string(testkube.FAILED_ExecutionStatus)
	}
}

// createSettingsXML saves the settings.xml to maven config folder and adds it to the list of arguments
func createSettingsXML(directory string, content string) (string, error) {
	settingsXML := filepath.Join(directory, "settings.xml")
	err := os.WriteFile(settingsXML, []byte(content), 0644)
	if err != nil {
		return "", fmt.Errorf("could not create settings.xml: %w", err)
	}

	return settingsXML, nil
}
