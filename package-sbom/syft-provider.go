package package_sbom

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/deepfence/package-scanner/output"
	"github.com/deepfence/package-scanner/util"
	"github.com/deepfence/vessel"
	vesselConstants "github.com/deepfence/vessel/constants"
	containerdRuntime "github.com/deepfence/vessel/containerd"
	crioRuntime "github.com/deepfence/vessel/crio"
	dockerRuntime "github.com/deepfence/vessel/docker"
	log "github.com/sirupsen/logrus"
)

var (
	linuxExcludeDirs = []string{"/var/lib/docker", "/var/lib/containerd", "/var/lib/containers", "/var/lib/crio", "/var/run/containers", "/mnt", "/run", "/proc", "/dev", "/boot", "/home/kubernetes/containerized_mounter", "/sys", "/lost+found"}
	mntDirs          = getNfsMountsDirs()
)

type ContainerScan struct {
	containerId string
	tempDir     string
	namespace   string
}

func (containerScan *ContainerScan) exportFileSystemTar() error {
	// Auto-detect underlying container runtime
	containerRuntime, endpoint, err := vessel.AutoDetectRuntime()
	if err != nil {
		return err
	}
	var containerRuntimeInterface vessel.Runtime
	switch containerRuntime {
	case vesselConstants.DOCKER:
		containerRuntimeInterface = dockerRuntime.New()
	case vesselConstants.CONTAINERD:
		containerRuntimeInterface = containerdRuntime.New(endpoint)
	case vesselConstants.CRIO:
		containerRuntimeInterface = crioRuntime.New(endpoint)
	}
	if containerRuntimeInterface == nil {
		fmt.Println("Error: Could not detect container runtime")
		os.Exit(1)
	}

	err = containerRuntimeInterface.ExtractFileSystemContainer(
		containerScan.containerId, containerScan.namespace,
		containerScan.tempDir+".tar", endpoint,
	)

	if err != nil {
		log.Error("erroed")
		return err
	}

	_, err = runCommand(exec.Command("tar", "-xf", strings.TrimSpace(containerScan.tempDir+".tar"), "-C", containerScan.tempDir), "tar : "+string(containerScan.tempDir))
	if err != nil {
		log.Error(err)
		return err
	}

	return nil
}

func runCommand(cmd *exec.Cmd, operation string) (*bytes.Buffer, error) {
	var out bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	errorOnRun := cmd.Run()
	if errorOnRun != nil {
		return nil, errors.New(operation + fmt.Sprint(errorOnRun) + ": " + stderr.String())
	}
	return &out, nil
}

func GenerateSBOM(config util.Config) ([]byte, error) {
	jsonFile := filepath.Join("/tmp", util.RandomString(12)+"output.json")
	syftArgs := []string{"packages", config.Source, "-o", "json", "--file", jsonFile, "-q"}
	if strings.HasPrefix(config.Source, "dir:") || config.Source == "." {
		for _, excludeDir := range linuxExcludeDirs {
			syftArgs = append(syftArgs, "--exclude", "."+excludeDir+"/**")
		}
		var scanDir = config.Source
		if strings.HasPrefix(config.Source, "dir:") {
			scanDir = strings.Split(scanDir, ":")[1]
		}
		scanDir, _ = filepath.Abs(scanDir)
		for _, excludeDir := range mntDirs {
			if strings.Index(excludeDir, scanDir) == 0 {
				excludeDir = strings.Replace(excludeDir, scanDir, "", 1)
			}
			syftArgs = append(syftArgs, "--exclude", "."+excludeDir+"/**")
		}
	} else {
		if config.NodeType != util.NodeTypeContainer {
			for _, excludeDir := range linuxExcludeDirs {
				syftArgs = append(syftArgs, "--exclude", excludeDir)
			}
		}

		if !strings.HasPrefix(config.Source, "registry:") {
			if (config.ContainerRuntimeName == vesselConstants.CONTAINERD ||
				config.ContainerRuntimeName == vesselConstants.CRIO) &&
				config.ContainerRuntime != nil {
				// This means the underlying container runtime is containerd
				// in case of image scan, we need to generate image tar file and
				// feed it to syft, since syft does not support listing images from containerd
				// ref: https://github.com/anchore/syft/issues/1048
				//
				// TODO : Remove this commit after anchore/syft#1048 is resolved
				//
				// create a temp directory for tar
				tmpDir, err := os.MkdirTemp("", "syft-")
				if err != nil {
					log.Errorf("Error creating temp directory: %v", err)
					return nil, err
				}
				defer os.RemoveAll(tmpDir)
				// create a tar file for the image
				tarFile := filepath.Join(tmpDir, "image.tar")
				_, err = config.ContainerRuntime.Save(config.Source, tarFile)
				if err != nil {
					log.Errorf("Error creating tar file: %v", err)
					return nil, err
				}
				// feed the tar file to syft
				switch config.ContainerRuntimeName {
				case vesselConstants.CONTAINERD:
					syftArgs[1] = "oci-archive:" + tarFile
				case vesselConstants.CRIO:
					syftArgs[1] = "docker-archive:" + tarFile
				}
			} else if config.NodeType == util.NodeTypeContainer {
				tmpDir, err := os.MkdirTemp("", "syft-")
				if err != nil {
					log.Errorf("Error creating temp directory: %v", err)
					return nil, err
				}

				defer os.RemoveAll(tmpDir)
				defer os.Remove(tmpDir + ".tar")

				var containerScan ContainerScan
				if config.KubernetesClusterName != "" {
					containerScan = ContainerScan{containerId: config.ContainerID, tempDir: tmpDir, namespace: ""}
				} else {
					containerScan = ContainerScan{containerId: config.ContainerName, tempDir: tmpDir, namespace: "default"}
				}
				err = containerScan.exportFileSystemTar()

				if err != nil {
					log.Error(err)
					return nil, err
				}
				syftArgs[1] = "dir:" + tmpDir
			}
		}
	}
	if config.ScanType != "" && config.ScanType != "all" {
		scanTypes := strings.Split(config.ScanType, ",")
		for _, scanType := range scanTypes {
			if scanType == "base" {
				syftArgs = append(syftArgs, "--catalogers", "dpkgdb-cataloger", "--catalogers", "rpmdb-cataloger", "--catalogers", "apkdb-cataloger", "--catalogers", "alpmdb-cataloger")
			} else if scanType == "ruby" {
				syftArgs = append(syftArgs, "--catalogers", "ruby-gemfile-cataloger", "--catalogers", "ruby-gemspec-cataloger")
			} else if scanType == "python" {
				syftArgs = append(syftArgs, "--catalogers", "python-index-cataloger", "--catalogers", "python-package-cataloger")
			} else if scanType == "javascript" {
				syftArgs = append(syftArgs, "--catalogers", "javascript-lock-cataloger", "--catalogers", "javascript-package-cataloger")
			} else if scanType == "php" {
				syftArgs = append(syftArgs, "--catalogers", "php-composer-installed-cataloger", "--catalogers", "php-composer-lock-cataloger")
			} else if scanType == "golang" {
				syftArgs = append(syftArgs, "--catalogers", "go-mod-file-cataloger")
			} else if scanType == "java" {
				syftArgs = append(syftArgs, "--catalogers", "java-cataloger")
			} else if scanType == "rust" {
				syftArgs = append(syftArgs, "--catalogers", "rust-cataloger")
			} else if scanType == "dotnet" {
				syftArgs = append(syftArgs, "--catalogers", "dotnet-deps-cataloger")
			}
		}
	}

	var publisher *output.Publisher
	var err error

	if config.VulnerabilityScan == true {
		publisher, err = output.NewPublisher(config)
		if err != nil {
			log.Error("error in creating publisher")
			return nil, err
		}
		publisher.PublishScanStatus("GENERATING_SBOM")
	}

	insecureRegistry := isRegistryInsecure(config.RegistryId)
	if strings.Contains(syftArgs[1], "registry:") && insecureRegistry {
		syftArgs[1] = strings.Replace(syftArgs[1], "registry:", "", -1)
	}

	cmd := exec.Command("syft", syftArgs...)
	if config.RegistryId != "" && config.NodeType == util.NodeTypeImage {
		authFilePath, err := GetConfigFileFromRegistry(config.RegistryId)
		if err != nil {
			log.Error("error in getting authFilePath")
			return nil, err
		}
		defer os.RemoveAll(authFilePath)
		cmd.Env = os.Environ()
		if authFilePath != "" {
			cmd.Env = append(cmd.Env, fmt.Sprintf("DOCKER_CONFIG=%s", authFilePath))
		}
		if insecureRegistry {
			cmd.Env = append(cmd.Env, fmt.Sprintf("SYFT_REGISTRY_INSECURE_SKIP_TLS_VERIFY=%s", "true"))
			cmd.Env = append(cmd.Env, fmt.Sprintf("SYFT_REGISTRY_INSECURE_USE_HTTP=%s", "true"))
		}
	}

	stdout, err := cmd.CombinedOutput()
	if err != nil {
		log.Error("error from syft command for syftArgs: " + strings.Join(syftArgs, " "))
		log.Error("output:" + string(stdout) + " " + err.Error())
		if config.VulnerabilityScan == true {
			publisher.PublishScanError(string(stdout) + " " + err.Error())
		}
		return nil, err
	}

	sbom, err := os.ReadFile(jsonFile)
	if err != nil {
		log.Error("error reading internal file", err)
		return nil, err
	}
	defer os.RemoveAll(jsonFile)

	if config.VulnerabilityScan == true {
		publisher.StopPublishScanStatus()
		// Send sbom to Deepfence Management Console for Vulnerability Scan
		publisher.RunVulnerabilityScan(sbom)

		if config.Quiet == true && config.FailOnScore <= 0 && config.FailOnCount <= 0 {
			return sbom, nil
		}

		vulnerabilityScanDetail, err := publisher.GetVulnerabilityScanResults()
		if err != nil {
			log.Error("error in getting vulnerability scan detail")
			return sbom, err
		}

		if config.Quiet == false {
			_ = publisher.Output(vulnerabilityScanDetail)
		}

		if config.FailOnCount > 0 {
			exitOnSeverity := func(count int, failOnCount int) {
				if count >= failOnCount {
					log.Fatalf("Exit vulnerability scan. Number of vulnerabilities (%d) reached/exceeded the limit (%d).", count, failOnCount)
					os.Exit(1)
				}
			}
			if vulnerabilityScanDetail.Total >= config.FailOnCount {
				exitOnSeverity(vulnerabilityScanDetail.Total, config.FailOnCount)
			} else if vulnerabilityScanDetail.Severity.Critical >= config.FailOnCriticalCount {
				exitOnSeverity(vulnerabilityScanDetail.Severity.Critical, config.FailOnCriticalCount)
			} else if vulnerabilityScanDetail.Severity.High >= config.FailOnHighCount {
				exitOnSeverity(vulnerabilityScanDetail.Severity.High, config.FailOnHighCount)
			} else if vulnerabilityScanDetail.Severity.Medium >= config.FailOnMediumCount {
				exitOnSeverity(vulnerabilityScanDetail.Severity.Medium, config.FailOnMediumCount)
			} else if vulnerabilityScanDetail.Severity.Low >= config.FailOnLowCount {
				exitOnSeverity(vulnerabilityScanDetail.Severity.Low, config.FailOnLowCount)
			}
		}
		if config.FailOnScore > 0.0 {
			exitOnSeverityScore := func(score float64, failOnScore float64) {
				if score >= failOnScore {
					log.Fatalf("Exit vulnerability scan. Vulnerability score (%f) reached/exceeded the limit (%f).", score, failOnScore)
					os.Exit(1)
				}
			}
			exitOnSeverityScore(vulnerabilityScanDetail.CveScore, config.FailOnScore)
		}
	}

	return sbom, nil
}

func getNfsMountsDirs() []string {
	cmdOutput, err := exec.Command("findmnt", "-l", "-t", "nfs4,tmpfs", "-n", "--output=TARGET").CombinedOutput()
	if err != nil {
		return nil
	}
	dirs := strings.Split(string(cmdOutput), "\n")
	var mountDirs []string
	for _, i := range dirs {
		if strings.TrimSpace(i) != "" {
			mountDirs = append(mountDirs, i)
		}
	}
	return mountDirs
}
