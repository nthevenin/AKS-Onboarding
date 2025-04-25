package controller

import (
	"context"
	"fmt"

	workloadsv1 "github.com/nthevenin/custom-workload-operator/api/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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

	// Validate PartitionCount (reset if invalid)
	if workload.Spec.PartitionCount > workload.Spec.Replicas {
		log.Info("PartitionCount exceeds Replicas, resetting PartitionCount to 0")
		workload.Spec.PartitionCount = 0
	}

	// List all Pods managed by this PartitionedWorkload
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(req.Namespace), client.MatchingLabels{
		"partitionedworkload": workload.Name,
	}); err != nil {
		log.Error(err, "Failed to list Pods")
		return ctrl.Result{}, err
	}
	log.Info("Successfully listed Pods managed by PartitionedWorkload", "podCount", len(pods.Items))

	// Ensure the desired number of Pods
	log.Info("Ensuring desired number of Pods")
	if err := r.ensureReplicas(ctx, &workload, &pods); err != nil {
		log.Error(err, "Failed to ensure replicas")
		return ctrl.Result{}, err
	}
	log.Info("Successfully ensured desired number of Pods")

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

func (r *PartitionedWorkloadReconciler) ensureReplicas(ctx context.Context, workload *workloadsv1.PartitionedWorkload, pods *corev1.PodList) error {
	log := logf.FromContext(ctx)
	log.Info("Ensuring replicas", "desiredReplicas", workload.Spec.Replicas, "currentReplicas", len(pods.Items))

	// Create new Pods if needed
	if int32(len(pods.Items)) < workload.Spec.Replicas {
		log.Info("Current Pods", "count", len(pods.Items))
		for i := int32(len(pods.Items)); i < workload.Spec.Replicas; i++ {
			newPod := corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					GenerateName: workload.Name + "-",
					Namespace:    workload.Namespace,
					Labels: map[string]string{
						"partitionedworkload": workload.Name,
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
				log.Error(err, "Failed to create Pod")
				return fmt.Errorf("failed to create Pod: %w", err)
			}
			log.Info("Created new Pod", "podName", newPod.Name)
		}
	}

	// Delete excess Pods if needed
	if int32(len(pods.Items)) > workload.Spec.Replicas {
		for i := int32(len(pods.Items)) - 1; i >= workload.Spec.Replicas; i-- {
			// Check if the Pod exists before deleting
			var pod corev1.Pod
			if err := r.Get(ctx, client.ObjectKey{Name: pods.Items[i].Name, Namespace: pods.Items[i].Namespace}, &pod); err != nil {
				if apierrors.IsNotFound(err) {
					log.Info("Pod not found, skipping deletion", "podName", pods.Items[i].Name)
					continue
				}
				log.Error(err, "Failed to get Pod for deletion")
				return fmt.Errorf("failed to get Pod for deletion: %w", err)
			}

			// Delete the Pod
			if err := r.Delete(ctx, &pods.Items[i]); err != nil {
				log.Error(err, "Failed to delete Pod")
				return fmt.Errorf("failed to delete Pod: %w", err)
			}
			log.Info("Deleted excess Pod", "podName", pods.Items[i].Name)
		}
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
