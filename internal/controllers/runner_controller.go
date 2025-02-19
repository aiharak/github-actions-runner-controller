package controllers

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"time"

	garV1 "github-actions-runner-controller/api/v1"

	dockerref "github.com/docker/distribution/reference"
	"github.com/go-logr/logr"
	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/xerrors"
	appsV1 "k8s.io/api/apps/v1"
	coreV1 "k8s.io/api/core/v1"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metaV1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

const (
	ownerKey               = ".metadata.controller"
	optimisticLockErrorMsg = "the object has been modified; please apply your changes to the latest version and try again"
	expiresAtAnnotation    = "github-actions-runner.kaidotio.github.io/expiresAt"
)

type RunnerReconciler struct {
	client.Client
	Log                     logr.Logger
	Scheme                  *runtime.Scheme
	Recorder                record.EventRecorder
	PushRegistryHost        string
	PullRegistryHost        string
	EnableRunnerMetrics     bool
	ExporterImage           string
	GitHubAppClientId       string
	GitHubAppInstallationId string
	GitHubAppPrivateKey     string
	KanikoImage             string
	BinaryVersion           string
	RunnerVersion           string
	Disableupdate           bool
}

func (r *RunnerReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var requeueAfter time.Duration

	runner := &garV1.Runner{}
	logger := r.Log.WithValues("runner", req.NamespacedName)
	if err := r.Get(ctx, req.NamespacedName, runner); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	if err := r.cleanupOwnedResources(ctx, runner); err != nil {
		return ctrl.Result{}, err
	}

	if runner.Spec.TokenSecretKeyRef == nil && r.GitHubAppClientId != "" && r.GitHubAppInstallationId != "" && r.GitHubAppPrivateKey != "" {
		var tokenSecret v1.Secret
		if err := r.Client.Get(
			ctx,
			client.ObjectKey{
				Name:      req.Name,
				Namespace: req.Namespace,
			},
			&tokenSecret,
		); apierrors.IsNotFound(err) {
			tokenSecret, err := r.createTokenSecret(runner)
			if err != nil {
				return ctrl.Result{}, err
			}
			if err := controllerutil.SetControllerReference(runner, tokenSecret, r.Scheme); err != nil {
				return ctrl.Result{}, err
			}
			if err := r.Create(ctx, tokenSecret); err != nil {
				return ctrl.Result{}, err
			}
			r.Recorder.Eventf(runner, coreV1.EventTypeNormal, "SuccessfulCreated", "Created token secret: %q", tokenSecret.Name)
			logger.V(1).Info("create", "secret", tokenSecret)

			expire, err := time.Parse(time.RFC3339, tokenSecret.Annotations[expiresAtAnnotation])
			if err != nil {
				return ctrl.Result{}, err
			}
			requeueAfter = expire.Sub(time.Now()) - time.Minute
		} else if err != nil {
			return ctrl.Result{}, err
		} else {
			expectedTokenSecret, err := r.createTokenSecret(runner)
			if err != nil {
				return ctrl.Result{}, err
			}
			if !reflect.DeepEqual(tokenSecret.Data, expectedTokenSecret.Data) ||
				!reflect.DeepEqual(tokenSecret.StringData, expectedTokenSecret.StringData) {
				tokenSecret.Annotations = expectedTokenSecret.Annotations
				tokenSecret.Data = expectedTokenSecret.Data
				tokenSecret.StringData = expectedTokenSecret.StringData

				if err := r.Update(ctx, &tokenSecret); err != nil {
					return ctrl.Result{}, err
				}
				r.Recorder.Eventf(runner, coreV1.EventTypeNormal, "SuccessfulUpdated", "Updated token secret: %q", tokenSecret.Name)
				logger.V(1).Info("update", "secret", tokenSecret)

				expire, err := time.Parse(time.RFC3339, tokenSecret.Annotations[expiresAtAnnotation])
				if err != nil {
					return ctrl.Result{}, err
				}
				requeueAfter = expire.Sub(time.Now()) - time.Minute
			}
		}

		runner.Spec.TokenSecretKeyRef = &coreV1.SecretKeySelector{
			LocalObjectReference: coreV1.LocalObjectReference{
				Name: req.Name,
			},
			Key: "GITHUB_TOKEN",
		}
	}

	var workspaceConfigMap v1.ConfigMap
	if err := r.Client.Get(
		ctx,
		client.ObjectKey{
			Name:      req.Name + "-workspace",
			Namespace: req.Namespace,
		},
		&workspaceConfigMap,
	); apierrors.IsNotFound(err) {
		workspaceConfigMap = *r.buildWorkspaceConfigMap(runner)
		if err := controllerutil.SetControllerReference(runner, &workspaceConfigMap, r.Scheme); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.Create(ctx, &workspaceConfigMap); err != nil {
			return ctrl.Result{}, err
		}
		r.Recorder.Eventf(runner, coreV1.EventTypeNormal, "SuccessfulCreated", "Created workspace config map: %q", workspaceConfigMap.Name)
		logger.V(1).Info("create", "config map", workspaceConfigMap)
	} else if err != nil {
		return ctrl.Result{}, err
	} else {
		expectedWorkspaceConfigMap := r.buildWorkspaceConfigMap(runner)
		if !reflect.DeepEqual(workspaceConfigMap.Data, expectedWorkspaceConfigMap.Data) ||
			!reflect.DeepEqual(workspaceConfigMap.BinaryData, expectedWorkspaceConfigMap.BinaryData) {
			workspaceConfigMap.Data = expectedWorkspaceConfigMap.Data
			workspaceConfigMap.BinaryData = expectedWorkspaceConfigMap.BinaryData

			if err := r.Update(ctx, &workspaceConfigMap); err != nil {
				return ctrl.Result{}, err
			}
			r.Recorder.Eventf(runner, coreV1.EventTypeNormal, "SuccessfulUpdated", "Updated config map: %q", workspaceConfigMap.Name)
			logger.V(1).Info("update", "config map", workspaceConfigMap)
		}
	}

	var deployment appsV1.Deployment
	if err := r.Client.Get(
		ctx,
		client.ObjectKey{
			Name:      req.Name + "-runner",
			Namespace: req.Namespace,
		},
		&deployment,
	); apierrors.IsNotFound(err) {
		deployment = *r.buildDeployment(runner)
		if err := controllerutil.SetControllerReference(runner, &deployment, r.Scheme); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.Create(ctx, &deployment); err != nil {
			return ctrl.Result{}, err
		}
		r.Recorder.Eventf(runner, coreV1.EventTypeNormal, "SuccessfulCreated", "Created deployment: %q", deployment.Name)
		logger.V(1).Info("create", "deployment", deployment)
	} else if err != nil {
		return ctrl.Result{}, err
	} else {
		expectedDeployment := r.buildDeployment(runner)
		if !reflect.DeepEqual(deployment.Spec.Template, expectedDeployment.Spec.Template) {
			deployment.Spec.Template = expectedDeployment.Spec.Template

			if err := r.Update(ctx, &deployment); err != nil {
				if strings.Contains(err.Error(), optimisticLockErrorMsg) {
					return ctrl.Result{RequeueAfter: time.Second}, nil
				}
				return ctrl.Result{}, err
			}
			r.Recorder.Eventf(runner, coreV1.EventTypeNormal, "SuccessfulUpdated", "Updated deployment: %q", deployment.Name)
			logger.V(1).Info("update", "deployment", deployment)
		}
	}

	return ctrl.Result{RequeueAfter: requeueAfter}, nil
}

func (r *RunnerReconciler) buildRepositoryName(runner *garV1.Runner) string {
	named, err := dockerref.ParseNormalizedNamed(runner.Spec.Image)
	if err != nil {
		return fmt.Sprintf("%x", sha256.Sum256([]byte(runner.Spec.Image+r.BinaryVersion+r.RunnerVersion)))[:7]
	}
	trimmed := dockerref.TrimNamed(named).String()
	return fmt.Sprintf("%x", sha256.Sum256([]byte(trimmed+r.BinaryVersion+r.RunnerVersion)))[:7]
}

func (r *RunnerReconciler) buildBuilderContainer(runner *garV1.Runner) v1.Container {
	if runner.Spec.BuilderContainerSpec.Resources.Limits == nil {
		runner.Spec.BuilderContainerSpec.Resources.Limits = make(v1.ResourceList)
	}
	if runner.Spec.BuilderContainerSpec.Resources.Limits.Memory().IsZero() {
		runner.Spec.BuilderContainerSpec.Resources.Limits[v1.ResourceMemory] = resource.MustParse("4Gi")
	}
	return v1.Container{
		Name:            "kaniko",
		Image:           r.KanikoImage,
		ImagePullPolicy: v1.PullIfNotPresent,
		Args: []string{
			"--dockerfile=Dockerfile",
			"--context=dir:///workspace",
			"--cache=true",
			"--compressed-caching=false",
			fmt.Sprintf("--destination=%s/%s", r.PushRegistryHost, r.buildRepositoryName(runner)),
		},
		EnvFrom: runner.Spec.BuilderContainerSpec.EnvFrom,
		Env:     runner.Spec.BuilderContainerSpec.Env,
		VolumeMounts: append([]v1.VolumeMount{
			{
				Name:      "workspace",
				MountPath: "/workspace/Dockerfile",
				SubPath:   "Dockerfile",
				ReadOnly:  true,
			},
		}, runner.Spec.BuilderContainerSpec.VolumeMounts...),
		Resources:                runner.Spec.BuilderContainerSpec.Resources,
		TerminationMessagePath:   coreV1.TerminationMessagePathDefault,
		TerminationMessagePolicy: coreV1.TerminationMessageReadFile,
	}
}

func (r *RunnerReconciler) buildRunnerContainer(runner *garV1.Runner) v1.Container {
	args := []string{
		"--without-install",
		"--repository=$(REPOSITORY)",
		"--hostname=$(HOSTNAME)",
	}
	env := runner.Spec.RunnerContainerSpec.Env
	envFrom := runner.Spec.RunnerContainerSpec.EnvFrom

	env = append(env, []coreV1.EnvVar{
		{
			Name:  "REPOSITORY",
			Value: runner.Spec.Repository,
		},
		{
			Name: "HOSTNAME",
			ValueFrom: &coreV1.EnvVarSource{
				FieldRef: &coreV1.ObjectFieldSelector{
					APIVersion: "v1",
					FieldPath:  "metadata.name",
				},
			},
		},
	}...)

	if runner.Spec.TokenSecretKeyRef != nil {
		args = append(args, "--token=$(TOKEN)")
		env = append(env, coreV1.EnvVar{
			Name: "TOKEN",
			ValueFrom: &coreV1.EnvVarSource{
				SecretKeyRef: runner.Spec.TokenSecretKeyRef,
			},
		})
	}

	if runner.Spec.AppSecretRef != nil {
		args = append(args, []string{
			"--github-app-id=$(github_app_id)",
			"--github-app-installation-id=$(github_app_installation_id)",
			"--github-app-private-key=$(github_app_private_key)",
		}...)
		envFrom = append(envFrom, coreV1.EnvFromSource{
			SecretRef: runner.Spec.AppSecretRef,
		})
	}

	c := v1.Container{
		Name: "runner",
		SecurityContext: &v1.SecurityContext{
			Privileged:             func(b bool) *bool { return &b }(false),
			ReadOnlyRootFilesystem: func(b bool) *bool { return &b }(false),
			RunAsUser:              func(i int64) *int64 { return &i }(60000),
			RunAsNonRoot:           func(b bool) *bool { return &b }(true),
			SeccompProfile: &coreV1.SeccompProfile{
				Type: coreV1.SeccompProfileTypeRuntimeDefault,
			},
		},
		Image:                    fmt.Sprintf("%s/%s", r.PullRegistryHost, r.buildRepositoryName(runner)),
		ImagePullPolicy:          v1.PullAlways,
		Args:                     args,
		EnvFrom:                  envFrom,
		Env:                      env,
		Resources:                runner.Spec.RunnerContainerSpec.Resources,
		VolumeMounts:             runner.Spec.RunnerContainerSpec.VolumeMounts,
		TerminationMessagePath:   coreV1.TerminationMessagePathDefault,
		TerminationMessagePolicy: coreV1.TerminationMessageReadFile,
	}
	if r.Disableupdate {
		c.Args = append(c.Args, "--disableupdate")
	}
	return c
}

func (r *RunnerReconciler) buildExporterContainer(runner *garV1.Runner) v1.Container {
	return v1.Container{
		Name:            "exporter",
		Image:           r.ExporterImage,
		ImagePullPolicy: v1.PullAlways,
		Args: []string{
			"server",
			"--api-address=0.0.0.0:8000",
			"--monitor-address=0.0.0.0:9090",
			"--repository=$(REPOSITORY)",
			"--token=$(TOKEN)",
		},
		Env: []coreV1.EnvVar{
			{
				Name:  "REPOSITORY",
				Value: runner.Spec.Repository,
			},
			{
				Name: "TOKEN",
				ValueFrom: &coreV1.EnvVarSource{
					SecretKeyRef: runner.Spec.TokenSecretKeyRef,
				},
			},
		},
		Ports: []coreV1.ContainerPort{
			{
				ContainerPort: 9090,
				Protocol:      "TCP",
			},
		},
		TerminationMessagePath:   coreV1.TerminationMessagePathDefault,
		TerminationMessagePolicy: coreV1.TerminationMessageReadFile,
	}
}

func (r *RunnerReconciler) buildDeployment(runner *garV1.Runner) *appsV1.Deployment {
	containers := []v1.Container{
		r.buildRunnerContainer(runner),
	}

	if r.EnableRunnerMetrics {
		containers = append(containers, r.buildExporterContainer(runner))
	}

	appLabel := runner.Name + "-runner"
	labels := map[string]string{
		"app": appLabel,
	}
	for k, v := range runner.Spec.Template.ObjectMeta.Labels {
		labels[k] = v
	}
	runner.Spec.Template.ObjectMeta.Labels = labels
	annotations := map[string]string{
		"image": runner.Spec.Image,
	}
	for k, v := range runner.Spec.Template.ObjectMeta.Annotations {
		annotations[k] = v
	}
	runner.Spec.Template.ObjectMeta.Annotations = annotations
	return &appsV1.Deployment{
		ObjectMeta: metaV1.ObjectMeta{
			Name:      runner.Name + "-runner",
			Namespace: runner.Namespace,
		},
		Spec: appsV1.DeploymentSpec{
			Selector: &metaV1.LabelSelector{
				MatchLabels: map[string]string{
					"app": appLabel,
				},
			},
			Replicas: func(i int32) *int32 {
				return &i
			}(1),
			Strategy: appsV1.DeploymentStrategy{
				Type: appsV1.RollingUpdateDeploymentStrategyType,
				RollingUpdate: &appsV1.RollingUpdateDeployment{
					MaxSurge: &intstr.IntOrString{
						Type:   intstr.String,
						StrVal: "25%",
					},
					MaxUnavailable: &intstr.IntOrString{
						Type:   intstr.Int,
						IntVal: 1,
					},
				},
			},
			Template: v1.PodTemplateSpec{
				ObjectMeta: runner.Spec.Template.ObjectMeta,
				Spec: v1.PodSpec{
					Affinity: &v1.Affinity{
						PodAntiAffinity: &v1.PodAntiAffinity{
							PreferredDuringSchedulingIgnoredDuringExecution: []v1.WeightedPodAffinityTerm{
								{
									Weight: 100,
									PodAffinityTerm: v1.PodAffinityTerm{
										LabelSelector: &metaV1.LabelSelector{
											MatchLabels: map[string]string{
												"app": appLabel,
											},
										},
										TopologyKey: "kubernetes.io/hostname",
									},
								},
							},
						},
					},
					InitContainers: []v1.Container{
						r.buildBuilderContainer(runner),
					},
					Containers: containers,
					Volumes: append([]v1.Volume{
						{
							Name: "workspace",
							VolumeSource: v1.VolumeSource{
								ConfigMap: &v1.ConfigMapVolumeSource{
									LocalObjectReference: v1.LocalObjectReference{
										Name: runner.Name + "-workspace",
									},
									DefaultMode: func(i int32) *int32 {
										return &i
									}(420),
								},
							},
						},
					}, runner.Spec.Template.Spec.Volumes...),
					RestartPolicy: coreV1.RestartPolicyAlways,
					TerminationGracePeriodSeconds: func(i int64) *int64 {
						return &i
					}(30),
					DNSPolicy: coreV1.DNSClusterFirst,
					SecurityContext: &coreV1.PodSecurityContext{
						SeccompProfile: &coreV1.SeccompProfile{
							Type: coreV1.SeccompProfileTypeRuntimeDefault,
						},
					},
					SchedulerName: coreV1.DefaultSchedulerName,
				},
			},
		},
	}
}

func (r *RunnerReconciler) buildWorkspaceConfigMap(runner *garV1.Runner) *v1.ConfigMap {
	return &v1.ConfigMap{
		ObjectMeta: metaV1.ObjectMeta{
			Name:      runner.Name + "-workspace",
			Namespace: runner.Namespace,
		},
		Data: map[string]string{
			"Dockerfile": fmt.Sprintf(`
FROM %s
USER root
ENV DEBIAN_FRONTEND=noninteractive
RUN (command -v apt && apt update && apt install -y ca-certificates iputils-ping tar sudo git) || \
      (command -v apt-get && apt-get update && apt-get install -y --no-install-recommends ca-certificates iputils-ping tar sudo git) || \
      (command -v dnf && dnf install -y ca-certificates iputils tar sudo git) || \
      (command -v yum && yum install -y ca-certificates iputils tar sudo git) || \
      (command -v zypper && zypper install -n ca-certificates iputils tar sudo git-core) || \
      (echo "Unknown OS version" && exit 1)

ADD https://github.com/kaidotdev/github-actions-runner-controller/releases/download/v%s/runner_%s_linux_amd64 /usr/local/bin/runner
RUN chmod +x /usr/local/bin/runner

RUN echo 'runner::60000:60000::/home/runner:/bin/sh' >> /etc/passwd
RUN echo 'runner::60000:' >> /etc/group
RUN mkdir -p /home/runner && chown -R runner:runner /home/runner

RUN echo "runner:!:0:0:99999:7:::" >> /etc/shadow
RUN echo "runner ALL=(ALL) NOPASSWD: ALL" | sudo EDITOR='tee -a' visudo

WORKDIR /home/runner

RUN /usr/local/bin/runner --only-install --runner-version %s

USER 60000

ENTRYPOINT ["/usr/local/bin/runner"]
`, runner.Spec.Image, r.BinaryVersion, r.BinaryVersion, r.RunnerVersion),
		},
	}
}

func (r *RunnerReconciler) createTokenSecret(runner *garV1.Runner) (*v1.Secret, error) {
	body := struct {
		Repositories  []string          `json:"repositories"`
		RepositoryIds []int             `json:"repository_ids"`
		Permissions   map[string]string `json:"permissions"`
	}{}

	accessToken := struct {
		Token     string `json:"token"`
		ExpiresAt string `json:"expires_at"`
	}{}

	err, jwtToken := signJwt(r.GitHubAppPrivateKey, r.GitHubAppClientId)
	if err != nil {
		return nil, xerrors.Errorf("failed to sign jwt: %w", err)
	}

	body.Repositories = []string{strings.SplitN(runner.Spec.Repository, "/", 2)[1]}
	body.Permissions = map[string]string{
		"actions":        "read",
		"administration": "write",
		"metadata":       "read",
	}
	b, err := json.Marshal(body)
	if err != nil {
		return nil, xerrors.Errorf("failed to marshal body: %w", err)
	}

	accessTokenRequest, err := http.NewRequest("POST", fmt.Sprintf("https://api.github.com/app/installations/%s/access_tokens", r.GitHubAppInstallationId), bytes.NewReader(b))
	if err != nil {
		return nil, xerrors.Errorf("failed to create request: %w", err)
	}

	accessTokenRequest.Header.Set("Accept", "application/vnd.github+json")
	accessTokenRequest.Header.Set("Authorization", fmt.Sprintf("Bearer %s", *jwtToken))
	accessTokenRequest.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	accessTokenResponse, err := http.DefaultClient.Do(accessTokenRequest)
	if err != nil {
		return nil, xerrors.Errorf("failed to do request: %w", err)
	}
	defer func() {
		_ = accessTokenResponse.Body.Close()
	}()

	if accessTokenResponse.StatusCode != http.StatusCreated {
		return nil, xerrors.Errorf("failed to get access token: %d", accessTokenResponse.StatusCode)
	}

	if err := json.NewDecoder(accessTokenResponse.Body).Decode(&accessToken); err != nil {
		return nil, xerrors.Errorf("failed to decode access token: %w", err)
	}

	return &v1.Secret{
		ObjectMeta: metaV1.ObjectMeta{
			Name:      runner.Name,
			Namespace: runner.Namespace,
			Annotations: map[string]string{
				expiresAtAnnotation: accessToken.ExpiresAt,
			},
		},
		StringData: map[string]string{
			"GITHUB_TOKEN": accessToken.Token,
		},
	}, nil
}

func signJwt(privateKey string, clientId string) (error, *string) {
	block, _ := pem.Decode([]byte(privateKey))
	if block == nil {
		return xerrors.New("failed to decode private key"), nil
	}

	rsaPrivateKey, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		return xerrors.Errorf("failed to parse private key: %w", err), nil
	}

	now := time.Now()
	claims := jwt.MapClaims{
		"iat": now.Unix(),
		"exp": now.Add(time.Minute * 10).Unix(),
		"iss": clientId,
	}

	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	jwtToken, err := token.SignedString(rsaPrivateKey)
	if err != nil {
		return xerrors.Errorf("failed to sign token: %w", err), nil
	}
	return nil, &jwtToken
}

func (r *RunnerReconciler) cleanupOwnedResources(ctx context.Context, runner *garV1.Runner) error {
	var configMaps v1.ConfigMapList
	if err := r.List(
		ctx,
		&configMaps,
		client.InNamespace(runner.Namespace),
		client.MatchingFields{ownerKey: runner.Name},
	); err != nil {
		return err
	}

	for _, configMap := range configMaps.Items {
		configMap := configMap

		if configMap.Name == runner.Name+"-workspace" {
			continue
		}

		if err := r.Client.Delete(ctx, &configMap); err != nil {
			return err
		}
		r.Recorder.Eventf(runner, coreV1.EventTypeNormal, "SuccessfulDeleted", "Deleted config map: %q", configMap.Name)
	}

	var deployments appsV1.DeploymentList
	if err := r.List(
		ctx,
		&deployments,
		client.InNamespace(runner.Namespace),
		client.MatchingFields{ownerKey: runner.Name},
	); err != nil {
		return err
	}

	for _, deployment := range deployments.Items {
		deployment := deployment

		if deployment.Name == runner.Name+"-runner" {
			continue
		}

		if err := r.Client.Delete(ctx, &deployment); err != nil {
			return err
		}
		r.Recorder.Eventf(runner, coreV1.EventTypeNormal, "SuccessfulDeleted", "Deleted deployment: %q", deployment.Name)
	}

	return nil
}

func (r *RunnerReconciler) SetupWithManager(mgr ctrl.Manager) error {
	ctx := context.Background()
	if err := mgr.GetFieldIndexer().IndexField(ctx, &v1.ConfigMap{}, ownerKey, func(rawObj client.Object) []string {
		configMap := rawObj.(*v1.ConfigMap)
		owner := metaV1.GetControllerOf(configMap)
		if owner == nil {
			return nil
		}
		if owner.Kind != "Runner" {
			return nil
		}

		return []string{owner.Name}
	}); err != nil {
		return err
	}

	if err := mgr.GetFieldIndexer().IndexField(ctx, &appsV1.Deployment{}, ownerKey, func(rawObj client.Object) []string {
		deployment := rawObj.(*appsV1.Deployment)
		owner := metaV1.GetControllerOf(deployment)
		if owner == nil {
			return nil
		}
		if owner.Kind != "Runner" {
			return nil
		}

		return []string{owner.Name}
	}); err != nil {
		return err
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&garV1.Runner{}).
		Owns(&v1.ConfigMap{}).
		Owns(&appsV1.Deployment{}).
		WithEventFilter(predicate.GenerationChangedPredicate{}).
		WithOptions(controller.Options{MaxConcurrentReconciles: 1}).
		Complete(r)
}
