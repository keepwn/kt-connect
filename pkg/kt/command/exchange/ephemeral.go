package exchange

import (
	"context"
	"fmt"
	"github.com/alibaba/kt-connect/pkg/common"
	"github.com/alibaba/kt-connect/pkg/kt"
	"github.com/alibaba/kt-connect/pkg/kt/cluster"
	"github.com/alibaba/kt-connect/pkg/kt/command/general"
	"github.com/alibaba/kt-connect/pkg/kt/exec/sshchannel"
	"github.com/alibaba/kt-connect/pkg/kt/options"
	"github.com/alibaba/kt-connect/pkg/kt/tunnel"
	"github.com/alibaba/kt-connect/pkg/kt/util"
	"github.com/rs/zerolog/log"
	coreV1 "k8s.io/api/core/v1"
	"strconv"
	"strings"
	"sync"
	"time"
)

func ByEphemeralContainer(resourceName string, cli kt.CliInterface, options *options.DaemonOptions) error {
	log.Warn().Msgf("Experimental feature. It just works on kubernetes above v1.23, and it can NOT work with istio.")

	ctx := context.Background()
	pods, err := getPodsOfResource(ctx, cli.Kubernetes(), resourceName, options.Namespace)

	for _, pod := range pods {
		if pod.Status.Phase != coreV1.PodRunning {
			log.Warn().Msgf("Pod %s is not running (%s), will not be exchanged", pod.Name, pod.Status.Phase)
			continue
		}
		privateKey, err2 := createEphemeralContainer(ctx, cli.Kubernetes(), common.KtExchangeContainer, pod.Name, options)
		if err2 != nil {
			return err2
		}

		// record data
		options.RuntimeOptions.Shadow = util.Append(options.RuntimeOptions.Shadow, pod.Name)

		localSSHPort, err2 := tunnel.ForwardPodToLocal(options.ExchangeOptions.Expose, pod.Name, privateKey, options)
		if err2 != nil {
			return err2
		}
		err = exchangeWithEphemeralContainer(options.ExchangeOptions.Expose, localSSHPort, privateKey)
		if err != nil {
			return err
		}
	}
	return nil
}


func getPodsOfResource(ctx context.Context, k8s cluster.KubernetesInterface, resourceName, namespace string) ([]coreV1.Pod, error) {
	resourceType, name, err := general.ParseResourceName(resourceName)
	if err != nil {
		return nil, err
	}

	switch resourceType {
	case "pod":
		pod, err := k8s.GetPod(ctx, name, namespace)
		if err != nil {
			return nil, err
		} else {
			return []coreV1.Pod{*pod}, nil
		}
	case "svc":
		fallthrough
	case "service":
		return getPodsOfService(ctx, k8s, name, namespace)
	}
	return nil, fmt.Errorf("invalid resource type: %s", resourceType)
}

func getPodsOfService(ctx context.Context, k8s cluster.KubernetesInterface, serviceName, namespace string) ([]coreV1.Pod, error) {
	svc, err := k8s.GetService(ctx, serviceName, namespace)
	if err != nil {
		return nil, err
	}
	pods, err := k8s.GetPodsByLabel(ctx, svc.Spec.Selector, namespace)
	if err != nil {
		return nil, err
	}
	return pods.Items, nil
}

func createEphemeralContainer(ctx context.Context, k8s cluster.KubernetesInterface, containerName, podName string, options *options.DaemonOptions) (string, error) {
	log.Info().Msgf("Adding ephemeral container for pod %s", podName)

	envs := make(map[string]string)
	privateKey, err := k8s.AddEphemeralContainer(ctx, containerName, podName, options, envs)
	if err != nil {
		return "", err
	}

	for i := 0; i < 10; i++ {
		log.Info().Msgf("Waiting for ephemeral container %s to be ready", containerName)
		ready, err2 := isEphemeralContainerReady(ctx, k8s, containerName, podName, options.Namespace)
		if err2 != nil {
			return "", err2
		} else if ready {
			break
		}
		time.Sleep(5 * time.Second)
	}
	return privateKey, nil
}

func isEphemeralContainerReady(ctx context.Context, k8s cluster.KubernetesInterface, podName, containerName, namespace string) (bool, error) {
	pod, err := k8s.GetPod(ctx, podName, namespace)
	if err != nil {
		return false, err
	}
	cStats := pod.Status.EphemeralContainerStatuses
	for i := range cStats {
		if cStats[i].Name == containerName {
			if cStats[i].State.Running != nil {
				return true, nil
			} else if cStats[i].State.Terminated != nil {
				return false, fmt.Errorf("ephemeral container %s is terminated, code: %d",
					containerName, cStats[i].State.Terminated.ExitCode)
			}
		}
	}
	return false, nil
}

func exchangeWithEphemeralContainer(exposePorts string, localSSHPort int, privateKey string) error {
	ssh := sshchannel.SSHChannel{}
	// Get all listened ports on remote host
	listenedPorts, err := getListenedPorts(&ssh, localSSHPort, privateKey)
	if err != nil {
		return err
	}

	redirectPorts, err := remoteRedirectPort(exposePorts, listenedPorts)
	if err != nil {
		return err
	}
	var redirectPortStr string
	for k, v := range redirectPorts {
		redirectPortStr += fmt.Sprintf("%d:%d,", k, v)
	}
	redirectPortStr = redirectPortStr[:len(redirectPortStr)-1]
	err = setupIptables(&ssh, redirectPortStr, localSSHPort, privateKey)
	if err != nil {
		return err
	}
	portPairs := strings.Split(exposePorts, ",")
	for _, exposePort := range portPairs {
		localPort, remotePort, err2 := util.ParsePortMapping(exposePort)
		if err2 != nil {
			return err2
		}
		var wg sync.WaitGroup
		tunnel.ExposeLocalPort(&wg, &ssh, localPort, redirectPorts[remotePort], localSSHPort, privateKey)
		wg.Done()
	}

	return nil
}


func setupIptables(ssh sshchannel.Channel, redirectPorts string, localSSHPort int, privateKey string) error {
	res, err := ssh.RunScript(
		privateKey,
		fmt.Sprintf("127.0.0.1:%d", localSSHPort),
		fmt.Sprintf("/setup_iptables.sh %s", redirectPorts))

	if err != nil {
		log.Error().Err(err).Msgf("Setup iptables failed, error")
	}

	log.Debug().Msgf("Run setup iptables result: %s", res)
	return err
}

func getListenedPorts(ssh sshchannel.Channel, localSSHPort int, privateKey string) (map[int]struct{}, error) {
	result, err := ssh.RunScript(
		privateKey,
		fmt.Sprintf("127.0.0.1:%d", localSSHPort),
		`netstat -tuln | grep -E '^(tcp|udp|tcp6)' | grep LISTEN | awk '{print $4}' | awk -F: '{printf("%s\n", $NF)}'`)

	if err != nil {
		return nil, err
	}

	log.Debug().Msgf("Run get listened ports result: %s", result)
	var listenedPorts = make(map[int]struct{})
	// The result should be a string like
	// 38059
	// 22
	parts := strings.Split(result, "\n")
	for i := range parts {
		if len(parts[i]) > 0 {
			port, err2 := strconv.Atoi(parts[i])
			if err2 != nil {
				log.Warn().Err(err2).Msgf("Failed to fetch listened ports (got '%s')", parts[i])
				continue
			}
			listenedPorts[port] = struct{}{}
		}
	}

	return listenedPorts, nil
}

func remoteRedirectPort(exposePorts string, listenedPorts map[int]struct{}) (map[int]int, error) {
	portPairs := strings.Split(exposePorts, ",")
	redirectPort := make(map[int]int)
	for _, exposePort := range portPairs {
		_, remotePort, err := util.ParsePortMapping(exposePort)
		if err != nil {
			return nil, err
		}
		port := randPort(listenedPorts)
		if port == -1 {
			return nil, fmt.Errorf("failed to find redirect port for port: %d", remotePort)
		}
		redirectPort[remotePort] = port
	}

	return redirectPort, nil
}

func randPort(listenedPorts map[int]struct{}) int {
	for i := 0; i < 100; i++ {
		port := util.RandomPort()
		if _, exists := listenedPorts[port]; !exists {
			return port
		}
	}
	return -1
}

