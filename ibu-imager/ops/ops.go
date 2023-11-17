package ops

import (
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
	"gopkg.in/yaml.v3"

	"github.com/openshift-kni/lifecycle-agent/ibu-imager/common"
	"github.com/openshift-kni/lifecycle-agent/internal/clusterconfig"
)

// Ops is an interface for executing commands and actions in the host namespace
//
//go:generate mockgen -source=ops.go -package=ops -destination=mock_ops.go
type Ops interface {
	SystemctlAction(action string, args ...string) (string, error)
	RunInHostNamespace(command string, args ...string) (string, error)
	RunBashInHostNamespace(command string, args ...string) (string, error)

	RunUnauthenticatedEtcdServer(etcdImage, authFile string) error
	GetImageFromPodDefinition(etcdStaticPodFile, containerImage string) (string, error)
	RunRecert(authFile, recertContainerImage string, additionalArgs ...string) error
}

type ops struct {
	log      *logrus.Logger
	executor Execute
}

// NewOps creates and returns an Ops interface for executing host namespace operations
func NewOps(log *logrus.Logger, executor Execute) Ops {
	return &ops{executor: executor, log: log}
}

func (o *ops) SystemctlAction(action string, args ...string) (string, error) {
	o.log.Infof("Running systemctl %s %s", action, args)
	output, err := o.RunInHostNamespace("systemctl", append([]string{action}, args...)...)
	if err != nil {
		err = fmt.Errorf("failed executing systemctl %s %s: %w", action, args, err)
	}
	return output, err
}

// RunInHostNamespace execute a command in the host environment via nsenter
func (o *ops) RunInHostNamespace(command string, args ...string) (string, error) {
	// nsenter is used here to launch processes inside the container in a way that makes said processes feel
	// and behave as if they're running on the host directly rather than inside the container
	commandBase := "nsenter"

	arguments := []string{
		"--target", "1",
		// Entering the cgroup namespace is not required for podman on CoreOS (where the
		// agent typically runs), but it's needed on some Fedora versions and
		// some other systemd based systems. Those systems are used to run dry-mode
		// agents for load testing. If this flag is not used, Podman will sometimes
		// have trouble creating a systemd cgroup slice for new containers.
		"--cgroup",
		// The mount namespace is required for podman to access the host's container
		// storage
		"--mount",
		// TODO: Document why we need the IPC namespace
		"--ipc",
		"--pid",
		"--",
		command,
	}

	arguments = append(arguments, args...)
	return o.executor.Execute(commandBase, arguments...)
}

func (o *ops) RunBashInHostNamespace(command string, args ...string) (string, error) {
	args = append([]string{command}, args...)
	return o.RunInHostNamespace("bash", "-c", strings.Join(args, " "))
}

func (o *ops) GetImageFromPodDefinition(podFile, containerImage string) (string, error) {
	type PodConfig struct {
		Spec struct {
			Containers []struct {
				Name  string `yaml:"name"`
				Image string `yaml:"image"`
			} `yaml:"containers"`
		} `yaml:"spec"`
	}

	yamlData, err := os.ReadFile(podFile)
	if err != nil {
		return "", fmt.Errorf("error reading the YAML file: %w", err)
	}

	var podConfig PodConfig
	if err = yaml.Unmarshal(yamlData, &podConfig); err != nil {
		return "", fmt.Errorf("error unmarshaling YAML data: %w", err)
	}

	var etcdImage string
	for _, container := range podConfig.Spec.Containers {
		if container.Name == containerImage {
			etcdImage = container.Image
			return etcdImage, nil
		}
	}

	return "", fmt.Errorf("no 'etcd' container found or no image specified in YAML definition: %w", err)
}

func (o *ops) RunUnauthenticatedEtcdServer(etcdImage, authFile string) error {
	o.log.Info("Run unauthenticated etcd server for recert tool")

	command := "podman"
	args := append(clusterconfig.PodmanRecertArgs, authFile, "--name", "recert_etcd",
		"--entrypoint", "etcd",
		"-v", "/var/lib/etcd:/store",
		etcdImage,
		"--name", "editor", "--data-dir", "/store")

	// Run the command and return an error if it occurs
	_, err := o.RunInHostNamespace(command, args...)
	if err != nil {
		return err
	}

	o.log.Info("Waiting for unauthenticated etcd start serving for recert tool")
	err = o.waitForEtcd(common.EtcdDefaultEndpoint + "/health")
	if err != nil {
		return fmt.Errorf("failed to wait for unauthenticated etcd server: %w", err)
	}
	o.log.Info("Unauthenticated etcd server for recert is up and running")

	return nil
}

func (o *ops) waitForEtcd(healthzEndpoint string) error {
	timeout := time.After(1 * time.Minute)
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-timeout:
			return fmt.Errorf("timeout waiting for etcd")
		case <-ticker.C:
			resp, err := http.Get(healthzEndpoint)
			if err != nil {
				o.log.Infof("Waiting for etcd: %s", err)
				continue
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				o.log.Infof("Waiting for etcd, status: %d", resp.StatusCode)
				continue
			}

			return nil
		}
	}
}

func (o *ops) RunRecert(recertContainerImage, authFile string, additionalArgs ...string) error {
	command := "podman"
	args := append(clusterconfig.PodmanRecertArgs, authFile, "--name", "recert",
		"-v", "/etc:/host-etc",
		"-v", "/etc/kubernetes:/kubernetes",
		"-v", "/var/lib/kubelet:/kubelet",
		"-v", common.BackupCertsDir+":/certs",
		"-v", "/etc/machine-config-daemon:/machine-config-daemon",
		recertContainerImage,
		"--etcd-endpoint", "localhost:2379",
		"--static-dir", "/kubernetes",
		"--static-dir", "/kubelet",
		"--static-dir", "/machine-config-daemon")

	// Add additional arguments to the command
	args = append(args, additionalArgs...)

	_, err := o.RunInHostNamespace(command, args...)
	if err != nil {
		return fmt.Errorf("failed to run recert tool container: %w", err)
	}

	return nil
}
