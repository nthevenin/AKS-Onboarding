package controller

import (
	"context"
	"fmt"

	workloadsv1 "github.com/nthevenin/custom-workload-operator/api/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// PartitionedWorkloadReconciler reconciles a PartitionedWorkload object
type PartitionedWorkloadReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=workloads.example.com,resources=partitionedworkloads,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=workloads.example.com,resources=partitionedworkloads/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=workloads.example.com,resources=partitionedworkloads/finalizers,verbs=update

// Reconcile is part of the main Kubernetes reconciliation loop
func (r *PartitionedWorkloadReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	log.Info("Starting reconciliation for PartitionedWorkload", "name", req.NamespacedName)

	// Fetch the PartitionedWorkload resource
	var workload workloadsv1.PartitionedWorkload
	if err := r.Get(ctx, req.NamespacedName, &workload); err != nil {
		if client.IgnoreNotFound(err) != nil {
			log.Error(err, "Failed to fetch PartitionedWorkload")
			return ctrl.Result{}, err
		}
		log.Info("PartitionedWorkload resource not found. Ignoring since object must be deleted.")
		return ctrl.Result{}, nil
	}
	log.Info("Successfully fetched PartitionedWorkload resource", "name", workload.Name)

	// List all Pods managed by this PartitionedWorkload
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(req.Namespace), client.MatchingLabels{
		"partitionedworkload": workload.Name,
	}); err != nil {
		log.Error(err, "Failed to list Pods")
		return ctrl.Result{}, err
	}
	log.Info("Successfully listed Pods managed by PartitionedWorkload", "podCount", len(pods.Items))

	// Ensure the desired number of Pods and partitioning
	log.Info("Ensuring desired number of Pods and partitioning")
	if err := r.ensureReplicasAndPartition(ctx, &workload, &pods); err != nil {
		log.Error(err, "Failed to ensure replicas and partitioning")
		return ctrl.Result{}, err
	}
	log.Info("Successfully ensured desired number of Pods and partitioning")

	// Update the status
	log.Info("Updating PartitionedWorkload status")
	if err := r.updateStatus(ctx, &workload, &pods); err != nil {
		log.Error(err, "Failed to update status")
		return ctrl.Result{}, err
	}
	log.Info("Successfully updated PartitionedWorkload status")

	log.Info("Reconciliation complete for PartitionedWorkload", "name", req.NamespacedName)
	return ctrl.Result{}, nil
}

func (r *PartitionedWorkloadReconciler) ensureReplicasAndPartition(ctx context.Context, workload *workloadsv1.PartitionedWorkload, pods *corev1.PodList) error {
	log := logf.FromContext(ctx)
	log.Info("Ensuring replicas and partitioning", "desiredReplicas", workload.Spec.Replicas, "partitionCount", workload.Spec.PartitionCount)

	// Separate pods into reference version and new version
	var referenceVersionPods, newVersionPods []corev1.Pod
	for _, pod := range pods.Items {
		if pod.Labels["version"] == workload.Status.ReferenceVersion {
			referenceVersionPods = append(referenceVersionPods, pod)
		} else {
			newVersionPods = append(newVersionPods, pod)
		}
	}

	// Ensure partitioning logic
	desiredReferenceVersionCount := workload.Spec.PartitionCount
	desiredNewVersionCount := workload.Spec.Replicas - desiredReferenceVersionCount

	// Scale down reference version pods if needed
	for len(referenceVersionPods) > int(desiredReferenceVersionCount) {
		podToDelete := referenceVersionPods[len(referenceVersionPods)-1]
		if err := r.Delete(ctx, &podToDelete); err != nil {
			log.Error(err, "Failed to delete reference version Pod", "podName", podToDelete.Name)
			return fmt.Errorf("failed to delete reference version Pod: %w", err)
		}
		log.Info("Deleted reference version Pod", "podName", podToDelete.Name)
		referenceVersionPods = referenceVersionPods[:len(referenceVersionPods)-1]
	}

	// Scale down new version pods if needed
	for len(newVersionPods) > int(desiredNewVersionCount) {
		podToDelete := newVersionPods[len(newVersionPods)-1]
		if err := r.Delete(ctx, &podToDelete); err != nil {
			log.Error(err, "Failed to delete new version Pod", "podName", podToDelete.Name)
			return fmt.Errorf("failed to delete new version Pod: %w", err)
		}
		log.Info("Deleted new version Pod", "podName", podToDelete.Name)
		newVersionPods = newVersionPods[:len(newVersionPods)-1]
	}

	// Create new version pods if needed
	for len(newVersionPods) < int(desiredNewVersionCount) {
		newPod := corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: workload.Name + "-",
				Namespace:    workload.Namespace,
				Labels: map[string]string{
					"partitionedworkload": workload.Name,
					"version":             workload.Status.Version,
				},
			},
			Spec: corev1.PodSpec{
				Containers: workload.Spec.PodTemplate.Containers,
			},
		}

		// Set OwnerReference
		if err := ctrl.SetControllerReference(workload, &newPod, r.Scheme); err != nil {
			log.Error(err, "Failed to set OwnerReference for Pod")
			return fmt.Errorf("failed to set OwnerReference: %w", err)
		}

		if err := r.Create(ctx, &newPod); err != nil {
			log.Error(err, "Failed to create new version Pod")
			return fmt.Errorf("failed to create new version Pod: %w", err)
		}
		log.Info("Created new version Pod", "podName", newPod.Name)
		newVersionPods = append(newVersionPods, newPod)
	}

	return nil
}

// updateStatus updates the status of the PartitionedWorkload resource
func (r *PartitionedWorkloadReconciler) updateStatus(ctx context.Context, workload *workloadsv1.PartitionedWorkload, pods *corev1.PodList) error {
	log := logf.FromContext(ctx)
	log.Info("Updating status for PartitionedWorkload", "name", workload.Name)

	available := int32(0)
	for _, pod := range pods.Items {
		if pod.Status.Phase == corev1.PodRunning {
			available++
		}
	}

	workload.Status.AvailableReplicas = available
	if err := r.Status().Update(ctx, workload); err != nil {
		log.Error(err, "Failed to update status")
		return fmt.Errorf("failed to update status: %w", err)
	}

	log.Info("Updated status", "availableReplicas", available)
	return nil
}

// SetupWithManager sets up the controller with the Manager
func (r *PartitionedWorkloadReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&workloadsv1.PartitionedWorkload{}).
		Owns(&corev1.Pod{}). // Ensure the controller watches Pods
		Complete(r)
}
