package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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

	// Generate the current pod-template-hash for the workload's PodTemplateSpec
	currentHash := generatePodTemplateHash(workload.Spec.PodTemplate)

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
	if err := r.ensureReplicasAndPartition(ctx, &workload, &pods, currentHash); err != nil {
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

func (r *PartitionedWorkloadReconciler) ensureReplicasAndPartition(ctx context.Context, workload *workloadsv1.PartitionedWorkload, pods *corev1.PodList, currentHash string) error {
	log := logf.FromContext(ctx)
	log.Info("Ensuring replicas and partitioning", "desiredReplicas", workload.Spec.Replicas, "partitionCount", workload.Spec.PartitionCount, "currentHash", currentHash)

	// Separate pods into old (by previous hash) and new (by current hash)
	var oldPods, newPods []corev1.Pod
	for _, pod := range pods.Items {
		if pod.DeletionTimestamp != nil {
			log.Info("Skipping terminating Pod", "podName", pod.Name)
			continue
		}
		if pod.Labels["pod-template-hash"] == currentHash {
			newPods = append(newPods, pod)
		} else {
			oldPods = append(oldPods, pod)
		}
	}
	log.Info("Categorized pods by pod-template-hash", "oldPodsCount", len(oldPods), "newPodsCount", len(newPods))

	// Calculate desired counts
	desiredOldPods := workload.Spec.PartitionCount
	desiredNewPods := workload.Spec.Replicas - desiredOldPods
	log.Info("Calculated desired pod counts", "desiredOldPods", desiredOldPods, "desiredNewPods", desiredNewPods)

	// Create a reconciliation plan
	var podsToDelete []corev1.Pod
	podsToCreate := 0

	// Delete excess old pods if there are more than desiredOldPods
	if len(oldPods) > int(desiredOldPods) {
		podsToDelete = append(podsToDelete, oldPods[int(desiredOldPods):]...)
		oldPods = oldPods[:int(desiredOldPods)]
	}

	// Delete excess new pods if there are more than desiredNewPods
	if len(newPods) > int(desiredNewPods) {
		podsToDelete = append(podsToDelete, newPods[int(desiredNewPods):]...)
		newPods = newPods[:int(desiredNewPods)]
	}

	// Determine the number of new pods to create
	if len(newPods) < int(desiredNewPods) {
		podsToCreate = int(desiredNewPods) - len(newPods)
	}
	log.Info("Reconciliation plan", "podsToDeleteCount", len(podsToDelete), "podsToCreateCount", podsToCreate)

	// Execute the reconciliation plan
	// Delete excess pods
	for _, pod := range podsToDelete {
		if err := r.Delete(ctx, &pod); err != nil {
			if client.IgnoreNotFound(err) != nil {
				log.Error(err, "Failed to delete Pod", "podName", pod.Name)
				return fmt.Errorf("failed to delete Pod: %w", err)
			}
		}
		log.Info("Deleted Pod", "podName", pod.Name)
	}

	// Create new pods
	for i := 0; i < podsToCreate; i++ {
		newPod := corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: workload.Name + "-",
				Namespace:    workload.Namespace,
				Labels: map[string]string{
					"partitionedworkload": workload.Name,
					"pod-template-hash":   currentHash, // Add the current hash as a label
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
			log.Error(err, "Failed to create new Pod")
			return fmt.Errorf("failed to create new Pod: %w", err)
		}
		log.Info("Created new Pod", "podName", newPod.Name)

		// Add the newly created pod to the newPods list
		newPods = append(newPods, newPod)
	}

	// Final check to ensure total pod count matches desired replicas
	totalPods := len(oldPods) + len(newPods)
	if totalPods != int(workload.Spec.Replicas) {
		log.Error(nil, "Mismatch in total pod count after reconciliation", "expected", workload.Spec.Replicas, "actual", totalPods)
		return fmt.Errorf("mismatch in total pod count: expected %d, got %d", workload.Spec.Replicas, totalPods)
	}

	log.Info("Reconciliation complete", "totalPods", totalPods, "desiredOldPods", desiredOldPods, "desiredNewPods", desiredNewPods)
	return nil
}

// updateStatus updates the status of the PartitionedWorkload resource
func (r *PartitionedWorkloadReconciler) updateStatus(ctx context.Context, workload *workloadsv1.PartitionedWorkload, pods *corev1.PodList) error {
	log := logf.FromContext(ctx)
	log.Info("Updating status for PartitionedWorkload", "name", workload.Name)

	// Count available replicas
	available := int32(0)
	for _, pod := range pods.Items {
		if pod.Status.Phase == corev1.PodRunning {
			available++
		}
	}

	workload.Status.AvailableReplicas = available
	if err := r.Status().Update(ctx, workload); err != nil {
		if apierrors.IsConflict(err) {
			log.Info("Conflict while updating status, retrying")
			return fmt.Errorf("conflict while updating status, requeueing")
		}
		log.Error(err, "Failed to update status")
		return fmt.Errorf("failed to update status: %w", err)
	}

	log.Info("Updated status", "availableReplicas", available)
	return nil
}

// generatePodTemplateHash generates a truncated hash for the given PodTemplateSpec
func generatePodTemplateHash(template workloadsv1.PodTemplateSpec) string {
	hash := sha256.New()
	hash.Write([]byte(fmt.Sprintf("%v", template)))
	fullHash := hex.EncodeToString(hash.Sum(nil))
	return fullHash[:16] // Truncate to the first 16 characters
}

// SetupWithManager sets up the controller with the Manager
func (r *PartitionedWorkloadReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&workloadsv1.PartitionedWorkload{}).
		Owns(&corev1.Pod{}). // Ensure the controller watches Pods
		Complete(r)
}
