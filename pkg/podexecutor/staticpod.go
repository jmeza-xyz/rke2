//go:build linux
// +build linux

package podexecutor

import (
	"context"
	"encoding/json"
	"io/ioutil"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/k3s-io/k3s/pkg/agent/containerd"
	"github.com/k3s-io/k3s/pkg/agent/cri"
	"github.com/k3s-io/k3s/pkg/agent/cridockerd"
	"github.com/k3s-io/k3s/pkg/cli/cmds"
	daemonconfig "github.com/k3s-io/k3s/pkg/daemons/config"
	"github.com/k3s-io/k3s/pkg/daemons/executor"
	"github.com/k3s-io/k3s/pkg/util"
	pkgerrors "github.com/pkg/errors"
	"github.com/rancher/rke2/pkg/auth"
	"github.com/rancher/rke2/pkg/bootstrap"
	"github.com/rancher/rke2/pkg/images"
	"github.com/rancher/rke2/pkg/logging"
	"github.com/rancher/rke2/pkg/staticpod"
	"github.com/sirupsen/logrus"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	auditv1 "k8s.io/apiserver/pkg/apis/audit/v1"
	"k8s.io/apiserver/pkg/authentication/authenticator"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
	"sigs.k8s.io/yaml"
)

var (
	ssldirs = []string{
		"/etc/ssl/certs",
		"/etc/pki/tls/certs",
		"/etc/ca-certificates",
		"/usr/local/share/ca-certificates",
		"/usr/share/ca-certificates",
	}
	defaultAuditPolicyFile = "/etc/rancher/rke2/audit-policy.yaml"
)

type ControlPlaneResources struct {
	KubeAPIServerCPURequest             string
	KubeAPIServerCPULimit               string
	KubeAPIServerMemoryRequest          string
	KubeAPIServerMemoryLimit            string
	KubeSchedulerCPURequest             string
	KubeSchedulerCPULimit               string
	KubeSchedulerMemoryRequest          string
	KubeSchedulerMemoryLimit            string
	KubeControllerManagerCPURequest     string
	KubeControllerManagerCPULimit       string
	KubeControllerManagerMemoryRequest  string
	KubeControllerManagerMemoryLimit    string
	KubeProxyCPURequest                 string
	KubeProxyCPULimit                   string
	KubeProxyMemoryRequest              string
	KubeProxyMemoryLimit                string
	EtcdCPURequest                      string
	EtcdCPULimit                        string
	EtcdMemoryRequest                   string
	EtcdMemoryLimit                     string
	CloudControllerManagerCPURequest    string
	CloudControllerManagerCPULimit      string
	CloudControllerManagerMemoryRequest string
	CloudControllerManagerMemoryLimit   string
}

type ControlPlaneEnv struct {
	KubeAPIServer          []string
	KubeScheduler          []string
	KubeControllerManager  []string
	KubeProxy              []string
	Etcd                   []string
	CloudControllerManager []string
}

type ControlPlaneMounts struct {
	KubeAPIServer          []string
	KubeScheduler          []string
	KubeControllerManager  []string
	KubeProxy              []string
	Etcd                   []string
	CloudControllerManager []string
}

type ControlPlaneProbeConfs struct {
	KubeAPIServer          staticpod.ProbeConfs
	KubeScheduler          staticpod.ProbeConfs
	KubeControllerManager  staticpod.ProbeConfs
	KubeProxy              staticpod.ProbeConfs
	Etcd                   staticpod.ProbeConfs
	CloudControllerManager staticpod.ProbeConfs
}

type StaticPodConfig struct {
	Resolver      *images.Resolver
	stopKubelet   context.CancelFunc
	CloudProvider *CloudProviderConfig
	ControlPlaneResources
	DataDir           string
	RuntimeEndpoint   string
	ManifestsDir      string
	IngressController string
	ImagesDir         string
	AuditPolicyFile   string
	PSAConfigFile     string
	KubeletPath       string
	ControlPlaneEnv
	ControlPlaneMounts
	ControlPlaneProbeConfs
	ProfileMode      ProfileMode
	DisableETCD      bool
	ExternalDatabase bool
	IsServer         bool

	apiServerReady <-chan struct{}
	etcdReady      chan struct{}
	criReady       chan struct{}
}

type CloudProviderConfig struct {
	Name string
	Path string
}

// apiserverSyncAndReady returns a channel that is closed once the etcd and apiserver static pods have been synced,
// and the apiserver readyz endpoint returns success.
func apiserverSyncAndReady(ctx context.Context, nodeConfig *daemonconfig.Node, cfg cmds.Agent) <-chan struct{} {
	ready := make(chan struct{})
	go func() {
		defer close(ready)
		reconcileStaticPods(ctx, cfg.ContainerRuntimeEndpoint, cfg.DataDir)
		<-util.APIServerReadyChan(ctx, nodeConfig.AgentConfig.KubeConfigK3sController, util.DefaultAPIServerReadyTimeout)
	}()
	return ready
}

// Bootstrap prepares the static executor to run components by setting the system default registry
// and staging the kubelet and containerd binaries.  On servers, it also ensures that manifests are
// copied in to place and in sync with the system configuration.
func (s *StaticPodConfig) Bootstrap(ctx context.Context, nodeConfig *daemonconfig.Node, cfg cmds.Agent) error {
	s.apiServerReady = apiserverSyncAndReady(ctx, nodeConfig, cfg)
	s.etcdReady = make(chan struct{})
	s.criReady = make(chan struct{})

	// On servers this is set to an initial value from the CLI when the resolver is created, so that
	// static pod manifests can be created before the agent bootstrap is complete. The agent itself
	// really only needs to know about the runtime and pause images, all of which are configured after the
	// default registry has been set by the server.
	if nodeConfig.AgentConfig.SystemDefaultRegistry != "" {
		if err := s.Resolver.ParseAndSetDefaultRegistry(nodeConfig.AgentConfig.SystemDefaultRegistry); err != nil {
			return err
		}
	}
	pauseImage, err := s.Resolver.GetReference(images.Pause)
	if err != nil {
		return err
	}
	nodeConfig.AgentConfig.PauseImage = pauseImage.Name()

	// stage bootstrap content from runtime image
	execPath, err := bootstrap.Stage(s.Resolver, nodeConfig, cfg)
	if err != nil {
		return err
	}
	if err := os.Setenv("PATH", execPath+":"+os.Getenv("PATH")); err != nil {
		return err
	}
	if s.IsServer {
		return bootstrap.UpdateManifests(s.Resolver, s.IngressController, nodeConfig, cfg)
	}

	// Remove the kube-proxy static pod manifest before starting the agent.
	// If kube-proxy should run, the manifest will be recreated after the apiserver is up.
	if err := staticpod.Remove(s.ManifestsDir, "kube-proxy"); err != nil {
		logrus.Error(err)
	}

	return nil
}

// Kubelet starts the kubelet in a subprocess with watching goroutine.
func (s *StaticPodConfig) Kubelet(ctx context.Context, args []string) error {
	extraArgs := []string{
		"--volume-plugin-dir=/var/lib/kubelet/volumeplugins",
		"--file-check-frequency=5s",
		"--sync-frequency=30s",
	}
	if s.CloudProvider != nil {
		extraArgs = append(extraArgs,
			"--cloud-provider="+s.CloudProvider.Name,
			"--cloud-config="+s.CloudProvider.Path,
		)
	}

	args = append(extraArgs, args...)
	args, logOut := logging.ExtractFromArgs(args)
	ctx, cancel := context.WithCancel(ctx)
	s.stopKubelet = cancel

	go func() {
		wait.PollImmediateInfiniteWithContext(ctx, 5*time.Second, func(ctx context.Context) (bool, error) {
			cmd := exec.CommandContext(ctx, s.KubeletPath, args...)
			cmd.Stdout = logOut
			cmd.Stderr = logOut
			addDeathSig(cmd)

			err := cmd.Run()
			logrus.Errorf("Kubelet exited: %v", err)

			return false, nil
		})
	}()

	return nil
}

// KubeProxy starts Kube Proxy as a static pod.
func (s *StaticPodConfig) KubeProxy(_ context.Context, args []string) error {
	image, err := s.Resolver.GetReference(images.KubeProxy)
	if err != nil {
		return err
	}
	if err := images.Pull(s.ImagesDir, images.KubeProxy, image); err != nil {
		return err
	}

	return staticpod.Run(s.ManifestsDir, staticpod.Args{
		Command:       "kube-proxy",
		Args:          args,
		Image:         image,
		CISMode:       s.ProfileMode.isCISMode(),
		HealthPort:    10256,
		HealthProto:   "HTTP",
		CPURequest:    s.ControlPlaneResources.KubeProxyCPURequest,
		CPULimit:      s.ControlPlaneResources.KubeProxyCPULimit,
		MemoryRequest: s.ControlPlaneResources.KubeProxyMemoryRequest,
		MemoryLimit:   s.ControlPlaneResources.KubeProxyMemoryLimit,
		ExtraEnv:      s.ControlPlaneEnv.KubeProxy,
		ExtraMounts:   s.ControlPlaneMounts.KubeProxy,
		ProbeConfs:    s.ControlPlaneProbeConfs.KubeProxy,
		Privileged:    true,
	})
}

// APIServerHandlers returning the authenticator and request handler for requests to the apiserver endpoint.
func (s *StaticPodConfig) APIServerHandlers(ctx context.Context) (authenticator.Request, http.Handler, error) {
	<-s.APIServerReadyChan()
	kubeConfigAPIServer := filepath.Join(s.DataDir, "server", "cred", "api-server.kubeconfig")
	tokenauth, err := auth.BootstrapTokenAuthenticator(ctx, kubeConfigAPIServer)
	return tokenauth, http.NotFoundHandler(), err
}

// APIServer sets up the apiserver static pod once etcd is available.
func (s *StaticPodConfig) APIServer(_ context.Context, args []string) error {
	image, err := s.Resolver.GetReference(images.KubeAPIServer)
	if err != nil {
		return err
	}
	if err := images.Pull(s.ImagesDir, images.KubeAPIServer, image); err != nil {
		return err
	}
	if err := staticpod.Remove(s.ManifestsDir, "kube-apiserver"); err != nil {
		return err
	}

	auditLogFile := ""
	kubeletPreferredAddressTypesFound := false
	for i, arg := range args {
		switch name, value, _ := strings.Cut(arg, "="); name {
		case "--advertise-port", "--basic-auth-file":
			// This is an option k3s adds that does not exist upstream
			args = append(args[:i], args[i+1:]...)
		case "--etcd-servers":
			if s.ExternalDatabase {
				args = append(args[:i], args[i+1:]...)
				args = append([]string{"--etcd-servers=" + "unixs://" + filepath.Join(s.DataDir, "server", "kine.sock")}, args...)
			}
		case "--audit-log-path":
			auditLogFile = value
		case "--kubelet-preferred-address-types":
			kubeletPreferredAddressTypesFound = true
		}
	}
	if !kubeletPreferredAddressTypesFound {
		args = append([]string{"--kubelet-preferred-address-types=InternalIP,ExternalIP,Hostname"}, args...)
	}

	if s.ProfileMode.isCISMode() && s.AuditPolicyFile == "" {
		s.AuditPolicyFile = defaultAuditPolicyFile
	}

	if s.AuditPolicyFile != "" {
		if err := writeDefaultPolicyFile(s.AuditPolicyFile); err != nil {
			return err
		}
		extraArgs := []string{
			"--audit-policy-file=" + s.AuditPolicyFile,
			"--audit-log-maxage=30",
			"--audit-log-maxbackup=10",
			"--audit-log-maxsize=100",
		}
		if auditLogFile == "" {
			auditLogFile = filepath.Join(s.DataDir, "server/logs/audit.log")
			extraArgs = append(extraArgs, "--audit-log-path="+auditLogFile)
		}
		args = append(extraArgs, args...)
	}

	args = append([]string{"--admission-control-config-file=" + s.PSAConfigFile}, args...)

	files := []string{}
	excludeFiles := []string{}
	if !s.DisableETCD {
		files = append(files, etcdNameFile(s.DataDir))
	}

	dirs := onlyExisting(ssldirs)
	if auditLogFile != "" && auditLogFile != "-" {
		dirs = append(dirs, filepath.Dir(auditLogFile))
		excludeFiles = append(excludeFiles, auditLogFile)
	}

	// Need to mount the entire server directory so that any files recreated in this directory
	// after the pod has been started are not masked by a stale mount
	// encryption config is refreshed by the secrets-encryption controller
	// so we mount the directory to allow the pod to see the updates
	dirs = append(dirs, filepath.Join(s.DataDir, "server"))
	excludeFiles = append(excludeFiles, filepath.Join(s.DataDir, "server/cred/encryption-config.json"))

	apiServerArgs := staticpod.Args{
		Command:       "kube-apiserver",
		Args:          args,
		Image:         image,
		Dirs:          dirs,
		CISMode:       s.ProfileMode.isCISMode(),
		CPURequest:    s.ControlPlaneResources.KubeAPIServerCPURequest,
		CPULimit:      s.ControlPlaneResources.KubeAPIServerCPULimit,
		MemoryRequest: s.ControlPlaneResources.KubeAPIServerMemoryRequest,
		MemoryLimit:   s.ControlPlaneResources.KubeAPIServerMemoryLimit,
		ExtraEnv:      s.ControlPlaneEnv.KubeAPIServer,
		ExtraMounts:   s.ControlPlaneMounts.KubeAPIServer,
		ProbeConfs:    s.ControlPlaneProbeConfs.KubeAPIServer,
		Files:         files,
		ExcludeFiles:  excludeFiles,
		HealthExec: []string{
			"kubectl",
			"get",
			"--server=https://localhost:6443/",
			"--client-certificate=" + s.DataDir + "/server/tls/client-kube-apiserver.crt",
			"--client-key=" + s.DataDir + "/server/tls/client-kube-apiserver.key",
			"--certificate-authority=" + s.DataDir + "/server/tls/server-ca.crt",
			"--raw=/livez",
		},
		ReadyExec: []string{
			"kubectl",
			"get",
			"--server=https://localhost:6443/",
			"--client-certificate=" + s.DataDir + "/server/tls/client-kube-apiserver.crt",
			"--client-key=" + s.DataDir + "/server/tls/client-kube-apiserver.key",
			"--certificate-authority=" + s.DataDir + "/server/tls/server-ca.crt",
			"--raw=/readyz",
		},
	}

	return after(s.ETCDReadyChan(), func() error {
		return staticpod.Run(s.ManifestsDir, apiServerArgs)
	})
}

var permitPortSharingFlag = []string{"--permit-port-sharing=true"}

// Scheduler starts the kube-scheduler static pod, once the apiserver is available.
func (s *StaticPodConfig) Scheduler(_ context.Context, nodeReady <-chan struct{}, args []string) error {
	image, err := s.Resolver.GetReference(images.KubeScheduler)
	if err != nil {
		return err
	}
	if err := images.Pull(s.ImagesDir, images.KubeScheduler, image); err != nil {
		return err
	}
	files := []string{}
	if !s.DisableETCD {
		files = append(files, etcdNameFile(s.DataDir))
	}

	args = append(permitPortSharingFlag, args...)
	return after(s.APIServerReadyChan(), func() error {
		return staticpod.Run(s.ManifestsDir, staticpod.Args{
			Command:       "kube-scheduler",
			Args:          args,
			Image:         image,
			CISMode:       s.ProfileMode.isCISMode(),
			HealthPort:    10259,
			HealthProto:   "HTTPS",
			CPURequest:    s.ControlPlaneResources.KubeSchedulerCPURequest,
			CPULimit:      s.ControlPlaneResources.KubeSchedulerCPULimit,
			MemoryRequest: s.ControlPlaneResources.KubeSchedulerMemoryRequest,
			MemoryLimit:   s.ControlPlaneResources.KubeSchedulerMemoryLimit,
			ExtraEnv:      s.ControlPlaneEnv.KubeScheduler,
			ExtraMounts:   s.ControlPlaneMounts.KubeScheduler,
			ProbeConfs:    s.ControlPlaneProbeConfs.KubeScheduler,
			Files:         files,
		})
	})
}

// onlyExisting filters out paths from the list that cannot be accessed
func onlyExisting(paths []string) []string {
	existing := []string{}
	for _, path := range paths {
		if _, err := os.Stat(path); err == nil {
			existing = append(existing, path)
		}
	}
	return existing
}

// after calls a function after a message is received from a channel.
func after(after <-chan struct{}, f func() error) error {
	go func() {
		<-after
		if err := f(); err != nil {
			logrus.Fatal(err)
		}
	}()
	return nil
}

// ControllerManager starts the kube-controller-manager static pod, once the apiserver is available.
func (s *StaticPodConfig) ControllerManager(_ context.Context, args []string) error {
	image, err := s.Resolver.GetReference(images.KubeControllerManager)
	if err != nil {
		return err
	}
	if err := images.Pull(s.ImagesDir, images.KubeControllerManager, image); err != nil {
		return err
	}
	if s.CloudProvider != nil {
		extraArgs := []string{
			"--cloud-provider=" + s.CloudProvider.Name,
			"--cloud-config=" + s.CloudProvider.Path,
		}
		args = append(extraArgs, args...)
	}
	args = append(permitPortSharingFlag, args...)

	files := []string{}
	if !s.DisableETCD {
		files = append(files, etcdNameFile(s.DataDir))
	}

	return after(s.APIServerReadyChan(), func() error {
		extraArgs := []string{
			"--flex-volume-plugin-dir=/var/lib/kubelet/volumeplugins",
			"--terminated-pod-gc-threshold=1000",
		}
		args = append(extraArgs, args...)
		return staticpod.Run(s.ManifestsDir, staticpod.Args{
			Command:       "kube-controller-manager",
			Args:          args,
			Image:         image,
			Dirs:          onlyExisting(ssldirs),
			CISMode:       s.ProfileMode.isCISMode(),
			HealthPort:    10257,
			HealthProto:   "HTTPS",
			CPURequest:    s.ControlPlaneResources.KubeControllerManagerCPURequest,
			CPULimit:      s.ControlPlaneResources.KubeControllerManagerCPULimit,
			MemoryRequest: s.ControlPlaneResources.KubeControllerManagerMemoryRequest,
			MemoryLimit:   s.ControlPlaneResources.KubeControllerManagerMemoryLimit,
			ExtraEnv:      s.ControlPlaneEnv.KubeControllerManager,
			ExtraMounts:   s.ControlPlaneMounts.KubeControllerManager,
			ProbeConfs:    s.ControlPlaneProbeConfs.KubeControllerManager,
			Files:         files,
		})
	})
}

// CloudControllerManager starts the cloud-controller-manager static pod, once the cloud controller manager RBAC
// (and subsequently, the api server) is available.
func (s *StaticPodConfig) CloudControllerManager(_ context.Context, ccmRBACReady <-chan struct{}, args []string) error {
	image, err := s.Resolver.GetReference(images.CloudControllerManager)
	if err != nil {
		return err
	}
	if err := images.Pull(s.ImagesDir, images.CloudControllerManager, image); err != nil {
		return err
	}
	return after(ccmRBACReady, func() error {
		return staticpod.Run(s.ManifestsDir, staticpod.Args{
			Command:       "cloud-controller-manager",
			Args:          args,
			Image:         image,
			Dirs:          onlyExisting(ssldirs),
			CISMode:       s.ProfileMode.isCISMode(),
			HealthPort:    10258,
			HealthProto:   "HTTPS",
			CPURequest:    s.ControlPlaneResources.CloudControllerManagerCPURequest,
			CPULimit:      s.ControlPlaneResources.CloudControllerManagerCPULimit,
			MemoryRequest: s.ControlPlaneResources.CloudControllerManagerMemoryRequest,
			MemoryLimit:   s.ControlPlaneResources.CloudControllerManagerMemoryLimit,
			ExtraEnv:      s.ControlPlaneEnv.CloudControllerManager,
			ExtraMounts:   s.ControlPlaneMounts.CloudControllerManager,
			ProbeConfs:    s.ControlPlaneProbeConfs.CloudControllerManager,
			Files:         []string{},
		})
	})
}

// CurrentETCDOptions retrieves the etcd configuration from the static pod definition at etcd.yaml
// in the manifests directory, if it exists.
func (s *StaticPodConfig) CurrentETCDOptions() (opts executor.InitialOptions, err error) {
	bytes, err := ioutil.ReadFile(filepath.Join(s.ManifestsDir, "etcd.yaml"))
	if os.IsNotExist(err) {
		return opts, nil
	}

	pod := &v1.Pod{}
	if err := yaml.Unmarshal(bytes, pod); err != nil {
		return opts, err
	}

	v, ok := pod.Annotations["etcd.k3s.io/initial"]
	if ok {
		return opts, json.NewDecoder(strings.NewReader(v)).Decode(&opts)
	}

	return opts, nil
}

// ETCD starts the etcd static pod.
func (s *StaticPodConfig) ETCD(ctx context.Context, args *executor.ETCDConfig, extraArgs []string, test executor.TestFunc) error {
	go func() {
		defer close(s.etcdReady)
		for {
			if err := test(ctx); err != nil {
				logrus.Infof("Failed to test etcd connection: %v", err)
			} else {
				logrus.Info("Connection to etcd is ready")
				return
			}

			select {
			case <-time.After(5 * time.Second):
			case <-ctx.Done():
				return
			}
		}
	}()

	// nil args indicates a no-op start; all we need to do is wait for the test
	// func to indicate readiness and close the channel.
	if args == nil {
		return nil
	}

	image, err := s.Resolver.GetReference(images.ETCD)
	if err != nil {
		return err
	}
	if err := images.Pull(s.ImagesDir, images.ETCD, image); err != nil {
		return err
	}

	initial, err := json.Marshal(args.InitialOptions)
	if err != nil {
		return err
	}

	confFile, err := args.ToConfigFile(extraArgs)
	if err != nil {
		return err
	}

	spa := staticpod.Args{
		Annotations: map[string]string{
			"etcd.k3s.io/initial": string(initial),
		},
		Command: "etcd",
		Args: []string{
			"--config-file=" + confFile,
		},
		Image: image,
		Dirs:  []string{args.DataDir},
		Files: []string{
			args.ServerTrust.CertFile,
			args.ServerTrust.KeyFile,
			args.ServerTrust.TrustedCAFile,
			args.PeerTrust.CertFile,
			args.PeerTrust.KeyFile,
			args.PeerTrust.TrustedCAFile,
		},
		CISMode:       s.ProfileMode.isAnyMode(),
		HealthPort:    2381,
		HealthPath:    "/health?serializable=true",
		HealthProto:   "HTTP",
		CPURequest:    s.ControlPlaneResources.EtcdCPURequest,
		CPULimit:      s.ControlPlaneResources.EtcdCPULimit,
		MemoryRequest: s.ControlPlaneResources.EtcdMemoryRequest,
		MemoryLimit:   s.ControlPlaneResources.EtcdMemoryLimit,
		ExtraEnv:      s.ControlPlaneEnv.Etcd,
		ExtraMounts:   s.ControlPlaneMounts.Etcd,
		ProbeConfs:    s.ControlPlaneProbeConfs.Etcd,
	}

	if s.ProfileMode.isAnyMode() {
		etcdUser, err := user.Lookup("etcd")
		if err != nil {
			return err
		}
		uid, err := strconv.ParseInt(etcdUser.Uid, 10, 64)
		if err != nil {
			return err
		}
		gid, err := strconv.ParseInt(etcdUser.Gid, 10, 64)
		if err != nil {
			return err
		}
		if spa.SecurityContext == nil {
			spa.SecurityContext = &v1.PodSecurityContext{}
		}
		spa.SecurityContext.RunAsUser = &uid
		spa.SecurityContext.RunAsGroup = &gid

		for _, p := range []string{args.DataDir, filepath.Dir(args.ServerTrust.CertFile)} {
			if err := chownr(p, int(uid), int(gid)); err != nil {
				return err
			}
		}
	}

	if cmds.AgentConfig.EnableSELinux {
		if spa.SecurityContext == nil {
			spa.SecurityContext = &v1.PodSecurityContext{}
		}
		if spa.SecurityContext.SELinuxOptions == nil {
			spa.SecurityContext.SELinuxOptions = &v1.SELinuxOptions{
				Type: "rke2_service_db_t",
			}
		}
	}

	// If performing a cluster-reset, ensure that the kubelet and etcd are stopped when the context is cancelled at the end of the cluster-reset process.
	if args.ForceNewCluster {
		go func() {
			<-ctx.Done()
			logrus.Infof("Shutting down kubelet and etcd")
			if s.stopKubelet != nil {
				s.stopKubelet()
			}
			if err := s.stopEtcd(); err != nil {
				logrus.Errorf("Failed to stop etcd: %v", err)
			}
		}()
	}

	return staticpod.Run(s.ManifestsDir, spa)
}

// Containerd starts the k3s implementation of containerd
func (s *StaticPodConfig) Containerd(ctx context.Context, config *daemonconfig.Node) error {
	defer close(s.criReady)
	return containerd.Run(ctx, config)
}

// Docker starts the k3s implementation of cridockerd
func (s *StaticPodConfig) Docker(ctx context.Context, config *daemonconfig.Node) error {
	defer close(s.criReady)
	return cridockerd.Run(ctx, config)
}

func (s *StaticPodConfig) CRI(ctx context.Context, cfg *daemonconfig.Node) error {
	defer close(s.criReady)
	// agentless sets cri socket path to /dev/null to indicate no CRI is needed
	if cfg.ContainerRuntimeEndpoint != "/dev/null" {
		return cri.WaitForService(ctx, cfg.ContainerRuntimeEndpoint, "CRI")
	}
	return nil
}

func (s *StaticPodConfig) APIServerReadyChan() <-chan struct{} {
	if s.apiServerReady == nil {
		panic("executor not bootstrapped")
	}
	return s.apiServerReady
}

func (s *StaticPodConfig) ETCDReadyChan() <-chan struct{} {
	if s.etcdReady == nil {
		panic("executor not bootstrapped")
	}
	return s.etcdReady
}

func (s *StaticPodConfig) CRIReadyChan() <-chan struct{} {
	if s.criReady == nil {
		panic("executor not bootstrapped")
	}
	return s.criReady
}

// stopEtcd searches the container runtime endpoint for the etcd static pod, and terminates it.
func (s *StaticPodConfig) stopEtcd() error {
	ctx := context.Background()
	conn, err := cri.Connection(ctx, s.RuntimeEndpoint)
	if err != nil {
		return pkgerrors.WithMessage(err, "failed to connect to cri")
	}
	cRuntime := runtimeapi.NewRuntimeServiceClient(conn)
	defer conn.Close()

	filter := &runtimeapi.PodSandboxFilter{
		LabelSelector: map[string]string{
			"component":                   "etcd",
			"io.kubernetes.pod.namespace": "kube-system",
			"tier":                        "control-plane",
		},
	}
	resp, err := cRuntime.ListPodSandbox(ctx, &runtimeapi.ListPodSandboxRequest{Filter: filter})
	if err != nil {
		return pkgerrors.WithMessage(err, "failed to list pods")
	}

	for _, pod := range resp.Items {
		if pod.Annotations["kubernetes.io/config.source"] != "file" {
			continue
		}
		if _, err := cRuntime.RemovePodSandbox(ctx, &runtimeapi.RemovePodSandboxRequest{PodSandboxId: pod.Id}); err != nil {
			return pkgerrors.WithMessage(err, "failed to terminate pod")
		}
	}

	return nil
}

// chownr recursively changes the ownership of the given
// path to the given user ID and group ID.
func chownr(path string, uid, gid int) error {
	return filepath.Walk(path, func(name string, info os.FileInfo, err error) error {
		if err == nil {
			err = os.Chown(name, uid, gid)
		}
		return err
	})
}

func kineSock(dataDir string) string {
	return filepath.Join(dataDir, "server", "kine.sock")
}

func etcdNameFile(dataDir string) string {
	return filepath.Join(dataDir, "server", "db", "etcd", "name")
}

func writeDefaultPolicyFile(policyFilePath string) error {
	auditPolicy := auditv1.Policy{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Policy",
			APIVersion: "audit.k8s.io/v1",
		},
		ObjectMeta: metav1.ObjectMeta{},
		Rules: []auditv1.PolicyRule{
			{
				Level: "None",
			},
		},
	}
	bytes, err := yaml.Marshal(auditPolicy)
	if err != nil {
		return err
	}
	return writeIfNotExists(policyFilePath, bytes)
}

// writeIfNotExists writes content to a file at a given path, but only if the file does not already exist
func writeIfNotExists(path string, content []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		if os.IsExist(err) {
			return nil
		}
		return err
	}
	defer file.Close()
	_, err = file.Write(content)
	return err
}
