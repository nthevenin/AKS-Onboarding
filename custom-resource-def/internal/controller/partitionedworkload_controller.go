package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	workloadsv1 "github.com/nthevenin/custom-workload-operator/api/v1"
	appsv1 "k8s.io/api/apps/v1"
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

	// Step 1: List existing ControllerRevisions
	var revisions appsv1.ControllerRevisionList
	if err := r.List(ctx, &revisions, client.InNamespace(workload.Namespace), client.MatchingLabels{
		"partitionedworkload": workload.Name,
	}); err != nil {
		log.Error(err, "Failed to list ControllerRevisions")
		return err
	}

	// Step 2: Sort and categorize ControllerRevisions
	sort.Slice(revisions.Items, func(i, j int) bool {
		return revisions.Items[i].Revision < revisions.Items[j].Revision
	})

	log.Info("Listed ControllerRevisions", "revisionCount", len(revisions.Items))
	for _, rev := range revisions.Items {
		log.Info("ControllerRevision", "name", rev.Name, "revision", rev.Revision)
	}

	// Identify the current and previous revisions
	var currentRevision *appsv1.ControllerRevision
	var oldRevisions []*appsv1.ControllerRevision

	if len(revisions.Items) > 0 {
		currentRevision = &revisions.Items[len(revisions.Items)-1] // Most recent revision

		// Convert []appsv1.ControllerRevision to []*appsv1.ControllerRevision
		for i := 0; i < len(revisions.Items)-1; i++ {
			oldRevisions = append(oldRevisions, &revisions.Items[i])
		}
	}

	log.Info("Identified revisions", "currentRevision", currentRevision.Name, "oldRevisionCount", len(oldRevisions))
	// Step 3: Create a new ControllerRevision if the PodTemplateSpec has changed
	serializedTemplate, err := json.Marshal(workload.Spec.PodTemplate)
	if err != nil {
		log.Error(err, "Failed to serialize PodTemplateSpec")
		return err
	}
	if currentRevision == nil || !bytes.Equal(currentRevision.Data.Raw, serializedTemplate) {
		newRevision := &appsv1.ControllerRevision{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("%s-%d", workload.Name, time.Now().UnixNano()),
				Namespace: workload.Namespace,
				Labels: map[string]string{
					"partitionedworkload": workload.Name,
				},
				OwnerReferences: []metav1.OwnerReference{
					*metav1.NewControllerRef(workload, workloadsv1.GroupVersion.WithKind("PartitionedWorkload")),
				},
			},
			Data: runtime.RawExtension{
				Raw: serializedTemplate,
			},
			Revision: func() int64 {
				if currentRevision != nil {
					return currentRevision.Revision + 1
				}
				return 1
			}(),
		}
		if err := r.Create(ctx, newRevision); err != nil {
			log.Error(err, "Failed to create ControllerRevision")
			return err
		}
		currentRevision = newRevision
		log.Info("Created new ControllerRevision", "revision", currentRevision.Revision)
	}

	// Step 4: Categorize Pods by revision
	var oldPods, newPods []corev1.Pod
	for _, pod := range pods.Items {
		if pod.Labels["controller-revision-hash"] == currentRevision.Name {
			newPods = append(newPods, pod)
		} else {
			for _, oldRev := range oldRevisions {
				if pod.Labels["controller-revision-hash"] == oldRev.Name {
					oldPods = append(oldPods, pod)
					break
				}
			}
		}
	}
	log.Info("Categorized pods by ControllerRevision", "oldPodsCount", len(oldPods), "newPodsCount", len(newPods))

	// Step 5: Calculate desired counts
	desiredOldPods := workload.Spec.PartitionCount
	desiredNewPods := workload.Spec.Replicas - desiredOldPods
	log.Info("Calculated desired pod counts", "desiredOldPods", desiredOldPods, "desiredNewPods", desiredNewPods)

	// Step 6: Reconciliation plan
	var podsToDelete []corev1.Pod
	podsToCreate := 0

	// Retain the desired number of old Pods
	if len(oldPods) > int(desiredOldPods) {
		podsToDelete = append(podsToDelete, oldPods[int(desiredOldPods):]...)
		oldPods = oldPods[:int(desiredOldPods)]
	}

	// Delete excess new Pods
	if len(newPods) > int(desiredNewPods) {
		podsToDelete = append(podsToDelete, newPods[int(desiredNewPods):]...)
		newPods = newPods[:int(desiredNewPods)]
	}

	// Determine Pods to create
	totalPods := len(oldPods) + len(newPods)
	if totalPods < int(workload.Spec.Replicas) {
		podsToCreate = int(workload.Spec.Replicas) - totalPods
	}
	log.Info("Reconciliation plan", "podsToDeleteCount", len(podsToDelete), "podsToCreateCount", podsToCreate)

	// Step 7: Execute the reconciliation plan
	// Delete Pods
	for _, pod := range podsToDelete {
		if err := r.Delete(ctx, &pod); err != nil && !apierrors.IsNotFound(err) {
			log.Error(err, "Failed to delete Pod", "podName", pod.Name)
			return err
		}
		log.Info("Deleted Pod", "podName", pod.Name)
	}

	// Create new Pods
	for i := 0; i < podsToCreate; i++ {
		newPod := corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: workload.Name + "-",
				Namespace:    workload.Namespace,
				Labels: map[string]string{
					"partitionedworkload":      workload.Name,
					"controller-revision-hash": currentRevision.Name,
				},
			},
			Spec: corev1.PodSpec{
				Containers: workload.Spec.PodTemplate.Containers,
			},
		}
		if err := ctrl.SetControllerReference(workload, &newPod, r.Scheme); err != nil {
			log.Error(err, "Failed to set OwnerReference for Pod")
			return err
		}
		if err := r.Create(ctx, &newPod); err != nil {
			log.Error(err, "Failed to create new Pod")
			return err
		}
		log.Info("Created new Pod", "podName", newPod.Name)
	}

	// Step 8: Final check
	totalPods = len(oldPods) + len(newPods)
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

// SetupWithManager sets up the controller with the Manager
func (r *PartitionedWorkloadReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&workloadsv1.PartitionedWorkload{}).
		Owns(&corev1.Pod{}). // Ensure the controller watches Pods
		Complete(r)
}
