package container_image_availability

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/openshift/assisted-installer-agent/src/util"
	"github.com/openshift/assisted-service/models"
	"github.com/pkg/errors"

	"github.com/sirupsen/logrus"
)

var (
	Megabyte = math.Pow10(6)
)

const (
	templateInspect  = "podman image inspect --format={{.Size}} %s"
	templatePull     = "podman pull %s"
	templateGetImage = "podman images --quiet %s"
	templateTimeout  = "timeout %s %s"
)

//go:generate mockery -name ImageAvailabilityDependencies -inpkg
type ImageAvailabilityDependencies interface {
	ExecutePrivileged(command string, args ...string) (stdout string, stderr string, exitCode int)
}

type ProcessExecuter struct{}

func (e *ProcessExecuter) ExecutePrivileged(command string, args ...string) (stdout string, stderr string, exitCode int) {
	return util.ExecutePrivileged(command, args...)
}

func executeString(executer ImageAvailabilityDependencies, cmd string) (stdout string, stderr string, exitCode int) {
	args := strings.Split(cmd, " ")
	return executer.ExecutePrivileged(args[0], args[1:]...)
}

func getImageSizeInBytes(executer ImageAvailabilityDependencies, image string) (float64, error) {
	cmd := fmt.Sprintf(templateInspect, image)
	stdout, stderr, exitCode := executeString(executer, cmd)
	if exitCode != 0 {
		return 0, errors.Errorf("podman inspect exited with non-zero exit code %d: %s\n %s", exitCode, stdout, stderr)
	}

	val := strings.TrimSpace(stdout)
	size, err := strconv.ParseFloat(val, 64)
	if err != nil {
		return 0, errors.Wrapf(err, "Failed to convert %s to float", val)
	}

	return size, nil
}

func calcMBps(bytes, nanosecond float64) float64 {
	if nanosecond == 0 {
		return 0
	}

	return (bytes / Megabyte) / (nanosecond / float64(time.Second))
}

func isImageAvailable(executer ImageAvailabilityDependencies, image string) bool {
	cmd := fmt.Sprintf(templateGetImage, image)
	stdout, _, exitCode := executeString(executer, cmd)
	return exitCode == 0 && stdout != ""
}

func pullImage(executer ImageAvailabilityDependencies, pullTimeoutSeconds int, image string) error {
	cmd := fmt.Sprintf(templatePull, image)
	cmd = fmt.Sprintf(templateTimeout, strconv.FormatInt(int64(pullTimeoutSeconds), 10), cmd)
	stdout, stderr, exitCode := executeString(executer, cmd)

	switch exitCode {
	case 0:
		return nil
	case util.TimeoutExitCode:
		return errors.Errorf("podman pull was timed out after %d seconds", pullTimeoutSeconds)
	default:
		return errors.Errorf("podman pull exited with non-zero exit code %d: %s\n %s", exitCode, stdout, stderr)
	}
}

func handleImageAvailability(executer ImageAvailabilityDependencies, log logrus.FieldLogger, pullTimeoutSeconds int, image string) *models.ContainerImageAvailability {
	imageExistLocallyBeforePull := isImageAvailable(executer, image)

	log.Infof("Image exists locally before pull: %s", strconv.FormatBool(imageExistLocallyBeforePull))

	response := &models.ContainerImageAvailability{
		Name:   image,
		Result: models.ContainerImageAvailabilityResultFailure,
	}

	start := time.Now()
	err := pullImage(executer, pullTimeoutSeconds, image)
	diff := float64(time.Since(start))

	if err != nil {
		log.WithError(err).Warnf("Pulling image %s wasn't available", image)
		return response
	}

	if !imageExistLocallyBeforePull {
		log.Infof("Pulling image %s is available. Took %f seconds", image, diff/float64(time.Second))

		sizeInBytes, err := getImageSizeInBytes(executer, image)
		if err != nil {
			log.WithError(err).Warnf("Couldn't get the image size of %s", image)
			return response
		}

		response.SizeBytes = sizeInBytes
		response.Time = diff / float64(time.Second)
		response.DownloadRate = calcMBps(response.SizeBytes, diff)
	}

	response.Result = models.ContainerImageAvailabilityResultSuccess
	return response
}

func Run(requestStr string, executer ImageAvailabilityDependencies, log logrus.FieldLogger) (stdout string, stderr string, exitCode int) {
	var request models.ContainerImageAvailabilityRequest
	var response models.ContainerImageAvailabilityResponse

	err := json.Unmarshal([]byte(requestStr), &request)
	if err != nil {
		log.WithError(err).Errorf("Failed to unmarshal image availability request string %s", requestStr)
		return "", err.Error(), -1
	}

	for _, image := range request.Images {
		response.Images = append(response.Images, handleImageAvailability(executer, log, int(request.Timeout), image))
	}

	b, err := json.Marshal(&response)
	if err != nil {
		log.WithError(err).Errorf("Failed to marshal image availability response %v", response)
		return "", err.Error(), -1
	}
	return string(b), "", 0
}
