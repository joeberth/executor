package executor

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/dadosjusbr/executor/status"
)

const (
	noExitError   = -2
	output        = "output"
	dirPermission = 0666
)

// Stage is a phase of data release process.
type Stage struct {
	Name     string            // Stage's name.
	Dir      string            // Directory to be concatenated with default base directory or with the base directory specified here in 'BaseDir'. This field is used to name the image built.
	BaseDir  string            // Base directory for the stage. This field overwrites the DefaultBaseDir in pipeline's definition.
	BuildEnv map[string]string // Variables to be used in the stage build. They will be concatenated with the default variables defined in the pipeline, overwriting them if repeated.
	RunEnv   map[string]string // Variables to be used in the stage run. They will be concatenated with the default variables defined in the pipeline, overwriting them if repeated.
}

// Pipeline represents the sequence of stages for data release.
type Pipeline struct {
	Name            string            // Pipeline's name.
	DefaultBaseDir  string            // Default base directory to be used in all stages.
	DefaultBuildEnv map[string]string // Default variables to be used in the build of all stages.
	DefaultRunEnv   map[string]string // Default variables to be used in the run of all stages.
	Stages          []Stage           // Confguration for the pipeline's stages.
	ErrorHandler    Stage             // Default stage to deal with any errors that occur in the execution of the pipeline.
}

// CmdResult represents information about a execution of a command.
type CmdResult struct {
	Stdin      string   `json:"stdin" bson:"stdin,omitempt"`    // String containing the standard input of the process.
	Stdout     string   `json:"stdout" bson:"stdout,omitempty"` // String containing the standard output of the process.
	Stderr     string   `json:"stderr" bson:"stderr,omitempty"` // String containing the standard error of the process.
	Cmd        string   `json:"cmd" bson:"cmd,omitempty"`       // Command that has been executed.
	CmdDir     string   `json:"cmdDir" bson:"cmdir,omitempty"`  // Local directory, in which the command has been executed.
	ExitStatus int      `json:"status" bson:"status,omitempty"` // Exit code of the process executed.
	Env        []string `json:"env" bson:"env,omitempty"`       // Copy of strings representing the environment variables in the form ke=value.
}

// StageExecutionResult represents information about the execution of a stage.
type StageExecutionResult struct {
	Stage       string    `json:"stage" bson:"stage,omitempty"`             // Name of stage.
	StartTime   time.Time `json:"start" bson:"start,omitempty"`             // Time at start of stage.
	FinalTime   time.Time `json:"end" bson:"end,omitempty"`                 // Time at the end of stage.
	BuildResult CmdResult `json:"buildResult" bson:"buildResult,omitempty"` // Build result.
	RunResult   CmdResult `json:"runResult" bson:"runResult,omitempty"`     // Run result.
}

// PipelineResult represents the pipeline information and their results.
type PipelineResult struct {
	Name         string                 `json:"name" bson:"name,omitempty"`               // Name of pipeline.
	StageResults []StageExecutionResult `json:"stageResult" bson:"stageResult,omitempty"` // Results of stage execution.
	StartTime    time.Time              `json:"start" bson:"start,omitempty"`             // Time at start of pipeline.
	FinalTime    time.Time              `json:"final" bson:"final,omitempty"`             // Time at end of pipeline.
	Status       string                 `json:"status" bson:"status,omitempty"`           // Pipeline execution status(OK, RunError, BuildError, SetupError...).
}

func setup(baseDir string) error {
	finalPath := fmt.Sprintf("%s/%s", baseDir, output)
	if err := os.RemoveAll(finalPath); err != nil {
		return fmt.Errorf("error removing existing output folder: %q", err)
	}

	if err := os.Mkdir(finalPath, dirPermission); err != nil {
		return fmt.Errorf("error creating output folder: %q", err)
	}

	cmdList := strings.Split(fmt.Sprintf("docker volume create --driver local --opt type=none --opt device=%s --opt o=bind --name=dadosjusbr", finalPath), " ")
	cmd := exec.Command(cmdList[0], cmdList[1:]...)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("error creating volume dadosjusbr: %q", err)
	}

	return nil
}

func tearDown() error {
	cmdList := strings.Split("docker volume rm -f dadosjusbr", " ")
	cmd := exec.Command(cmdList[0], cmdList[1:]...)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("error removing existing volume dadosjusbr: %q", err)
	}

	return nil
}

// Run executes the pipeline.
// For each stage defined in the pipeline we execute the `docker build` and
// `docker run`. If any of these two processes fail, we interrupt the flow
// and the error handler is called. Here, we consider a failure when the
// building or execution of the image returns a status other than 0 or
// when an error is raised within the buildImage or runImage functions.
//
// The error handler can be defined as a stage, but it will only be executed in
// case of an error in the pipeline standard flow, which is when we call the
// function handleError.
//
// When handleError is called we pass all informations about the pipeline
// execution until that point, which are:
// - the PipelineResult (until current stage),
// - the StageResult (from current stage),
// - error status and error message
// - the stage ErrorHandler (defined in the Pipeline).
// Thereby the error handler will able to process or store the problem that
// occurred in the current stage. The function runImage for stage ErrorHandler
// receives the StageResult as STDIN.
// If there are any errors in the execution of the error handler,
// the processing is completely stopped and the error is returned.
//
// If a specific error handler has not been defined, the default behavior is to
// return the error message that occurred in the standard flow along with the
// structure that describes all the pipeline execution information until that point.
func (p *Pipeline) Run() (PipelineResult, error) {
	result := PipelineResult{Name: p.Name, StartTime: time.Now()}

	if err := setup(p.DefaultBaseDir); err != nil {
		result.Status = status.Text(status.SetupError)
		return result, status.NewError(status.SetupError, fmt.Errorf("error in inicial setup: %q", err))
	}

	for index, stage := range p.Stages {
		var ser StageExecutionResult
		var err error
		ser.Stage = stage.Name
		ser.StartTime = time.Now()

		if len(stage.BaseDir) == 0 {
			stage.BaseDir = p.DefaultBaseDir
		}
		dir := fmt.Sprintf("%s/%s", stage.BaseDir, stage.Dir)

		id := fmt.Sprintf("%s/%s", p.Name, stage.Name)
		// 'index+1' because the index starts from 0.
		log.Printf("Executing Pipeline %s [%d/%d]\n", id, index+1, len(p.Stages))

		stage.BuildEnv = mergeEnv(p.DefaultBuildEnv, stage.BuildEnv)
		ser.BuildResult, err = buildImage(id, dir, stage.BuildEnv)
		if err != nil {
			m := fmt.Sprintf("error when building image: %s", err)
			return handleError(&result, ser, status.BuildError, m, p.ErrorHandler)
		}
		if status.Code(ser.BuildResult.ExitStatus) != status.OK {
			m := fmt.Sprintf("error when building image: status code %d(%s) when building image for %s", ser.BuildResult.ExitStatus, status.Text(status.Code(ser.BuildResult.ExitStatus)), id)
			return handleError(&result, ser, status.BuildError, m, p.ErrorHandler)

		}
		log.Println("Image built sucessfully!")

		stdout := ""
		if index != 0 {
			// 'index-1' is accessing the output from previous stage.
			stdout = result.StageResults[index-1].RunResult.Stdout
		}

		stage.RunEnv = mergeEnv(p.DefaultRunEnv, stage.RunEnv)
		ser.RunResult, err = runImage(id, dir, stdout, stage.RunEnv)
		if err != nil {
			m := fmt.Sprintf("error when running image: %s", err)
			return handleError(&result, ser, status.RunError, m, p.ErrorHandler)
		}
		if status.Code(ser.RunResult.ExitStatus) != status.OK {
			m := fmt.Sprintf("error when running image: Status code %d(%s) when running image for %s", ser.RunResult.ExitStatus, status.Text(status.Code(ser.RunResult.ExitStatus)), id)
			return handleError(&result, ser, status.RunError, m, p.ErrorHandler)
		}
		log.Printf("Image executed successfully!\n\n")

		ser.FinalTime = time.Now()
		result.StageResults = append(result.StageResults, ser)
	}

	if err := tearDown(); err != nil {
		result.Status = status.Text(status.SetupError)
		return result, status.NewError(status.SetupError, fmt.Errorf("error in tear down: %q", err))
	}

	result.Status = status.Text(status.OK)
	result.FinalTime = time.Now()

	return result, nil
}

// handleError is responsible for build and run the stage ErrorHandler
// defined in the Pipeline. It is called when occurs any error in
// pipeline standard flow. If a specific error handler has not been
// defined, the default behavior is to return the PipelineResult until
// the last stage executed and the error occurred.
//
// When handleError is called it receives all informations about the pipeline
// execution until that point, which are:
// - the PipelineResult (until current stage),
// - the StageResult (from current stage),
// - error status and error message
// - the stage ErrorHandler (defined in the Pipeline).
// Thereby the error handler will able to process or store the problem that
// occurred in the current stage. The function runImage for stage ErrorHandler
// receives the StageResult as STDIN.
//
// If there are any errors in the execution of the error handler,
// the processing is completely stopped and the error is returned.
func handleError(result *PipelineResult, previousSer StageExecutionResult, previousStatus status.Code, msg string, handler Stage) (PipelineResult, error) {
	previousSer.FinalTime = time.Now()
	result.StageResults = append(result.StageResults, previousSer)

	if handler.Dir != "" {
		var serError StageExecutionResult
		var err error
		serError.Stage = handler.Name
		serError.StartTime = time.Now()

		id := fmt.Sprintf("%s/%s calls Error Handler", result.Name, previousSer.Stage)
		serError.BuildResult, err = buildImage(id, handler.Dir, handler.BuildEnv)
		if err != nil {
			result.StageResults = append(result.StageResults, serError)
			result.Status = status.Text(status.ErrorHandlerError)
			result.FinalTime = time.Now()

			return *result, status.NewError(status.BuildError, fmt.Errorf("error when building image for error handler: %s", err))
		}
		if status.Code(serError.BuildResult.ExitStatus) != status.OK {
			result.StageResults = append(result.StageResults, serError)
			result.Status = status.Text(status.ErrorHandlerError)
			result.FinalTime = time.Now()

			return *result, status.NewError(status.BuildError, fmt.Errorf("error when building image for error handler: Status code %d(%s) when running image for %s", serError.BuildResult.ExitStatus, status.Text(status.Code(serError.BuildResult.ExitStatus)), id))
		}

		erStdin, err := json.Marshal(previousSer)
		if err != nil {
			return *result, status.NewError(status.ErrorHandlerError, fmt.Errorf("error in parser StageExecutionResult for error handler: %s", err))
		}
		serError.RunResult, err = runImage(id, handler.Dir, string(erStdin), handler.RunEnv)
		if err != nil {
			result.StageResults = append(result.StageResults, serError)
			result.Status = status.Text(status.ErrorHandlerError)
			result.FinalTime = time.Now()

			return *result, status.NewError(status.RunError, fmt.Errorf("error when running image for error handler: %s", err))
		}
		if status.Code(serError.RunResult.ExitStatus) != status.OK {
			result.StageResults = append(result.StageResults, serError)
			result.Status = status.Text(status.ErrorHandlerError)
			result.FinalTime = time.Now()

			return *result, status.NewError(status.RunError, fmt.Errorf("error when running image for error handler: Status code %d(%s) when running image for %s", serError.RunResult.ExitStatus, status.Text(status.Code(serError.RunResult.ExitStatus)), id))
		}
	}

	result.Status = status.Text(previousStatus)
	result.FinalTime = time.Now()
	return *result, fmt.Errorf(msg)
}

func mergeEnv(defaultEnv, stageEnv map[string]string) map[string]string {
	env := make(map[string]string)

	for k, v := range defaultEnv {
		env[k] = v
	}
	for k, v := range stageEnv {
		env[k] = v
	}
	return env
}

// buildImage executes the 'docker build' for a image, considering the
// parameters defined for it and returns a CmdResult and an error, if any.
func buildImage(id, dir string, buildEnv map[string]string) (CmdResult, error) {
	log.Printf("Building image for %s", id)

	var b strings.Builder
	for k, v := range buildEnv {
		fmt.Fprintf(&b, "--build-arg %s=%s ", k, v)
	}
	env := b.String()

	cmdList := strings.Split(fmt.Sprintf("docker build %s-t %s .", env, filepath.Base(dir)), " ")
	cmd := exec.Command(cmdList[0], cmdList[1:]...)
	cmd.Dir = dir
	var outb, errb bytes.Buffer
	cmd.Stdout = &outb
	cmd.Stderr = &errb

	log.Printf("$ %s", strings.Join(cmdList, " "))
	err := cmd.Run()
	switch err.(type) {
	case *exec.Error:
		cmdResultError := CmdResult{
			ExitStatus: statusCode(err),
			Cmd:        strings.Join(cmdList, " "),
		}
		return cmdResultError, fmt.Errorf("command was not executed correctly: %s", err)
	}

	cmdResult := CmdResult{
		Stdout:     string(outb.Bytes()),
		Stderr:     string(errb.Bytes()),
		Cmd:        strings.Join(cmdList, " "),
		CmdDir:     dir,
		ExitStatus: statusCode(err),
		Env:        os.Environ(),
	}

	return cmdResult, nil
}

// statusCode returns the exit code returned for the cmd execution.
// 0 if no error.
// -1 if process was terminated by a signal or hasn't started.
// -2 if error is not an ExitError.
func statusCode(err error) int {
	if err == nil {
		return 0
	}
	if exitError, ok := err.(*exec.ExitError); ok {
		return exitError.ExitCode()
	}
	return noExitError
}

// runImage executes the 'docker run' for a image, considering the
// parameters defined for it and returns a CmdResult and an error, if any.
// It uses the stdout from the previous stage as the stdin for this new command.
func runImage(id, dir, previousStdout string, runEnv map[string]string) (CmdResult, error) {
	log.Printf("Running image for %s", id)

	var builder strings.Builder
	for key, value := range runEnv {
		fmt.Fprintf(&builder, "--env %s=%s ", key, value)
	}
	env := strings.TrimRight(builder.String(), " ")

	cmdList := strings.Split(fmt.Sprintf("docker run -i -v dadosjusbr:/output --rm %s %s", env, filepath.Base(dir)), " ")
	cmd := exec.Command(cmdList[0], cmdList[1:]...)
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader(previousStdout)
	var outb, errb bytes.Buffer
	cmd.Stdout = &outb
	cmd.Stderr = &errb

	log.Printf("$ %s", strings.Join(cmdList, " "))
	err := cmd.Run()
	switch err.(type) {
	case *exec.Error:
		cmdResultError := CmdResult{
			ExitStatus: statusCode(err),
			Cmd:        strings.Join(cmdList, " "),
		}
		return cmdResultError, fmt.Errorf("command was not executed correctly: %s", err)
	}

	cmdResult := CmdResult{
		Stdin:      previousStdout,
		Stdout:     string(outb.Bytes()),
		Stderr:     string(errb.Bytes()),
		Cmd:        strings.Join(cmdList, " "),
		CmdDir:     cmd.Dir,
		ExitStatus: statusCode(err),
		Env:        os.Environ(),
	}

	return cmdResult, nil
}
