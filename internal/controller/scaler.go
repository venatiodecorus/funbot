package controller

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

const (
	// k8sNamespace is the namespace where funbot resources live.
	k8sNamespace = "funbot"

	// workerDeploymentPrefix is used to derive deployment names from network names.
	workerDeploymentPrefix = "funbot-worker-"

	// scaleWaitTimeout is how long to wait for pods to become ready after scaling.
	scaleWaitTimeout = 2 * time.Minute

	// scaleCheckInterval is how often to check pod readiness during scale-up.
	scaleCheckInterval = 5 * time.Second
)

// Scaler manages Kubernetes operations for scaling worker Deployments
// and creating new ones at runtime.
type Scaler struct {
	clientset kubernetes.Interface
	log       *slog.Logger
}

// NewScaler creates a new Scaler using in-cluster config.
// Returns nil and a warning if not running in Kubernetes (e.g., local dev).
func NewScaler(log *slog.Logger) *Scaler {
	config, err := rest.InClusterConfig()
	if err != nil {
		log.Info("not running in kubernetes, scaler disabled", "error", err)
		return nil
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		log.Error("failed to create kubernetes client", "error", err)
		return nil
	}

	log.Info("kubernetes scaler initialized")
	return &Scaler{
		clientset: clientset,
		log:       log,
	}
}

// NewScalerWithClient creates a Scaler with a provided clientset (for testing).
func NewScalerWithClient(clientset kubernetes.Interface, log *slog.Logger) *Scaler {
	return &Scaler{
		clientset: clientset,
		log:       log,
	}
}

// deploymentName returns the Kubernetes Deployment name for a network.
func deploymentName(network string) string {
	return workerDeploymentPrefix + network
}

// GetReplicas returns the current replica count for a network's worker Deployment.
func (s *Scaler) GetReplicas(ctx context.Context, network string) (int32, error) {
	name := deploymentName(network)
	deploy, err := s.clientset.AppsV1().Deployments(k8sNamespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return 0, fmt.Errorf("getting deployment %s: %w", name, err)
	}
	if deploy.Spec.Replicas == nil {
		return 1, nil // k8s default
	}
	return *deploy.Spec.Replicas, nil
}

// GetReadyReplicas returns the number of ready pods for a network's worker Deployment.
func (s *Scaler) GetReadyReplicas(ctx context.Context, network string) (int32, error) {
	name := deploymentName(network)
	deploy, err := s.clientset.AppsV1().Deployments(k8sNamespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return 0, fmt.Errorf("getting deployment %s: %w", name, err)
	}
	return deploy.Status.ReadyReplicas, nil
}

// Scale sets the replica count for a network's worker Deployment.
func (s *Scaler) Scale(ctx context.Context, network string, replicas int32) error {
	name := deploymentName(network)
	s.log.Info("scaling deployment", "deployment", name, "replicas", replicas)

	deploy, err := s.clientset.AppsV1().Deployments(k8sNamespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("getting deployment %s: %w", name, err)
	}

	deploy.Spec.Replicas = &replicas
	_, err = s.clientset.AppsV1().Deployments(k8sNamespace).Update(ctx, deploy, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("updating deployment %s replicas to %d: %w", name, replicas, err)
	}

	s.log.Info("deployment scaled", "deployment", name, "replicas", replicas)
	return nil
}

// WaitForReady blocks until all replicas are ready or timeout expires.
func (s *Scaler) WaitForReady(ctx context.Context, network string, desiredReplicas int32) error {
	name := deploymentName(network)
	s.log.Info("waiting for pods to be ready", "deployment", name, "desired", desiredReplicas)

	deadline := time.Now().Add(scaleWaitTimeout)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		deploy, err := s.clientset.AppsV1().Deployments(k8sNamespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			s.log.Warn("error checking deployment status", "error", err)
			time.Sleep(scaleCheckInterval)
			continue
		}

		if deploy.Status.ReadyReplicas >= desiredReplicas {
			s.log.Info("all pods ready", "deployment", name, "ready", deploy.Status.ReadyReplicas)
			return nil
		}

		s.log.Debug("waiting for pods",
			"deployment", name,
			"ready", deploy.Status.ReadyReplicas,
			"desired", desiredReplicas,
		)
		time.Sleep(scaleCheckInterval)
	}

	return fmt.Errorf("timeout waiting for %s to reach %d ready replicas", name, desiredReplicas)
}

// DeploymentExists checks if a worker Deployment exists for the given network.
func (s *Scaler) DeploymentExists(ctx context.Context, network string) (bool, error) {
	name := deploymentName(network)
	_, err := s.clientset.AppsV1().Deployments(k8sNamespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		// Check if it's a "not found" error vs a real error
		if statusErr, ok := err.(*k8serrors.StatusError); ok && statusErr.Status().Reason == metav1.StatusReasonNotFound {
			return false, nil
		}
		return false, fmt.Errorf("checking deployment %s: %w", name, err)
	}
	return true, nil
}

// CreateWorkerDeployment creates a new worker Deployment for a runtime-added network.
// This is used by the !connect command to add networks dynamically.
func (s *Scaler) CreateWorkerDeployment(ctx context.Context, network, image string, replicas int32) error {
	name := deploymentName(network)
	s.log.Info("creating worker deployment", "deployment", name, "network", network)

	labels := map[string]string{
		"app":       "funbot",
		"component": "worker",
		"network":   network,
		"runtime":   "true", // Mark as runtime-created
	}

	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: k8sNamespace,
			Labels:    labels,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: labels,
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: labels,
				},
				Spec: corev1.PodSpec{
					TerminationGracePeriodSeconds: int64Ptr(15),
					Containers: []corev1.Container{
						{
							Name:            "worker",
							Image:           image,
							ImagePullPolicy: corev1.PullIfNotPresent,
							Args:            []string{"--role=worker", "--network=" + network},
							Env: []corev1.EnvVar{
								{
									Name:  "FUNBOT_ROLE",
									Value: "worker",
								},
								{
									Name:  "FUNBOT_NETWORK",
									Value: network,
								},
								{
									Name: "POD_NAME",
									ValueFrom: &corev1.EnvVarSource{
										FieldRef: &corev1.ObjectFieldSelector{
											FieldPath: "metadata.name",
										},
									},
								},
							},
							Ports: []corev1.ContainerPort{
								{
									Name:          "health",
									ContainerPort: 8080,
									Protocol:      corev1.ProtocolTCP,
								},
							},
							LivenessProbe: &corev1.Probe{
								ProbeHandler: corev1.ProbeHandler{
									HTTPGet: &corev1.HTTPGetAction{
										Path: "/healthz",
										Port: intOrString(8080),
									},
								},
								InitialDelaySeconds: 15,
								PeriodSeconds:       15,
							},
							ReadinessProbe: &corev1.Probe{
								ProbeHandler: corev1.ProbeHandler{
									HTTPGet: &corev1.HTTPGetAction{
										Path: "/readyz",
										Port: intOrString(8080),
									},
								},
								InitialDelaySeconds: 10,
								PeriodSeconds:       10,
							},
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:      "config",
									MountPath: "/etc/funbot",
									ReadOnly:  true,
								},
								{
									Name:      "proxies",
									MountPath: "/etc/funbot/proxies",
									ReadOnly:  true,
								},
								{
									Name:      "art-data",
									MountPath: "/data/asciiart",
								},
							},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: "config",
							VolumeSource: corev1.VolumeSource{
								ConfigMap: &corev1.ConfigMapVolumeSource{
									LocalObjectReference: corev1.LocalObjectReference{
										Name: "funbot-config",
									},
								},
							},
						},
						{
							Name: "proxies",
							VolumeSource: corev1.VolumeSource{
								Secret: &corev1.SecretVolumeSource{
									SecretName: "funbot-proxies",
								},
							},
						},
						{
							Name: "art-data",
							VolumeSource: corev1.VolumeSource{
								PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
									ClaimName: "funbot-art",
								},
							},
						},
					},
				},
			},
		},
	}

	_, err := s.clientset.AppsV1().Deployments(k8sNamespace).Create(ctx, deployment, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("creating deployment %s: %w", name, err)
	}

	s.log.Info("worker deployment created", "deployment", name)
	return nil
}

// DeleteWorkerDeployment removes a worker Deployment. Used by !disconnect.
func (s *Scaler) DeleteWorkerDeployment(ctx context.Context, network string) error {
	name := deploymentName(network)
	s.log.Info("deleting worker deployment", "deployment", name)

	err := s.clientset.AppsV1().Deployments(k8sNamespace).Delete(ctx, name, metav1.DeleteOptions{})
	if err != nil {
		return fmt.Errorf("deleting deployment %s: %w", name, err)
	}

	s.log.Info("worker deployment deleted", "deployment", name)
	return nil
}

// ListWorkerDeployments returns all funbot worker Deployments in the namespace.
func (s *Scaler) ListWorkerDeployments(ctx context.Context) ([]appsv1.Deployment, error) {
	list, err := s.clientset.AppsV1().Deployments(k8sNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: "app=funbot,component=worker",
	})
	if err != nil {
		return nil, fmt.Errorf("listing worker deployments: %w", err)
	}
	return list.Items, nil
}

// CalculateScaling determines how many additional pods are needed
// for a given number of required clients.
func (s *Scaler) CalculateScaling(
	currentPods int32,
	clientsPerPod int,
	availableClients int,
	neededClients int,
	availableProxies int,
) ScaleRecommendation {
	if neededClients <= availableClients {
		return ScaleRecommendation{
			Needed:   false,
			Message:  fmt.Sprintf("Sufficient capacity: %d available, %d needed", availableClients, neededClients),
			NewPods:  0,
			NewTotal: currentPods,
		}
	}

	deficit := neededClients - availableClients

	// Each new pod provides clientsPerPod direct connections
	// Plus we can use available proxies for additional capacity
	effectivePerPod := clientsPerPod
	podsNeeded := int32((deficit + effectivePerPod - 1) / effectivePerPod)

	return ScaleRecommendation{
		Needed:   true,
		Message:  fmt.Sprintf("Need %d more clients. Scale to %d pods? Use !confirm to proceed.", deficit, currentPods+podsNeeded),
		NewPods:  podsNeeded,
		NewTotal: currentPods + podsNeeded,
		Deficit:  deficit,
	}
}

// ScaleRecommendation holds the result of a scaling calculation.
type ScaleRecommendation struct {
	Needed   bool
	Message  string
	NewPods  int32
	NewTotal int32
	Deficit  int
}

func int64Ptr(i int64) *int64 {
	return &i
}

func intOrString(port int) intstr.IntOrString {
	return intstr.FromInt32(int32(port))
}
