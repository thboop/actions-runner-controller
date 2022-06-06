/*
Copyright 2020 The actions-runner-controller authors.

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

package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	actionsv1alpha1 "github.com/actions-runner-controller/actions-runner-controller/api/v1alpha1"
	"github.com/actions-runner-controller/actions-runner-controller/controllers"
	"github.com/actions-runner-controller/actions-runner-controller/github"
	"github.com/actions-runner-controller/actions-runner-controller/logging"
	"github.com/kelseyhightower/envconfig"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	_ "k8s.io/client-go/plugin/pkg/client/auth/gcp"
	ctrl "sigs.k8s.io/controller-runtime"
	// +kubebuilder:scaffold:imports
)

const (
	defaultRunnerImage = "summerwind/actions-runner:latest"
	defaultDockerImage = "docker:dind"
)

var (
	scheme = runtime.NewScheme()
	log    = ctrl.Log.WithName("actions-runner-controller")
)

func init() {
	_ = clientgoscheme.AddToScheme(scheme)

	_ = actionsv1alpha1.AddToScheme(scheme)
	// +kubebuilder:scaffold:scheme
}

type stringSlice []string

func (i *stringSlice) String() string {
	return fmt.Sprintf("%v", *i)
}

func (i *stringSlice) Set(value string) error {
	*i = append(*i, value)
	return nil
}

func main() {
	var (
		err      error
		ghClient *github.Client

		metricsAddr          string
		enableLeaderElection bool
		leaderElectionId     string
		port                 int
		syncPeriod           time.Duration

		gitHubAPICacheDuration time.Duration
		defaultScaleDownDelay  time.Duration

		runnerImage            string
		runnerImagePullSecrets stringSlice

		dockerImage          string
		dockerRegistryMirror string
		namespace            string
		logLevel             string

		runnerHookStorageClass         string
		runnerHookStorageSize          string
		runnerDefaultJobContainerImage string

		commonRunnerLabels commaSeparatedStringSlice
	)

	var c github.Config
	err = envconfig.Process("github", &c)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: processing environment variables: %v\n", err)
		os.Exit(1)
	}

	flag.StringVar(&metricsAddr, "metrics-addr", ":8080", "The address the metric endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "enable-leader-election", false,
		"Enable leader election for controller manager. Enabling this will ensure there is only one active controller manager.")
	flag.StringVar(&leaderElectionId, "leader-election-id", "actions-runner-controller", "Controller id for leader election.")
	flag.StringVar(&runnerImage, "runner-image", defaultRunnerImage, "The image name of self-hosted runner container to use by default if one isn't defined in yaml.")
	flag.StringVar(&dockerImage, "docker-image", defaultDockerImage, "The image name of docker sidecar container to use by default if one isn't defined in yaml.")
	flag.Var(&runnerImagePullSecrets, "runner-image-pull-secret", "The default image-pull secret name for self-hosted runner container.")
	flag.StringVar(&dockerRegistryMirror, "docker-registry-mirror", "", "The default Docker Registry Mirror used by runners.")
	flag.StringVar(&c.Token, "github-token", c.Token, "The personal access token of GitHub.")
	flag.Int64Var(&c.AppID, "github-app-id", c.AppID, "The application ID of GitHub App.")
	flag.Int64Var(&c.AppInstallationID, "github-app-installation-id", c.AppInstallationID, "The installation ID of GitHub App.")
	flag.StringVar(&c.AppPrivateKey, "github-app-private-key", c.AppPrivateKey, "The path of a private key file to authenticate as a GitHub App")
	flag.StringVar(&c.URL, "github-url", c.URL, "GitHub URL to be used for GitHub API calls")
	flag.StringVar(&c.UploadURL, "github-upload-url", c.UploadURL, "GitHub Upload URL to be used for GitHub API calls")
	flag.StringVar(&c.BasicauthUsername, "github-basicauth-username", c.BasicauthUsername, "Username for GitHub basic auth to use instead of PAT or GitHub APP in case it's running behind a proxy API")
	flag.StringVar(&c.BasicauthPassword, "github-basicauth-password", c.BasicauthPassword, "Password for GitHub basic auth to use instead of PAT or GitHub APP in case it's running behind a proxy API")
	flag.StringVar(&c.RunnerGitHubURL, "runner-github-url", c.RunnerGitHubURL, "GitHub URL to be used by runners during registration")
	flag.DurationVar(&gitHubAPICacheDuration, "github-api-cache-duration", 0, "DEPRECATED: The duration until the GitHub API cache expires. Setting this to e.g. 10m results in the controller tries its best not to make the same API call within 10m to reduce the chance of being rate-limited. Defaults to mostly the same value as sync-period. If you're tweaking this in order to make autoscaling more responsive, you'll probably want to tweak sync-period, too")
	flag.DurationVar(&defaultScaleDownDelay, "default-scale-down-delay", controllers.DefaultScaleDownDelay, "The approximate delay for a scale down followed by a scale up, used to prevent flapping (down->up->down->... loop)")
	flag.IntVar(&port, "port", 9443, "The port to which the admission webhook endpoint should bind")
	flag.DurationVar(&syncPeriod, "sync-period", 1*time.Minute, "Determines the minimum frequency at which K8s resources managed by this controller are reconciled.")
	flag.Var(&commonRunnerLabels, "common-runner-labels", "Runner labels in the K1=V1,K2=V2,... format that are inherited all the runners created by the controller. See https://github.com/actions-runner-controller/actions-runner-controller/issues/321 for more information")
	flag.StringVar(&namespace, "watch-namespace", "", "The namespace to watch for custom resources. Set to empty for letting it watch for all namespaces.")
	flag.StringVar(&logLevel, "log-level", logging.LogLevelDebug, `The verbosity of the logging. Valid values are "debug", "info", "warn", "error". Defaults to "debug".`)
	flag.StringVar(&runnerHookStorageClass, "runner-hook-storage-class", "default", "runner hooks storage class defaults to 'default'")
	flag.StringVar(&runnerHookStorageSize, "runner-hook-storage-size", "5Gi", "runner hooks storage size defaults to 5Gi")
	flag.StringVar(&runnerDefaultJobContainerImage, "runner-default-job-container-image", "", "runner default job container image")

	flag.Parse()

	logger := logging.NewLogger(logLevel)

	c.Log = &logger

	ghClient, err = c.NewClient()
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error: Client creation failed.", err)
		os.Exit(1)
	}

	ctrl.SetLogger(logger)

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:             scheme,
		MetricsBindAddress: metricsAddr,
		LeaderElection:     enableLeaderElection,
		LeaderElectionID:   leaderElectionId,
		Port:               port,
		SyncPeriod:         &syncPeriod,
		Namespace:          namespace,
	})
	if err != nil {
		log.Error(err, "unable to start manager")
		os.Exit(1)
	}

	runnerReconciler := &controllers.RunnerReconciler{
		Client:               mgr.GetClient(),
		Log:                  log.WithName("runner"),
		Scheme:               mgr.GetScheme(),
		GitHubClient:         ghClient,
		DockerImage:          dockerImage,
		DockerRegistryMirror: dockerRegistryMirror,
		// Defaults for self-hosted runner containers
		RunnerImage:            runnerImage,
		RunnerImagePullSecrets: runnerImagePullSecrets,

		HookStorageClass:         runnerHookStorageClass,
		HookStorageSize:          runnerHookStorageSize,
		DefaultJobContainerImage: runnerDefaultJobContainerImage,
	}

	if err = runnerReconciler.SetupWithManager(mgr); err != nil {
		log.Error(err, "unable to create controller", "controller", "Runner")
		os.Exit(1)
	}

	runnerReplicaSetReconciler := &controllers.RunnerReplicaSetReconciler{
		Client:       mgr.GetClient(),
		Log:          log.WithName("runnerreplicaset"),
		Scheme:       mgr.GetScheme(),
		GitHubClient: ghClient,
	}

	if err = runnerReplicaSetReconciler.SetupWithManager(mgr); err != nil {
		log.Error(err, "unable to create controller", "controller", "RunnerReplicaSet")
		os.Exit(1)
	}

	runnerDeploymentReconciler := &controllers.RunnerDeploymentReconciler{
		Client:             mgr.GetClient(),
		Log:                log.WithName("runnerdeployment"),
		Scheme:             mgr.GetScheme(),
		CommonRunnerLabels: commonRunnerLabels,
	}

	if err = runnerDeploymentReconciler.SetupWithManager(mgr); err != nil {
		log.Error(err, "unable to create controller", "controller", "RunnerDeployment")
		os.Exit(1)
	}

	runnerSetReconciler := &controllers.RunnerSetReconciler{
		Client:               mgr.GetClient(),
		Log:                  log.WithName("runnerset"),
		Scheme:               mgr.GetScheme(),
		CommonRunnerLabels:   commonRunnerLabels,
		DockerImage:          dockerImage,
		DockerRegistryMirror: dockerRegistryMirror,
		GitHubBaseURL:        ghClient.GithubBaseURL,
		// Defaults for self-hosted runner containers
		RunnerImage:            runnerImage,
		RunnerImagePullSecrets: runnerImagePullSecrets,
	}

	if err = runnerSetReconciler.SetupWithManager(mgr); err != nil {
		log.Error(err, "unable to create controller", "controller", "RunnerSet")
		os.Exit(1)
	}
	if gitHubAPICacheDuration == 0 {
		gitHubAPICacheDuration = syncPeriod - 10*time.Second
	}

	if gitHubAPICacheDuration < 0 {
		gitHubAPICacheDuration = 0
	}

	log.Info(
		"Initializing actions-runner-controller",
		"github-api-cache-duration", gitHubAPICacheDuration,
		"default-scale-down-delay", defaultScaleDownDelay,
		"sync-period", syncPeriod,
		"default-runner-image", runnerImage,
		"default-docker-image", dockerImage,
		"common-runnner-labels", commonRunnerLabels,
		"leader-election-enabled", enableLeaderElection,
		"leader-election-id", leaderElectionId,
		"watch-namespace", namespace,
	)

	horizontalRunnerAutoscaler := &controllers.HorizontalRunnerAutoscalerReconciler{
		Client:                mgr.GetClient(),
		Log:                   log.WithName("horizontalrunnerautoscaler"),
		Scheme:                mgr.GetScheme(),
		GitHubClient:          ghClient,
		CacheDuration:         gitHubAPICacheDuration,
		DefaultScaleDownDelay: defaultScaleDownDelay,
	}

	runnerPodReconciler := &controllers.RunnerPodReconciler{
		Client:       mgr.GetClient(),
		Log:          log.WithName("runnerpod"),
		Scheme:       mgr.GetScheme(),
		GitHubClient: ghClient,
	}

	runnerPersistentVolumeReconciler := &controllers.RunnerPersistentVolumeReconciler{
		Client: mgr.GetClient(),
		Log:    log.WithName("runnerpersistentvolume"),
		Scheme: mgr.GetScheme(),
	}

	runnerPersistentVolumeClaimReconciler := &controllers.RunnerPersistentVolumeClaimReconciler{
		Client: mgr.GetClient(),
		Log:    log.WithName("runnerpersistentvolumeclaim"),
		Scheme: mgr.GetScheme(),
	}

	if err = runnerPodReconciler.SetupWithManager(mgr); err != nil {
		log.Error(err, "unable to create controller", "controller", "RunnerPod")
		os.Exit(1)
	}

	if err = horizontalRunnerAutoscaler.SetupWithManager(mgr); err != nil {
		log.Error(err, "unable to create controller", "controller", "HorizontalRunnerAutoscaler")
		os.Exit(1)
	}

	if err = runnerPersistentVolumeReconciler.SetupWithManager(mgr); err != nil {
		log.Error(err, "unable to create controller", "controller", "RunnerPersistentVolume")
		os.Exit(1)
	}

	if err = runnerPersistentVolumeClaimReconciler.SetupWithManager(mgr); err != nil {
		log.Error(err, "unable to create controller", "controller", "RunnerPersistentVolumeClaim")
		os.Exit(1)
	}

	if err = (&actionsv1alpha1.Runner{}).SetupWebhookWithManager(mgr); err != nil {
		log.Error(err, "unable to create webhook", "webhook", "Runner")
		os.Exit(1)
	}
	if err = (&actionsv1alpha1.RunnerDeployment{}).SetupWebhookWithManager(mgr); err != nil {
		log.Error(err, "unable to create webhook", "webhook", "RunnerDeployment")
		os.Exit(1)
	}
	if err = (&actionsv1alpha1.RunnerReplicaSet{}).SetupWebhookWithManager(mgr); err != nil {
		log.Error(err, "unable to create webhook", "webhook", "RunnerReplicaSet")
		os.Exit(1)
	}
	// +kubebuilder:scaffold:builder

	injector := &controllers.PodRunnerTokenInjector{
		Client:       mgr.GetClient(),
		GitHubClient: ghClient,
		Log:          ctrl.Log.WithName("webhook").WithName("PodRunnerTokenInjector"),
	}
	if err = injector.SetupWithManager(mgr); err != nil {
		log.Error(err, "unable to create webhook server", "webhook", "PodRunnerTokenInjector")
		os.Exit(1)
	}

	log.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		log.Error(err, "problem running manager")
		os.Exit(1)
	}
}

type commaSeparatedStringSlice []string

func (s *commaSeparatedStringSlice) String() string {
	return fmt.Sprintf("%v", *s)
}

func (s *commaSeparatedStringSlice) Set(value string) error {
	for _, v := range strings.Split(value, ",") {
		if v == "" {
			continue
		}

		*s = append(*s, v)
	}
	return nil
}
