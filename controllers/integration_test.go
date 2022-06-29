package controllers

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"time"

	github2 "github.com/actions-runner-controller/actions-runner-controller/github"
	"github.com/google/go-github/v45/github"

	"github.com/actions-runner-controller/actions-runner-controller/github/fake"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	actionsv1alpha1 "github.com/actions-runner-controller/actions-runner-controller/api/v1alpha1"
)

type testEnvironment struct {
	Namespace *corev1.Namespace
	Responses *fake.FixedResponses

	webhookServer    *httptest.Server
	ghClient         *github2.Client
	fakeRunnerList   *fake.RunnersList
	fakeGithubServer *httptest.Server
}

var (
	workflowRunsFor3Replicas             = `{"total_count": 5, "workflow_runs":[{"status":"queued"}, {"status":"queued"}, {"status":"in_progress"}, {"status":"in_progress"}, {"status":"completed"}]}"`
	workflowRunsFor3Replicas_queued      = `{"total_count": 2, "workflow_runs":[{"status":"queued"}, {"status":"queued"}]}"`
	workflowRunsFor3Replicas_in_progress = `{"total_count": 1, "workflow_runs":[{"status":"in_progress"}]}"`
	workflowRunsFor1Replicas             = `{"total_count": 6, "workflow_runs":[{"status":"queued"}, {"status":"completed"}, {"status":"completed"}, {"status":"completed"}, {"status":"completed"}]}"`
	workflowRunsFor1Replicas_queued      = `{"total_count": 1, "workflow_runs":[{"status":"queued"}]}"`
	workflowRunsFor1Replicas_in_progress = `{"total_count": 0, "workflow_runs":[]}"`
)

// SetupIntegrationTest will set up a testing environment.
// This includes:
// * creating a Namespace to be used during the test
// * starting all the reconcilers
// * stopping all the reconcilers after the test ends
// Call this function at the start of each of your tests.
func SetupIntegrationTest(ctx2 context.Context) *testEnvironment {
	var ctx context.Context
	var cancel func()
	ns := &corev1.Namespace{}

	env := &testEnvironment{
		Namespace:     ns,
		webhookServer: nil,
		ghClient:      nil,
	}

	BeforeEach(func() {
		ctx, cancel = context.WithCancel(ctx2)
		*ns = corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: "testns-" + randStringRunes(5)},
		}

		err := k8sClient.Create(ctx, ns)
		Expect(err).NotTo(HaveOccurred(), "failed to create test namespace")

		mgr, err := ctrl.NewManager(cfg, ctrl.Options{
			Namespace: ns.Name,
		})
		Expect(err).NotTo(HaveOccurred(), "failed to create manager")

		responses := &fake.FixedResponses{}
		responses.ListRunners = fake.DefaultListRunnersHandler()
		responses.ListRepositoryWorkflowRuns = &fake.Handler{
			Status: 200,
			Body:   workflowRunsFor3Replicas,
			Statuses: map[string]string{
				"queued":      workflowRunsFor3Replicas_queued,
				"in_progress": workflowRunsFor3Replicas_in_progress,
			},
		}
		fakeRunnerList := fake.NewRunnersList()
		responses.ListRunners = fakeRunnerList.HandleList()
		fakeGithubServer := fake.NewServer(fake.WithFixedResponses(responses))

		env.Responses = responses
		env.fakeRunnerList = fakeRunnerList
		env.fakeGithubServer = fakeGithubServer
		env.ghClient = newGithubClient(fakeGithubServer)

		controllerName := func(name string) string {
			return fmt.Sprintf("%s%s", ns.Name, name)
		}

		runnerController := &RunnerReconciler{
			Client:                      mgr.GetClient(),
			Scheme:                      scheme.Scheme,
			Log:                         logf.Log,
			Recorder:                    mgr.GetEventRecorderFor("runnerreplicaset-controller"),
			GitHubClient:                env.ghClient,
			RunnerImage:                 "example/runner:test",
			DockerImage:                 "example/docker:test",
			Name:                        controllerName("runner"),
			RegistrationRecheckInterval: time.Millisecond * 100,
			RegistrationRecheckJitter:   time.Millisecond * 10,
			UnregistrationRetryDelay:    1 * time.Second,
		}
		err = runnerController.SetupWithManager(mgr)
		Expect(err).NotTo(HaveOccurred(), "failed to setup runner controller")

		replicasetController := &RunnerReplicaSetReconciler{
			Client:       mgr.GetClient(),
			Scheme:       scheme.Scheme,
			Log:          logf.Log,
			Recorder:     mgr.GetEventRecorderFor("runnerreplicaset-controller"),
			GitHubClient: env.ghClient,
			Name:         controllerName("runnerreplicaset"),
		}
		err = replicasetController.SetupWithManager(mgr)
		Expect(err).NotTo(HaveOccurred(), "failed to setup runnerreplicaset controller")

		deploymentsController := &RunnerDeploymentReconciler{
			Client:   mgr.GetClient(),
			Scheme:   scheme.Scheme,
			Log:      logf.Log,
			Recorder: mgr.GetEventRecorderFor("runnerdeployment-controller"),
			Name:     controllerName("runnnerdeployment"),
		}
		err = deploymentsController.SetupWithManager(mgr)
		Expect(err).NotTo(HaveOccurred(), "failed to setup runnerdeployment controller")

		autoscalerController := &HorizontalRunnerAutoscalerReconciler{
			Client:        mgr.GetClient(),
			Scheme:        scheme.Scheme,
			Log:           logf.Log,
			GitHubClient:  env.ghClient,
			Recorder:      mgr.GetEventRecorderFor("horizontalrunnerautoscaler-controller"),
			CacheDuration: 1 * time.Second,
			Name:          controllerName("horizontalrunnerautoscaler"),
		}
		err = autoscalerController.SetupWithManager(mgr)
		Expect(err).NotTo(HaveOccurred(), "failed to setup autoscaler controller")

		autoscalerWebhook := &HorizontalRunnerAutoscalerGitHubWebhook{
			Client:    mgr.GetClient(),
			Scheme:    scheme.Scheme,
			Log:       logf.Log,
			Recorder:  mgr.GetEventRecorderFor("horizontalrunnerautoscaler-controller"),
			Name:      controllerName("horizontalrunnerautoscalergithubwebhook"),
			Namespace: ns.Name,
		}
		err = autoscalerWebhook.SetupWithManager(mgr)
		Expect(err).NotTo(HaveOccurred(), "failed to setup autoscaler webhook")

		mux := http.NewServeMux()
		mux.HandleFunc("/", autoscalerWebhook.Handle)

		env.webhookServer = httptest.NewServer(mux)

		go func() {
			defer GinkgoRecover()

			err := mgr.Start(ctx)
			Expect(err).NotTo(HaveOccurred(), "failed to start manager")
		}()
	})

	AfterEach(func() {
		defer cancel()

		env.fakeGithubServer.Close()
		env.webhookServer.Close()

		err := k8sClient.Delete(ctx, ns)
		Expect(err).NotTo(HaveOccurred(), "failed to delete test namespace")
	})

	return env
}

var _ = Context("INTEGRATION: Inside of a new namespace", func() {
	ctx := context.TODO()
	env := SetupIntegrationTest(ctx)
	ns := env.Namespace

	Describe("when no existing resources exist", func() {

		It("should create and scale organizational runners without any scaling metrics on pull_request event", func() {
			name := "example-runnerdeploy"

			{
				rd := &actionsv1alpha1.RunnerDeployment{
					ObjectMeta: metav1.ObjectMeta{
						Name:      name,
						Namespace: ns.Name,
					},
					Spec: actionsv1alpha1.RunnerDeploymentSpec{
						Replicas: intPtr(1),
						Selector: &metav1.LabelSelector{
							MatchLabels: map[string]string{
								"foo": "bar",
							},
						},
						Template: actionsv1alpha1.RunnerTemplate{
							ObjectMeta: metav1.ObjectMeta{
								Labels: map[string]string{
									"foo": "bar",
								},
							},
							Spec: actionsv1alpha1.RunnerSpec{
								RunnerConfig: actionsv1alpha1.RunnerConfig{
									Organization: "test",
									Image:        "bar",
									Group:        "baz",
								},
								RunnerPodSpec: actionsv1alpha1.RunnerPodSpec{
									Env: []corev1.EnvVar{
										{Name: "FOO", Value: "FOOVALUE"},
									},
								},
							},
						},
					},
				}

				ExpectCreate(ctx, rd, "test RunnerDeployment")
				ExpectRunnerSetsCountEventuallyEquals(ctx, ns.Name, 1)
				ExpectRunnerSetsManagedReplicasCountEventuallyEquals(ctx, ns.Name, 1)
			}

			// Scale-up to 2 replicas
			{
				hra := &actionsv1alpha1.HorizontalRunnerAutoscaler{
					ObjectMeta: metav1.ObjectMeta{
						Name:      name,
						Namespace: ns.Name,
					},
					Spec: actionsv1alpha1.HorizontalRunnerAutoscalerSpec{
						ScaleTargetRef: actionsv1alpha1.ScaleTargetRef{
							Name: name,
						},
						MinReplicas:                       intPtr(2),
						MaxReplicas:                       intPtr(5),
						ScaleDownDelaySecondsAfterScaleUp: intPtr(1),
						Metrics:                           nil,
						ScaleUpTriggers: []actionsv1alpha1.ScaleUpTrigger{
							{
								GitHubEvent: &actionsv1alpha1.GitHubEventScaleUpTriggerSpec{
									PullRequest: &actionsv1alpha1.PullRequestSpec{
										Types:    []string{"created"},
										Branches: []string{"main"},
									},
								},
								Amount:   1,
								Duration: metav1.Duration{Duration: time.Minute},
							},
						},
					},
				}

				ExpectCreate(ctx, hra, "test HorizontalRunnerAutoscaler")

				ExpectRunnerSetsCountEventuallyEquals(ctx, ns.Name, 1)
				ExpectRunnerSetsManagedReplicasCountEventuallyEquals(ctx, ns.Name, 2)
			}

			{
				env.ExpectRegisteredNumberCountEventuallyEquals(2, "count of fake runners after HRA creation")
			}

			// Scale-up to 3 replicas on second pull_request create webhook event
			{
				env.SendOrgPullRequestEvent("test", "valid", "main", "created")
				ExpectRunnerSetsManagedReplicasCountEventuallyEquals(ctx, ns.Name, 3, "runners after second webhook event")
			}
		})

		It("should create and scale organization's repository runners on pull_request event", func() {
			name := "example-runnerdeploy"

			{
				rd := &actionsv1alpha1.RunnerDeployment{
					ObjectMeta: metav1.ObjectMeta{
						Name:      name,
						Namespace: ns.Name,
					},
					Spec: actionsv1alpha1.RunnerDeploymentSpec{
						Replicas: intPtr(1),
						Selector: &metav1.LabelSelector{
							MatchLabels: map[string]string{
								"foo": "bar",
							},
						},
						Template: actionsv1alpha1.RunnerTemplate{
							ObjectMeta: metav1.ObjectMeta{
								Labels: map[string]string{
									"foo": "bar",
								},
							},
							Spec: actionsv1alpha1.RunnerSpec{
								RunnerConfig: actionsv1alpha1.RunnerConfig{
									Repository: "test/valid",
									Image:      "bar",
									Group:      "baz",
								},
								RunnerPodSpec: actionsv1alpha1.RunnerPodSpec{
									Env: []corev1.EnvVar{
										{Name: "FOO", Value: "FOOVALUE"},
									},
								},
							},
						},
					},
				}

				ExpectCreate(ctx, rd, "test RunnerDeployment")
				ExpectRunnerSetsCountEventuallyEquals(ctx, ns.Name, 1)
				ExpectRunnerSetsManagedReplicasCountEventuallyEquals(ctx, ns.Name, 1)
			}

			{
				ExpectRunnerDeploymentEventuallyUpdates(ctx, ns.Name, name, func(rd *actionsv1alpha1.RunnerDeployment) {
					rd.Spec.Replicas = intPtr(2)
				})
				ExpectRunnerSetsCountEventuallyEquals(ctx, ns.Name, 1)
				ExpectRunnerSetsManagedReplicasCountEventuallyEquals(ctx, ns.Name, 2)
			}

			// Scale-up to 3 replicas
			{
				hra := &actionsv1alpha1.HorizontalRunnerAutoscaler{
					ObjectMeta: metav1.ObjectMeta{
						Name:      name,
						Namespace: ns.Name,
					},
					Spec: actionsv1alpha1.HorizontalRunnerAutoscalerSpec{
						ScaleTargetRef: actionsv1alpha1.ScaleTargetRef{
							Name: name,
						},
						MinReplicas:                       intPtr(1),
						MaxReplicas:                       intPtr(3),
						ScaleDownDelaySecondsAfterScaleUp: intPtr(1),
						Metrics: []actionsv1alpha1.MetricSpec{
							{
								Type: actionsv1alpha1.AutoscalingMetricTypeTotalNumberOfQueuedAndInProgressWorkflowRuns,
							},
						},
						ScaleUpTriggers: []actionsv1alpha1.ScaleUpTrigger{
							{
								GitHubEvent: &actionsv1alpha1.GitHubEventScaleUpTriggerSpec{
									PullRequest: &actionsv1alpha1.PullRequestSpec{
										Types:    []string{"created"},
										Branches: []string{"main"},
									},
								},
								Amount:   1,
								Duration: metav1.Duration{Duration: time.Minute},
							},
						},
					},
				}

				ExpectCreate(ctx, hra, "test HorizontalRunnerAutoscaler")

				ExpectRunnerSetsCountEventuallyEquals(ctx, ns.Name, 1)
				ExpectRunnerSetsManagedReplicasCountEventuallyEquals(ctx, ns.Name, 3)
			}

			{
				env.ExpectRegisteredNumberCountEventuallyEquals(3, "count of fake runners after HRA creation")
			}

			// Scale-down to 1 replica
			{
				time.Sleep(time.Second)

				env.Responses.ListRepositoryWorkflowRuns.Body = workflowRunsFor1Replicas
				env.Responses.ListRepositoryWorkflowRuns.Statuses["queued"] = workflowRunsFor1Replicas_queued
				env.Responses.ListRepositoryWorkflowRuns.Statuses["in_progress"] = workflowRunsFor1Replicas_in_progress

				var hra actionsv1alpha1.HorizontalRunnerAutoscaler

				err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns.Name, Name: name}, &hra)

				Expect(err).NotTo(HaveOccurred(), "failed to get test HorizontalRunnerAutoscaler resource")

				hra.Annotations = map[string]string{
					"force-update": "1",
				}

				err = k8sClient.Update(ctx, &hra)

				Expect(err).NotTo(HaveOccurred(), "failed to get test HorizontalRunnerAutoscaler resource")

				ExpectRunnerSetsManagedReplicasCountEventuallyEquals(ctx, ns.Name, 1, "runners after HRA force update for scale-down")
			}

			// Scale-up to 2 replicas on first pull_request create webhook event
			{
				env.SendOrgPullRequestEvent("test", "valid", "main", "created")
				ExpectRunnerSetsCountEventuallyEquals(ctx, ns.Name, 1, "runner sets after webhook")
				ExpectRunnerSetsManagedReplicasCountEventuallyEquals(ctx, ns.Name, 2, "runners after first webhook event")
			}

			// Scale-up to 3 replicas on second pull_request create webhook event
			{
				env.SendOrgPullRequestEvent("test", "valid", "main", "created")
				ExpectRunnerSetsManagedReplicasCountEventuallyEquals(ctx, ns.Name, 3, "runners after second webhook event")
			}
		})

		It("should create and scale organization's repository runners on check_run event", func() {
			name := "example-runnerdeploy"

			{
				rd := &actionsv1alpha1.RunnerDeployment{
					ObjectMeta: metav1.ObjectMeta{
						Name:      name,
						Namespace: ns.Name,
					},
					Spec: actionsv1alpha1.RunnerDeploymentSpec{
						Replicas: intPtr(1),
						Selector: &metav1.LabelSelector{
							MatchLabels: map[string]string{
								"foo": "bar",
							},
						},
						Template: actionsv1alpha1.RunnerTemplate{
							ObjectMeta: metav1.ObjectMeta{
								Labels: map[string]string{
									"foo": "bar",
								},
							},
							Spec: actionsv1alpha1.RunnerSpec{
								RunnerConfig: actionsv1alpha1.RunnerConfig{
									Repository: "test/valid",
									Image:      "bar",
									Group:      "baz",
								},
								RunnerPodSpec: actionsv1alpha1.RunnerPodSpec{
									Env: []corev1.EnvVar{
										{Name: "FOO", Value: "FOOVALUE"},
									},
								},
							},
						},
					},
				}

				ExpectCreate(ctx, rd, "test RunnerDeployment")
				ExpectRunnerSetsCountEventuallyEquals(ctx, ns.Name, 1)
				ExpectRunnerSetsManagedReplicasCountEventuallyEquals(ctx, ns.Name, 1)
				env.ExpectRegisteredNumberCountEventuallyEquals(1, "count of fake list runners")
			}

			// Scale-up to 3 replicas by the default TotalNumberOfQueuedAndInProgressWorkflowRuns-based scaling
			// See workflowRunsFor3Replicas_queued and workflowRunsFor3Replicas_in_progress for GitHub List-Runners API responses
			// used while testing.
			{
				hra := &actionsv1alpha1.HorizontalRunnerAutoscaler{
					ObjectMeta: metav1.ObjectMeta{
						Name:      name,
						Namespace: ns.Name,
					},
					Spec: actionsv1alpha1.HorizontalRunnerAutoscalerSpec{
						ScaleTargetRef: actionsv1alpha1.ScaleTargetRef{
							Name: name,
						},
						MinReplicas:                       intPtr(1),
						MaxReplicas:                       intPtr(5),
						ScaleDownDelaySecondsAfterScaleUp: intPtr(1),
						Metrics: []actionsv1alpha1.MetricSpec{
							{
								Type: actionsv1alpha1.AutoscalingMetricTypeTotalNumberOfQueuedAndInProgressWorkflowRuns,
							},
						},
						ScaleUpTriggers: []actionsv1alpha1.ScaleUpTrigger{
							{
								GitHubEvent: &actionsv1alpha1.GitHubEventScaleUpTriggerSpec{
									CheckRun: &actionsv1alpha1.CheckRunSpec{
										Types:        []string{"created"},
										Status:       "pending",
										Repositories: []string{"valid", "foo", "bar"},
									},
								},
								Amount:   1,
								Duration: metav1.Duration{Duration: time.Minute},
							},
						},
					},
				}

				ExpectCreate(ctx, hra, "test HorizontalRunnerAutoscaler")

				ExpectRunnerSetsCountEventuallyEquals(ctx, ns.Name, 1)
				ExpectRunnerSetsManagedReplicasCountEventuallyEquals(ctx, ns.Name, 3)
				env.ExpectRegisteredNumberCountEventuallyEquals(3, "count of fake list runners")
				env.SyncRunnerRegistrations()
				ExpectRunnerCountEventuallyEquals(ctx, ns.Name, 3)
			}

			// Scale-up to 4 replicas on first check_run create webhook event
			{
				env.SendOrgCheckRunEvent("test", "valid", "pending", "created")
				ExpectRunnerSetsCountEventuallyEquals(ctx, ns.Name, 1, "runner sets after webhook")
				ExpectRunnerSetsManagedReplicasCountEventuallyEquals(ctx, ns.Name, 4, "runners after first webhook event")
				env.ExpectRegisteredNumberCountEventuallyEquals(4, "count of fake list runners")
				env.SyncRunnerRegistrations()
				ExpectRunnerCountEventuallyEquals(ctx, ns.Name, 4)
			}

			// Scale-up to 5 replicas on second check_run create webhook event
			replicasAfterSecondWebhook := 5
			{
				env.SendOrgCheckRunEvent("test", "valid", "pending", "created")
				ExpectRunnerSetsManagedReplicasCountEventuallyEquals(ctx, ns.Name, replicasAfterSecondWebhook, "runners after second webhook event")
				env.ExpectRegisteredNumberCountEventuallyEquals(replicasAfterSecondWebhook, "count of fake list runners")
				env.SyncRunnerRegistrations()
				ExpectRunnerCountEventuallyEquals(ctx, ns.Name, replicasAfterSecondWebhook)
			}

			// Do not scale-up on third check_run create webhook event
			// example repo is not in specified in actionsv1alpha1.CheckRunSpec.Repositories
			{
				env.SendOrgCheckRunEvent("test", "example", "pending", "created")
				ExpectRunnerSetsManagedReplicasCountEventuallyEquals(ctx, ns.Name, replicasAfterSecondWebhook, "runners after third webhook event")
				env.ExpectRegisteredNumberCountEventuallyEquals(replicasAfterSecondWebhook, "count of fake list runners")
				env.SyncRunnerRegistrations()
				ExpectRunnerCountEventuallyEquals(ctx, ns.Name, replicasAfterSecondWebhook)
			}
		})

		It("should create and scale organization's repository runners on workflow_job event", func() {
			name := "example-runnerdeploy"

			{
				rd := &actionsv1alpha1.RunnerDeployment{
					ObjectMeta: metav1.ObjectMeta{
						Name:      name,
						Namespace: ns.Name,
					},
					Spec: actionsv1alpha1.RunnerDeploymentSpec{
						Replicas: intPtr(1),
						Selector: &metav1.LabelSelector{
							MatchLabels: map[string]string{
								"foo": "bar",
							},
						},
						Template: actionsv1alpha1.RunnerTemplate{
							ObjectMeta: metav1.ObjectMeta{
								Labels: map[string]string{
									"foo": "bar",
								},
							},
							Spec: actionsv1alpha1.RunnerSpec{
								RunnerConfig: actionsv1alpha1.RunnerConfig{
									Repository: "test/valid",
									Image:      "bar",
									Group:      "baz",
								},
								RunnerPodSpec: actionsv1alpha1.RunnerPodSpec{
									Env: []corev1.EnvVar{
										{Name: "FOO", Value: "FOOVALUE"},
									},
								},
							},
						},
					},
				}

				ExpectCreate(ctx, rd, "test RunnerDeployment")
				ExpectRunnerSetsCountEventuallyEquals(ctx, ns.Name, 1)
				ExpectRunnerSetsManagedReplicasCountEventuallyEquals(ctx, ns.Name, 1)
				env.ExpectRegisteredNumberCountEventuallyEquals(1, "count of fake list runners")
			}

			// Scale-up to 1 replica via ScaleUpTriggers.GitHubEvent.WorkflowJob based scaling
			{
				hra := &actionsv1alpha1.HorizontalRunnerAutoscaler{
					ObjectMeta: metav1.ObjectMeta{
						Name:      name,
						Namespace: ns.Name,
					},
					Spec: actionsv1alpha1.HorizontalRunnerAutoscalerSpec{
						ScaleTargetRef: actionsv1alpha1.ScaleTargetRef{
							Name: name,
						},
						MinReplicas:                       intPtr(1),
						MaxReplicas:                       intPtr(5),
						ScaleDownDelaySecondsAfterScaleUp: intPtr(1),
						ScaleUpTriggers: []actionsv1alpha1.ScaleUpTrigger{
							{
								GitHubEvent: &actionsv1alpha1.GitHubEventScaleUpTriggerSpec{
									WorkflowJob: &actionsv1alpha1.WorkflowJobSpec{},
								},
								Amount:   1,
								Duration: metav1.Duration{Duration: time.Minute},
							},
						},
					},
				}

				ExpectCreate(ctx, hra, "test HorizontalRunnerAutoscaler")

				ExpectRunnerSetsCountEventuallyEquals(ctx, ns.Name, 1)
				ExpectRunnerSetsManagedReplicasCountEventuallyEquals(ctx, ns.Name, 1)
				env.ExpectRegisteredNumberCountEventuallyEquals(1, "count of fake list runners")
			}

			// Scale-up to 2 replicas on first workflow_job.queued webhook event
			{
				env.SendWorkflowJobEvent("test", "valid", "queued", []string{"self-hosted"})
				ExpectRunnerSetsManagedReplicasCountEventuallyEquals(ctx, ns.Name, 2, "runners after first webhook event")
				env.ExpectRegisteredNumberCountEventuallyEquals(2, "count of fake list runners")
			}

			// Scale-up to 3 replicas on second workflow_job.queued webhook event
			{
				env.SendWorkflowJobEvent("test", "valid", "queued", []string{"self-hosted"})
				ExpectRunnerSetsManagedReplicasCountEventuallyEquals(ctx, ns.Name, 3, "runners after second webhook event")
				env.ExpectRegisteredNumberCountEventuallyEquals(3, "count of fake list runners")
			}

			// Do not scale-up on third workflow_job.queued webhook event
			// repo "example" doesn't match our Spec
			{
				env.SendWorkflowJobEvent("test", "example", "queued", []string{"self-hosted"})
				ExpectRunnerSetsManagedReplicasCountEventuallyEquals(ctx, ns.Name, 3, "runners after third webhook event")
				env.ExpectRegisteredNumberCountEventuallyEquals(3, "count of fake list runners")
			}
		})

		It("should create and scale organization's repository runners only on check_run event", func() {
			name := "example-runnerdeploy"

			{
				rd := &actionsv1alpha1.RunnerDeployment{
					ObjectMeta: metav1.ObjectMeta{
						Name:      name,
						Namespace: ns.Name,
					},
					Spec: actionsv1alpha1.RunnerDeploymentSpec{
						Replicas: intPtr(1),
						Selector: &metav1.LabelSelector{
							MatchLabels: map[string]string{
								"foo": "bar",
							},
						},
						Template: actionsv1alpha1.RunnerTemplate{
							ObjectMeta: metav1.ObjectMeta{
								Labels: map[string]string{
									"foo": "bar",
								},
							},
							Spec: actionsv1alpha1.RunnerSpec{
								RunnerConfig: actionsv1alpha1.RunnerConfig{
									Repository: "test/valid",
									Image:      "bar",
									Group:      "baz",
								},
								RunnerPodSpec: actionsv1alpha1.RunnerPodSpec{
									Env: []corev1.EnvVar{
										{Name: "FOO", Value: "FOOVALUE"},
									},
								},
							},
						},
					},
				}

				ExpectCreate(ctx, rd, "test RunnerDeployment")
				ExpectRunnerSetsCountEventuallyEquals(ctx, ns.Name, 1)
				ExpectRunnerSetsManagedReplicasCountEventuallyEquals(ctx, ns.Name, 1)
				env.ExpectRegisteredNumberCountEventuallyEquals(1, "count of fake list runners")
			}

			// Scale-up to 1 replica via ScaleUpTriggers.GitHubEvent.CheckRun based scaling
			{
				hra := &actionsv1alpha1.HorizontalRunnerAutoscaler{
					ObjectMeta: metav1.ObjectMeta{
						Name:      name,
						Namespace: ns.Name,
					},
					Spec: actionsv1alpha1.HorizontalRunnerAutoscalerSpec{
						ScaleTargetRef: actionsv1alpha1.ScaleTargetRef{
							Name: name,
						},
						MinReplicas:                       intPtr(1),
						MaxReplicas:                       intPtr(5),
						ScaleDownDelaySecondsAfterScaleUp: intPtr(1),
						ScaleUpTriggers: []actionsv1alpha1.ScaleUpTrigger{
							{
								GitHubEvent: &actionsv1alpha1.GitHubEventScaleUpTriggerSpec{
									CheckRun: &actionsv1alpha1.CheckRunSpec{
										Types:  []string{"created"},
										Status: "pending",
									},
								},
								Amount:   1,
								Duration: metav1.Duration{Duration: time.Minute},
							},
						},
					},
				}

				ExpectCreate(ctx, hra, "test HorizontalRunnerAutoscaler")

				ExpectRunnerSetsCountEventuallyEquals(ctx, ns.Name, 1)
				ExpectRunnerSetsManagedReplicasCountEventuallyEquals(ctx, ns.Name, 1)
				env.ExpectRegisteredNumberCountEventuallyEquals(1, "count of fake list runners")
			}

			// Scale-up to 2 replicas on first check_run create webhook event
			{
				env.SendOrgCheckRunEvent("test", "valid", "pending", "created")
				ExpectRunnerSetsCountEventuallyEquals(ctx, ns.Name, 1, "runner sets after webhook")
				ExpectRunnerSetsManagedReplicasCountEventuallyEquals(ctx, ns.Name, 2, "runners after first webhook event")
				env.ExpectRegisteredNumberCountEventuallyEquals(2, "count of fake list runners")
			}

			// Scale-up to 3 replicas on second check_run create webhook event
			{
				env.SendOrgCheckRunEvent("test", "valid", "pending", "created")
				ExpectRunnerSetsManagedReplicasCountEventuallyEquals(ctx, ns.Name, 3, "runners after second webhook event")
				env.ExpectRegisteredNumberCountEventuallyEquals(3, "count of fake list runners")
			}
		})

		It("should create and scale user's repository runners on pull_request event", func() {
			name := "example-runnerdeploy"

			{
				rd := &actionsv1alpha1.RunnerDeployment{
					ObjectMeta: metav1.ObjectMeta{
						Name:      name,
						Namespace: ns.Name,
					},
					Spec: actionsv1alpha1.RunnerDeploymentSpec{
						Replicas: intPtr(1),
						Selector: &metav1.LabelSelector{
							MatchLabels: map[string]string{
								"foo": "bar",
							},
						},
						Template: actionsv1alpha1.RunnerTemplate{
							ObjectMeta: metav1.ObjectMeta{
								Labels: map[string]string{
									"foo": "bar",
								},
							},
							Spec: actionsv1alpha1.RunnerSpec{
								RunnerConfig: actionsv1alpha1.RunnerConfig{
									Repository: "test/valid",
									Image:      "bar",
									Group:      "baz",
								},
								RunnerPodSpec: actionsv1alpha1.RunnerPodSpec{
									Env: []corev1.EnvVar{
										{Name: "FOO", Value: "FOOVALUE"},
									},
								},
							},
						},
					},
				}

				ExpectCreate(ctx, rd, "test RunnerDeployment")
				ExpectRunnerSetsCountEventuallyEquals(ctx, ns.Name, 1)
				ExpectRunnerSetsManagedReplicasCountEventuallyEquals(ctx, ns.Name, 1)
			}

			{
				ExpectRunnerDeploymentEventuallyUpdates(ctx, ns.Name, name, func(rd *actionsv1alpha1.RunnerDeployment) {
					rd.Spec.Replicas = intPtr(2)
				})
				ExpectRunnerSetsCountEventuallyEquals(ctx, ns.Name, 1)
				ExpectRunnerSetsManagedReplicasCountEventuallyEquals(ctx, ns.Name, 2)
			}

			// Scale-up to 3 replicas
			{
				hra := &actionsv1alpha1.HorizontalRunnerAutoscaler{
					ObjectMeta: metav1.ObjectMeta{
						Name:      name,
						Namespace: ns.Name,
					},
					Spec: actionsv1alpha1.HorizontalRunnerAutoscalerSpec{
						ScaleTargetRef: actionsv1alpha1.ScaleTargetRef{
							Name: name,
						},
						MinReplicas:                       intPtr(1),
						MaxReplicas:                       intPtr(3),
						ScaleDownDelaySecondsAfterScaleUp: intPtr(1),
						Metrics: []actionsv1alpha1.MetricSpec{
							{
								Type: actionsv1alpha1.AutoscalingMetricTypeTotalNumberOfQueuedAndInProgressWorkflowRuns,
							},
						},
						ScaleUpTriggers: []actionsv1alpha1.ScaleUpTrigger{
							{
								GitHubEvent: &actionsv1alpha1.GitHubEventScaleUpTriggerSpec{
									PullRequest: &actionsv1alpha1.PullRequestSpec{
										Types:    []string{"created"},
										Branches: []string{"main"},
									},
								},
								Amount:   1,
								Duration: metav1.Duration{Duration: time.Minute},
							},
						},
					},
				}

				ExpectCreate(ctx, hra, "test HorizontalRunnerAutoscaler")

				ExpectRunnerSetsCountEventuallyEquals(ctx, ns.Name, 1)
				ExpectRunnerSetsManagedReplicasCountEventuallyEquals(ctx, ns.Name, 3)
			}

			{
				env.ExpectRegisteredNumberCountEventuallyEquals(3, "count of fake runners after HRA creation")
			}

			// Scale-down to 1 replica
			{
				time.Sleep(time.Second)

				env.Responses.ListRepositoryWorkflowRuns.Body = workflowRunsFor1Replicas
				env.Responses.ListRepositoryWorkflowRuns.Statuses["queued"] = workflowRunsFor1Replicas_queued
				env.Responses.ListRepositoryWorkflowRuns.Statuses["in_progress"] = workflowRunsFor1Replicas_in_progress

				var hra actionsv1alpha1.HorizontalRunnerAutoscaler

				err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns.Name, Name: name}, &hra)

				Expect(err).NotTo(HaveOccurred(), "failed to get test HorizontalRunnerAutoscaler resource")

				hra.Annotations = map[string]string{
					"force-update": "1",
				}

				err = k8sClient.Update(ctx, &hra)

				Expect(err).NotTo(HaveOccurred(), "failed to get test HorizontalRunnerAutoscaler resource")

				ExpectRunnerSetsManagedReplicasCountEventuallyEquals(ctx, ns.Name, 1, "runners after HRA force update for scale-down")
				ExpectHRADesiredReplicasEquals(ctx, ns.Name, name, 1, "runner deployment desired replicas")
			}

			// Scale-up to 2 replicas on first pull_request create webhook event
			{
				env.SendUserPullRequestEvent("test", "valid", "main", "created")
				ExpectRunnerSetsCountEventuallyEquals(ctx, ns.Name, 1, "runner sets after webhook")
				ExpectRunnerSetsManagedReplicasCountEventuallyEquals(ctx, ns.Name, 2, "runners after first webhook event")
				ExpectHRADesiredReplicasEquals(ctx, ns.Name, name, 2, "runner deployment desired replicas")
			}

			// Scale-up to 3 replicas on second pull_request create webhook event
			{
				env.SendUserPullRequestEvent("test", "valid", "main", "created")
				ExpectRunnerSetsManagedReplicasCountEventuallyEquals(ctx, ns.Name, 3, "runners after second webhook event")
				ExpectHRADesiredReplicasEquals(ctx, ns.Name, name, 3, "runner deployment desired replicas")
			}
		})

		It("should create and scale user's repository runners only on pull_request event", func() {
			name := "example-runnerdeploy"

			{
				rd := &actionsv1alpha1.RunnerDeployment{
					ObjectMeta: metav1.ObjectMeta{
						Name:      name,
						Namespace: ns.Name,
					},
					Spec: actionsv1alpha1.RunnerDeploymentSpec{
						Replicas: intPtr(1),
						Selector: &metav1.LabelSelector{
							MatchLabels: map[string]string{
								"foo": "bar",
							},
						},
						Template: actionsv1alpha1.RunnerTemplate{
							ObjectMeta: metav1.ObjectMeta{
								Labels: map[string]string{
									"foo": "bar",
								},
							},
							Spec: actionsv1alpha1.RunnerSpec{
								RunnerConfig: actionsv1alpha1.RunnerConfig{
									Repository: "test/valid",
									Image:      "bar",
									Group:      "baz",
								},
								RunnerPodSpec: actionsv1alpha1.RunnerPodSpec{
									Env: []corev1.EnvVar{
										{Name: "FOO", Value: "FOOVALUE"},
									},
								},
							},
						},
					},
				}

				ExpectCreate(ctx, rd, "test RunnerDeployment")
				ExpectRunnerSetsCountEventuallyEquals(ctx, ns.Name, 1)
				ExpectRunnerSetsManagedReplicasCountEventuallyEquals(ctx, ns.Name, 1)
			}

			{
				hra := &actionsv1alpha1.HorizontalRunnerAutoscaler{
					ObjectMeta: metav1.ObjectMeta{
						Name:      name,
						Namespace: ns.Name,
					},
					Spec: actionsv1alpha1.HorizontalRunnerAutoscalerSpec{
						ScaleTargetRef: actionsv1alpha1.ScaleTargetRef{
							Name: name,
						},
						MinReplicas:                       intPtr(1),
						MaxReplicas:                       intPtr(3),
						ScaleDownDelaySecondsAfterScaleUp: intPtr(1),
						ScaleUpTriggers: []actionsv1alpha1.ScaleUpTrigger{
							{
								GitHubEvent: &actionsv1alpha1.GitHubEventScaleUpTriggerSpec{
									PullRequest: &actionsv1alpha1.PullRequestSpec{
										Types:    []string{"created"},
										Branches: []string{"main"},
									},
								},
								Amount:   1,
								Duration: metav1.Duration{Duration: time.Minute},
							},
						},
					},
				}

				ExpectCreate(ctx, hra, "test HorizontalRunnerAutoscaler")

				ExpectRunnerSetsCountEventuallyEquals(ctx, ns.Name, 1)
				ExpectRunnerSetsManagedReplicasCountEventuallyEquals(ctx, ns.Name, 1)
			}

			{
				env.ExpectRegisteredNumberCountEventuallyEquals(1, "count of fake runners after HRA creation")
			}

			// Scale-up to 2 replicas on first pull_request create webhook event
			{
				env.SendUserPullRequestEvent("test", "valid", "main", "created")
				ExpectRunnerSetsCountEventuallyEquals(ctx, ns.Name, 1, "runner sets after webhook")
				ExpectRunnerSetsManagedReplicasCountEventuallyEquals(ctx, ns.Name, 2, "runners after first webhook event")
				ExpectHRADesiredReplicasEquals(ctx, ns.Name, name, 2, "runner deployment desired replicas")
			}

			// Scale-up to 3 replicas on second pull_request create webhook event
			{
				env.SendUserPullRequestEvent("test", "valid", "main", "created")
				ExpectRunnerSetsManagedReplicasCountEventuallyEquals(ctx, ns.Name, 3, "runners after second webhook event")
				ExpectHRADesiredReplicasEquals(ctx, ns.Name, name, 3, "runner deployment desired replicas")
			}
		})

		It("should create and scale user's repository runners on check_run event", func() {
			name := "example-runnerdeploy"

			{
				rd := &actionsv1alpha1.RunnerDeployment{
					ObjectMeta: metav1.ObjectMeta{
						Name:      name,
						Namespace: ns.Name,
					},
					Spec: actionsv1alpha1.RunnerDeploymentSpec{
						Replicas: intPtr(1),
						Selector: &metav1.LabelSelector{
							MatchLabels: map[string]string{
								"foo": "bar",
							},
						},
						Template: actionsv1alpha1.RunnerTemplate{
							ObjectMeta: metav1.ObjectMeta{
								Labels: map[string]string{
									"foo": "bar",
								},
							},
							Spec: actionsv1alpha1.RunnerSpec{
								RunnerConfig: actionsv1alpha1.RunnerConfig{
									Repository: "test/valid",
									Image:      "bar",
									Group:      "baz",
								},
								RunnerPodSpec: actionsv1alpha1.RunnerPodSpec{
									Env: []corev1.EnvVar{
										{Name: "FOO", Value: "FOOVALUE"},
									},
								},
							},
						},
					},
				}

				ExpectCreate(ctx, rd, "test RunnerDeployment")
				ExpectRunnerSetsCountEventuallyEquals(ctx, ns.Name, 1)
				ExpectRunnerSetsManagedReplicasCountEventuallyEquals(ctx, ns.Name, 1)
				env.ExpectRegisteredNumberCountEventuallyEquals(1, "count of fake list runners")
			}

			// Scale-up to 3 replicas by the default TotalNumberOfQueuedAndInProgressWorkflowRuns-based scaling
			// See workflowRunsFor3Replicas_queued and workflowRunsFor3Replicas_in_progress for GitHub List-Runners API responses
			// used while testing.
			{
				hra := &actionsv1alpha1.HorizontalRunnerAutoscaler{
					ObjectMeta: metav1.ObjectMeta{
						Name:      name,
						Namespace: ns.Name,
					},
					Spec: actionsv1alpha1.HorizontalRunnerAutoscalerSpec{
						ScaleTargetRef: actionsv1alpha1.ScaleTargetRef{
							Name: name,
						},
						MinReplicas:                       intPtr(1),
						MaxReplicas:                       intPtr(5),
						ScaleDownDelaySecondsAfterScaleUp: intPtr(1),
						Metrics: []actionsv1alpha1.MetricSpec{
							{
								Type: actionsv1alpha1.AutoscalingMetricTypeTotalNumberOfQueuedAndInProgressWorkflowRuns,
							},
						},
						ScaleUpTriggers: []actionsv1alpha1.ScaleUpTrigger{
							{
								GitHubEvent: &actionsv1alpha1.GitHubEventScaleUpTriggerSpec{
									CheckRun: &actionsv1alpha1.CheckRunSpec{
										Types:  []string{"created"},
										Status: "pending",
									},
								},
								Amount:   1,
								Duration: metav1.Duration{Duration: time.Minute},
							},
						},
					},
				}

				ExpectCreate(ctx, hra, "test HorizontalRunnerAutoscaler")

				ExpectRunnerSetsCountEventuallyEquals(ctx, ns.Name, 1)
				ExpectRunnerSetsManagedReplicasCountEventuallyEquals(ctx, ns.Name, 3)
				env.ExpectRegisteredNumberCountEventuallyEquals(3, "count of fake list runners")
			}

			// Scale-up to 4 replicas on first check_run create webhook event
			{
				env.SendUserCheckRunEvent("test", "valid", "pending", "created")
				ExpectRunnerSetsCountEventuallyEquals(ctx, ns.Name, 1, "runner sets after webhook")
				ExpectRunnerSetsManagedReplicasCountEventuallyEquals(ctx, ns.Name, 4, "runners after first webhook event")
				env.ExpectRegisteredNumberCountEventuallyEquals(4, "count of fake list runners")
			}

			// Scale-up to 5 replicas on second check_run create webhook event
			{
				env.SendUserCheckRunEvent("test", "valid", "pending", "created")
				ExpectRunnerSetsManagedReplicasCountEventuallyEquals(ctx, ns.Name, 5, "runners after second webhook event")
				env.ExpectRegisteredNumberCountEventuallyEquals(5, "count of fake list runners")
			}
		})

		It("should create and scale user's repository runners only on check_run event", func() {
			name := "example-runnerdeploy"

			{
				rd := &actionsv1alpha1.RunnerDeployment{
					ObjectMeta: metav1.ObjectMeta{
						Name:      name,
						Namespace: ns.Name,
					},
					Spec: actionsv1alpha1.RunnerDeploymentSpec{
						Replicas: intPtr(1),
						Selector: &metav1.LabelSelector{
							MatchLabels: map[string]string{
								"foo": "bar",
							},
						},
						Template: actionsv1alpha1.RunnerTemplate{
							ObjectMeta: metav1.ObjectMeta{
								Labels: map[string]string{
									"foo": "bar",
								},
							},
							Spec: actionsv1alpha1.RunnerSpec{
								RunnerConfig: actionsv1alpha1.RunnerConfig{
									Repository: "test/valid",
									Image:      "bar",
									Group:      "baz",
								},
								RunnerPodSpec: actionsv1alpha1.RunnerPodSpec{
									Env: []corev1.EnvVar{
										{Name: "FOO", Value: "FOOVALUE"},
									},
								},
							},
						},
					},
				}

				ExpectCreate(ctx, rd, "test RunnerDeployment")
				ExpectRunnerSetsCountEventuallyEquals(ctx, ns.Name, 1)
				ExpectRunnerSetsManagedReplicasCountEventuallyEquals(ctx, ns.Name, 1)
			}

			{
				env.ExpectRegisteredNumberCountEventuallyEquals(1, "count of fake list runners")
			}

			// Scale-up to 3 replicas by the default TotalNumberOfQueuedAndInProgressWorkflowRuns-based scaling
			// See workflowRunsFor3Replicas_queued and workflowRunsFor3Replicas_in_progress for GitHub List-Runners API responses
			// used while testing.
			{
				hra := &actionsv1alpha1.HorizontalRunnerAutoscaler{
					ObjectMeta: metav1.ObjectMeta{
						Name:      name,
						Namespace: ns.Name,
					},
					Spec: actionsv1alpha1.HorizontalRunnerAutoscalerSpec{
						ScaleTargetRef: actionsv1alpha1.ScaleTargetRef{
							Name: name,
						},
						MinReplicas:                       intPtr(1),
						MaxReplicas:                       intPtr(5),
						ScaleDownDelaySecondsAfterScaleUp: intPtr(1),
						ScaleUpTriggers: []actionsv1alpha1.ScaleUpTrigger{
							{
								GitHubEvent: &actionsv1alpha1.GitHubEventScaleUpTriggerSpec{
									CheckRun: &actionsv1alpha1.CheckRunSpec{
										Types:  []string{"created"},
										Status: "pending",
									},
								},
								Amount:   1,
								Duration: metav1.Duration{Duration: time.Minute},
							},
						},
					},
				}

				ExpectCreate(ctx, hra, "test HorizontalRunnerAutoscaler")

				ExpectRunnerSetsCountEventuallyEquals(ctx, ns.Name, 1)
				ExpectRunnerSetsManagedReplicasCountEventuallyEquals(ctx, ns.Name, 1)
			}

			{
				env.ExpectRegisteredNumberCountEventuallyEquals(1, "count of fake list runners")
			}

			// Scale-up to 2 replicas on first check_run create webhook event
			{
				env.SendUserCheckRunEvent("test", "valid", "pending", "created")
				ExpectRunnerSetsCountEventuallyEquals(ctx, ns.Name, 1, "runner sets after webhook")
				ExpectRunnerSetsManagedReplicasCountEventuallyEquals(ctx, ns.Name, 2, "runners after first webhook event")
				env.ExpectRegisteredNumberCountEventuallyEquals(2, "count of fake list runners")
			}

			// Scale-up to 3 replicas on second check_run create webhook event
			{
				env.SendUserCheckRunEvent("test", "valid", "pending", "created")
				ExpectRunnerSetsManagedReplicasCountEventuallyEquals(ctx, ns.Name, 3, "runners after second webhook event")
				env.ExpectRegisteredNumberCountEventuallyEquals(3, "count of fake list runners")
			}
		})

		It("should be able to scale visible organization runner group with default labels", func() {
			name := "example-runnerdeploy"

			{
				rd := &actionsv1alpha1.RunnerDeployment{
					ObjectMeta: metav1.ObjectMeta{
						Name:      name,
						Namespace: ns.Name,
					},
					Spec: actionsv1alpha1.RunnerDeploymentSpec{
						Replicas: intPtr(1),
						Selector: &metav1.LabelSelector{
							MatchLabels: map[string]string{
								"foo": "bar",
							},
						},
						Template: actionsv1alpha1.RunnerTemplate{
							ObjectMeta: metav1.ObjectMeta{
								Labels: map[string]string{
									"foo": "bar",
								},
							},
							Spec: actionsv1alpha1.RunnerSpec{
								RunnerConfig: actionsv1alpha1.RunnerConfig{
									Repository: "test/valid",
									Image:      "bar",
									Group:      "baz",
								},
								RunnerPodSpec: actionsv1alpha1.RunnerPodSpec{
									Env: []corev1.EnvVar{
										{Name: "FOO", Value: "FOOVALUE"},
									},
								},
							},
						},
					},
				}

				ExpectCreate(ctx, rd, "test RunnerDeployment")

				hra := &actionsv1alpha1.HorizontalRunnerAutoscaler{
					ObjectMeta: metav1.ObjectMeta{
						Name:      name,
						Namespace: ns.Name,
					},
					Spec: actionsv1alpha1.HorizontalRunnerAutoscalerSpec{
						ScaleTargetRef: actionsv1alpha1.ScaleTargetRef{
							Name: name,
						},
						MinReplicas:                       intPtr(1),
						MaxReplicas:                       intPtr(5),
						ScaleDownDelaySecondsAfterScaleUp: intPtr(1),
						ScaleUpTriggers: []actionsv1alpha1.ScaleUpTrigger{
							{
								GitHubEvent: &actionsv1alpha1.GitHubEventScaleUpTriggerSpec{
									WorkflowJob: &actionsv1alpha1.WorkflowJobSpec{},
								},
								Amount:   1,
								Duration: metav1.Duration{Duration: time.Minute},
							},
						},
					},
				}

				ExpectCreate(ctx, hra, "test HorizontalRunnerAutoscaler")

				ExpectRunnerSetsCountEventuallyEquals(ctx, ns.Name, 1)
				ExpectRunnerSetsManagedReplicasCountEventuallyEquals(ctx, ns.Name, 1)
			}

			{
				env.ExpectRegisteredNumberCountEventuallyEquals(1, "count of fake list runners")
			}

			// Scale-up to 2 replicas on first workflow_job webhook event
			{
				env.SendWorkflowJobEvent("test", "valid", "queued", []string{"self-hosted"})
				ExpectRunnerSetsCountEventuallyEquals(ctx, ns.Name, 1, "runner sets after webhook")
				ExpectRunnerSetsManagedReplicasCountEventuallyEquals(ctx, ns.Name, 2, "runners after first webhook event")
				env.ExpectRegisteredNumberCountEventuallyEquals(2, "count of fake list runners")
			}
		})

		It("should be able to scale visible organization runner group with custom labels", func() {
			name := "example-runnerdeploy"

			{
				rd := &actionsv1alpha1.RunnerDeployment{
					ObjectMeta: metav1.ObjectMeta{
						Name:      name,
						Namespace: ns.Name,
					},
					Spec: actionsv1alpha1.RunnerDeploymentSpec{
						Replicas: intPtr(1),
						Selector: &metav1.LabelSelector{
							MatchLabels: map[string]string{
								"foo": "bar",
							},
						},
						Template: actionsv1alpha1.RunnerTemplate{
							ObjectMeta: metav1.ObjectMeta{
								Labels: map[string]string{
									"foo": "bar",
								},
							},
							Spec: actionsv1alpha1.RunnerSpec{
								RunnerConfig: actionsv1alpha1.RunnerConfig{
									Repository: "test/valid",
									Image:      "bar",
									Group:      "baz",
									Labels:     []string{"custom-label"},
								},
								RunnerPodSpec: actionsv1alpha1.RunnerPodSpec{
									Env: []corev1.EnvVar{
										{Name: "FOO", Value: "FOOVALUE"},
									},
								},
							},
						},
					},
				}

				ExpectCreate(ctx, rd, "test RunnerDeployment")

				hra := &actionsv1alpha1.HorizontalRunnerAutoscaler{
					ObjectMeta: metav1.ObjectMeta{
						Name:      name,
						Namespace: ns.Name,
					},
					Spec: actionsv1alpha1.HorizontalRunnerAutoscalerSpec{
						ScaleTargetRef: actionsv1alpha1.ScaleTargetRef{
							Name: name,
						},
						MinReplicas:                       intPtr(1),
						MaxReplicas:                       intPtr(5),
						ScaleDownDelaySecondsAfterScaleUp: intPtr(1),
						ScaleUpTriggers: []actionsv1alpha1.ScaleUpTrigger{
							{
								GitHubEvent: &actionsv1alpha1.GitHubEventScaleUpTriggerSpec{
									WorkflowJob: &actionsv1alpha1.WorkflowJobSpec{},
								},
								Amount:   1,
								Duration: metav1.Duration{Duration: time.Minute},
							},
						},
					},
				}

				ExpectCreate(ctx, hra, "test HorizontalRunnerAutoscaler")

				ExpectRunnerSetsCountEventuallyEquals(ctx, ns.Name, 1)
				ExpectRunnerSetsManagedReplicasCountEventuallyEquals(ctx, ns.Name, 1)
			}

			{
				env.ExpectRegisteredNumberCountEventuallyEquals(1, "count of fake list runners")
			}

			// Scale-up to 2 replicas on first workflow_job webhook event
			{
				env.SendWorkflowJobEvent("test", "valid", "queued", []string{"custom-label"})
				ExpectRunnerSetsCountEventuallyEquals(ctx, ns.Name, 1, "runner sets after webhook")
				ExpectRunnerSetsManagedReplicasCountEventuallyEquals(ctx, ns.Name, 2, "runners after first webhook event")
				env.ExpectRegisteredNumberCountEventuallyEquals(2, "count of fake list runners")
			}
		})

	})
})

func ExpectHRADesiredReplicasEquals(ctx context.Context, ns, name string, desired int, optionalDescriptions ...interface{}) {
	var rd actionsv1alpha1.HorizontalRunnerAutoscaler

	err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &rd)

	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "failed to get test HRA resource")

	replicas := rd.Status.DesiredReplicas

	ExpectWithOffset(1, *replicas).To(Equal(desired), optionalDescriptions...)
}

func (env *testEnvironment) ExpectRegisteredNumberCountEventuallyEquals(want int, optionalDescriptions ...interface{}) {
	EventuallyWithOffset(
		1,
		func() int {
			env.SyncRunnerRegistrations()

			rs, err := env.ghClient.ListRunners(context.Background(), "", "", "test/valid")
			Expect(err).NotTo(HaveOccurred(), "verifying list fake runners response")

			return len(rs)
		},
		time.Second*10, time.Millisecond*500).Should(Equal(want), optionalDescriptions...)
}

func (env *testEnvironment) SendOrgPullRequestEvent(org, repo, branch, action string) {
	resp, err := sendWebhook(env.webhookServer, "pull_request", &github.PullRequestEvent{
		PullRequest: &github.PullRequest{
			Base: &github.PullRequestBranch{
				Ref: github.String(branch),
			},
		},
		Repo: &github.Repository{
			Name: github.String(repo),
			Owner: &github.User{
				Login: github.String(org),
				Type:  github.String("Organization"),
			},
		},
		Action: github.String(action),
	})

	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "failed to send pull_request event")

	ExpectWithOffset(1, resp.StatusCode).To(Equal(200))
}

func (env *testEnvironment) SendOrgCheckRunEvent(org, repo, status, action string) {
	resp, err := sendWebhook(env.webhookServer, "check_run", &github.CheckRunEvent{
		CheckRun: &github.CheckRun{
			Status: github.String(status),
		},
		Org: &github.Organization{
			Login: github.String(org),
		},
		Repo: &github.Repository{
			Name: github.String(repo),
			Owner: &github.User{
				Login: github.String(org),
				Type:  github.String("Organization"),
			},
		},
		Action: github.String(action),
	})

	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "failed to send check_run event")

	ExpectWithOffset(1, resp.StatusCode).To(Equal(200))
}

func (env *testEnvironment) SendWorkflowJobEvent(org, repo, statusAndAction string, labels []string) {
	resp, err := sendWebhook(env.webhookServer, "workflow_job", &github.WorkflowJobEvent{
		WorkflowJob: &github.WorkflowJob{
			Status: &statusAndAction,
			Labels: labels,
		},
		Org: &github.Organization{
			Login: github.String(org),
		},
		Repo: &github.Repository{
			Name: github.String(repo),
			Owner: &github.User{
				Login: github.String(org),
				Type:  github.String("Organization"),
			},
		},
		Action: github.String(statusAndAction),
	})

	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "failed to send workflow_job event")

	ExpectWithOffset(1, resp.StatusCode).To(Equal(200))
}

func (env *testEnvironment) SendUserPullRequestEvent(owner, repo, branch, action string) {
	resp, err := sendWebhook(env.webhookServer, "pull_request", &github.PullRequestEvent{
		PullRequest: &github.PullRequest{
			Base: &github.PullRequestBranch{
				Ref: github.String(branch),
			},
		},
		Repo: &github.Repository{
			Name: github.String(repo),
			Owner: &github.User{
				Login: github.String(owner),
				Type:  github.String("User"),
			},
		},
		Action: github.String(action),
	})

	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "failed to send pull_request event")

	ExpectWithOffset(1, resp.StatusCode).To(Equal(200))
}

func (env *testEnvironment) SendUserCheckRunEvent(owner, repo, status, action string) {
	resp, err := sendWebhook(env.webhookServer, "check_run", &github.CheckRunEvent{
		CheckRun: &github.CheckRun{
			Status: github.String(status),
		},
		Repo: &github.Repository{
			Name: github.String(repo),
			Owner: &github.User{
				Login: github.String(owner),
				Type:  github.String("User"),
			},
		},
		Action: github.String(action),
	})

	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "failed to send check_run event")

	ExpectWithOffset(1, resp.StatusCode).To(Equal(200))
}

func (env *testEnvironment) SyncRunnerRegistrations() {
	var runnerList actionsv1alpha1.RunnerList

	err := k8sClient.List(context.TODO(), &runnerList, client.InNamespace(env.Namespace.Name))
	if err != nil {
		logf.Log.Error(err, "list runners")
	}

	env.fakeRunnerList.Sync(runnerList.Items)
}

func ExpectCreate(ctx context.Context, rd client.Object, s string) {
	err := k8sClient.Create(ctx, rd)

	ExpectWithOffset(1, err).NotTo(HaveOccurred(), fmt.Sprintf("failed to create %s resource", s))
}

func ExpectRunnerDeploymentEventuallyUpdates(ctx context.Context, ns string, name string, f func(rd *actionsv1alpha1.RunnerDeployment)) {
	// We wrap the update in the Eventually block to avoid the below error that occurs due to concurrent modification
	// made by the controller to update .Status.AvailableReplicas and .Status.ReadyReplicas
	//   Operation cannot be fulfilled on runnersets.actions.summerwind.dev "example-runnerset": the object has been modified; please apply your changes to the latest version and try again
	EventuallyWithOffset(
		1,
		func() error {
			var rd actionsv1alpha1.RunnerDeployment

			err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &rd)

			Expect(err).NotTo(HaveOccurred(), "failed to get test RunnerDeployment resource")

			f(&rd)

			return k8sClient.Update(ctx, &rd)
		},
		time.Second*1, time.Millisecond*500).Should(BeNil())
}

func ExpectRunnerSetsCountEventuallyEquals(ctx context.Context, ns string, count int, optionalDescription ...interface{}) {
	runnerSets := actionsv1alpha1.RunnerReplicaSetList{Items: []actionsv1alpha1.RunnerReplicaSet{}}

	EventuallyWithOffset(
		1,
		func() int {
			err := k8sClient.List(ctx, &runnerSets, client.InNamespace(ns))
			if err != nil {
				logf.Log.Error(err, "list runner sets")
			}

			return len(runnerSets.Items)
		},
		time.Second*10, time.Millisecond*500).Should(BeEquivalentTo(count), optionalDescription...)
}

func ExpectRunnerCountEventuallyEquals(ctx context.Context, ns string, count int, optionalDescription ...interface{}) {
	runners := actionsv1alpha1.RunnerList{Items: []actionsv1alpha1.Runner{}}

	EventuallyWithOffset(
		1,
		func() int {
			err := k8sClient.List(ctx, &runners, client.InNamespace(ns))
			if err != nil {
				logf.Log.Error(err, "list runner sets")
			}

			var running int

			for _, r := range runners.Items {
				if r.Status.Phase == string(corev1.PodRunning) {
					running++
				} else {
					var pod corev1.Pod
					if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: r.Name}, &pod); err != nil {
						logf.Log.Error(err, "simulating pod controller")
						continue
					}

					copy := pod.DeepCopy()
					copy.Status.Phase = corev1.PodRunning

					if err := k8sClient.Status().Patch(ctx, copy, client.MergeFrom(&pod)); err != nil {
						logf.Log.Error(err, "simulating pod controller")
						continue
					}
				}
			}

			return running
		},
		time.Second*10, time.Millisecond*500).Should(BeEquivalentTo(count), optionalDescription...)
}

func ExpectRunnerSetsManagedReplicasCountEventuallyEquals(ctx context.Context, ns string, count int, optionalDescription ...interface{}) {
	runnerSets := actionsv1alpha1.RunnerReplicaSetList{Items: []actionsv1alpha1.RunnerReplicaSet{}}

	EventuallyWithOffset(
		1,
		func() int {
			err := k8sClient.List(ctx, &runnerSets, client.InNamespace(ns))
			if err != nil {
				logf.Log.Error(err, "list runner sets")
			}

			if len(runnerSets.Items) == 0 {
				logf.Log.Info("No runnerreplicasets exist yet")
				return -1
			}

			if len(runnerSets.Items) != 1 {
				logf.Log.Info("Too many runnerreplicasets exist", "runnerSets", runnerSets)
				return -1
			}

			return *runnerSets.Items[0].Spec.Replicas
		},
		time.Second*5, time.Millisecond*500).Should(BeEquivalentTo(count), optionalDescription...)
}
