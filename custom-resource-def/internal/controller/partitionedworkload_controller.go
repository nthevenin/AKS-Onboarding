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

	// Logging the identified revisions
	log.Info("Identified revisions", "currentRevision", func() string {
		if currentRevision != nil {
			return currentRevision.Name
		}
		return "nil"
	}(), "oldRevisionCount", len(oldRevisions))

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

		// Insert the previous current revision into oldRevisions if it exists
		if currentRevision != nil {
			oldRevisions = append([]*appsv1.ControllerRevision{currentRevision}, oldRevisions...)
		}

		currentRevision = newRevision
		log.Info("Created new ControllerRevision", "revision", currentRevision.Revision)
	}

	// Step 4: Map pods to their revisions for better tracking
	podsByRevision := make(map[string][]corev1.Pod)
	var uncategorizedPods []corev1.Pod

	// Create a hashmap of revisions for quick access
	revisionMap := make(map[string]*appsv1.ControllerRevision)
	if currentRevision != nil {
		revisionMap[currentRevision.Name] = currentRevision
	}
	for _, rev := range oldRevisions {
		revisionMap[rev.Name] = rev
	}

	// Categorize each pod by its revision
	for _, pod := range pods.Items {
		revHash, exists := pod.Labels["controller-revision-hash"]
		if exists && revHash != "" {
			if _, found := revisionMap[revHash]; found {
				podsByRevision[revHash] = append(podsByRevision[revHash], pod)
			} else {
				// Important: Don't treat these pods as uncategorized if we're in a transition
				// They might be from a previous revision that's still valid
				if len(oldRevisions) > 0 {
					// Try to associate with the newest old revision if possible
					podsByRevision[oldRevisions[0].Name] = append(podsByRevision[oldRevisions[0].Name], pod)
					log.Info("Reassigned pod to newest old revision",
						"podName", pod.Name,
						"originalRevision", revHash,
						"newRevision", oldRevisions[0].Name)
				} else {
					uncategorizedPods = append(uncategorizedPods, pod)
				}
			}
		} else {
			uncategorizedPods = append(uncategorizedPods, pod)
		}
	}

	// Only delete truly uncategorized pods - keep others during transition
	if len(uncategorizedPods) > 0 {
		log.Info("Found uncategorized pods", "count", len(uncategorizedPods))
		for _, pod := range uncategorizedPods {
			log.Info("Deleting uncategorized pod", "podName", pod.Name)
			if err := r.Delete(ctx, &pod); err != nil && !apierrors.IsNotFound(err) {
				log.Error(err, "Failed to delete uncategorized pod", "podName", pod.Name)
				return err
			}
		}
	}

	// Step 5: Calculate desired counts
	desiredOldPods := int(workload.Spec.PartitionCount)
	desiredNewPods := int(workload.Spec.Replicas - workload.Spec.PartitionCount)
	log.Info("Calculated desired pod counts", "desiredOldPods", desiredOldPods, "desiredNewPods", desiredNewPods)

	// Step 6: Reconciliation plan
	var podsToDelete []corev1.Pod
	var podsToRetain []corev1.Pod

	var currentPods []corev1.Pod
	if currentRevision != nil {
		currentPods = podsByRevision[currentRevision.Name]
	}

	// Collect older pods, sorted by revision (newest old revision first)
	var oldPods []corev1.Pod
	if len(oldRevisions) > 0 {
		// Start with the newest old revision
		for _, revision := range oldRevisions {
			oldPods = append(oldPods, podsByRevision[revision.Name]...)
		}
	}

	// Keep the newest oldPods up to desiredOldPods count
	if len(oldPods) > desiredOldPods {
		// Keep the newest old pods (which are at the beginning of the slice)
		podsToRetain = append(podsToRetain, oldPods[:desiredOldPods]...)
		podsToDelete = append(podsToDelete, oldPods[desiredOldPods:]...)
	} else {
		podsToRetain = append(podsToRetain, oldPods...)
	}

	// Keep current pods up to the desiredNewPods count
	if len(currentPods) > desiredNewPods {
		podsToRetain = append(podsToRetain, currentPods[:desiredNewPods]...)
		podsToDelete = append(podsToDelete, currentPods[desiredNewPods:]...)
	} else {
		podsToRetain = append(podsToRetain, currentPods...)
	}

	// Calculate how many pods we need to create
	podsToCreate := desiredOldPods + desiredNewPods - len(podsToRetain)

	log.Info("Reconciliation plan",
		"podsToDeleteCount", len(podsToDelete),
		"podsToRetainCount", len(podsToRetain),
		"podsToCreateCount", podsToCreate,
		"oldPodsAvailable", len(oldPods),
		"currentPodsAvailable", len(currentPods))

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

	oldPodsToCreate := desiredOldPods - len(oldPods)
	if oldPodsToCreate > 0 && len(oldRevisions) > 0 {
		// If we need more old pods and have old revisions, create them with the most recent old revision
		mostRecentOldRevision := oldRevisions[0]

		// Try to deserialize different formats
		var oldTemplate workloadsv1.PodTemplateSpec

		if err := json.Unmarshal(mostRecentOldRevision.Data.Raw, &oldTemplate); err != nil {
			// Try to deserialize as a full PodTemplateSpec first
			log.Error(err, "Failed to deserialize old PodTemplateSpec directly",
				"revisionName", mostRecentOldRevision.Name)

			// Try to deserialize as wrapped in another structure
			var oldWorkload struct {
				Spec struct {
					PodTemplate workloadsv1.PodTemplateSpec `json:"podTemplate"`
				} `json:"spec"`
			}

			if err := json.Unmarshal(mostRecentOldRevision.Data.Raw, &oldWorkload); err != nil {
				log.Error(err, "Failed to deserialize old PodTemplateSpec as wrapped structure",
					"revisionName", mostRecentOldRevision.Name)
				return err
			}

			oldTemplate = oldWorkload.Spec.PodTemplate
		}

		// Verify containers in the old template
		if len(oldTemplate.Containers) == 0 {
			log.Error(nil, "Old template has no containers after attempted deserialization",
				"revisionName", mostRecentOldRevision.Name,
				"rawData", string(mostRecentOldRevision.Data.Raw))

			// Try to find any valid old revision with containers
			var validOldRevision *appsv1.ControllerRevision
			for _, revision := range oldRevisions {
				var testTemplate workloadsv1.PodTemplateSpec
				if err := json.Unmarshal(revision.Data.Raw, &testTemplate); err == nil && len(testTemplate.Containers) > 0 {
					validOldRevision = revision
					oldTemplate = testTemplate
					log.Info("Found valid old revision with containers",
						"revisionName", validOldRevision.Name,
						"containerCount", len(oldTemplate.Containers))
					break
				}

				// Try the wrapped structure
				var oldWorkload struct {
					Spec struct {
						PodTemplate workloadsv1.PodTemplateSpec `json:"podTemplate"`
					} `json:"spec"`
				}
				if err := json.Unmarshal(revision.Data.Raw, &oldWorkload); err == nil &&
					len(oldWorkload.Spec.PodTemplate.Containers) > 0 {
					validOldRevision = revision
					oldTemplate = oldWorkload.Spec.PodTemplate
					log.Info("Found valid old revision with containers in wrapped structure",
						"revisionName", validOldRevision.Name,
						"containerCount", len(oldTemplate.Containers))
					break
				}
			}

			if validOldRevision == nil {
				log.Info("No valid old revision found, using current template as fallback")
				oldTemplate = workload.Spec.PodTemplate
			} else {
				mostRecentOldRevision = validOldRevision
			}
		}

		log.Info("Creating new pods with old revision",
			"revisionName", mostRecentOldRevision.Name,
			"containerCount", len(oldTemplate.Containers),
			"containerImage", func() string {
				if len(oldTemplate.Containers) > 0 {
					return oldTemplate.Containers[0].Image
				}
				return "unknown"
			}(),
			"count", oldPodsToCreate)

		for i := 0; i < oldPodsToCreate; i++ {
			newPod := corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					GenerateName: workload.Name + "-",
					Namespace:    workload.Namespace,
					Labels: map[string]string{
						"partitionedworkload":      workload.Name,
						"controller-revision-hash": mostRecentOldRevision.Name,
					},
				},
				Spec: corev1.PodSpec{
					Containers: oldTemplate.Containers,
					SecurityContext: &corev1.PodSecurityContext{
						SeccompProfile: &corev1.SeccompProfile{
							Type: corev1.SeccompProfileTypeRuntimeDefault,
						},
					},
				},
			}

			if err := ctrl.SetControllerReference(workload, &newPod, r.Scheme); err != nil {
				log.Error(err, "Failed to set OwnerReference for Pod")
				return err
			}
			if err := r.Create(ctx, &newPod); err != nil {
				log.Error(err, "Failed to create new Pod with old revision",
					"revisionName", mostRecentOldRevision.Name)
				return err
			}
			log.Info("Created new Pod with old revision",
				"podName", newPod.Name,
				"revisionName", mostRecentOldRevision.Name)
		}
	}

	newPodsToCreate := desiredNewPods - len(currentPods)
	if newPodsToCreate > 0 && currentRevision != nil {
		// Create pods with the current revision
		log.Info("Creating new pods with current revision",
			"revisionName", currentRevision.Name,
			"containerImage", workload.Spec.PodTemplate.Containers[0].Image,
			"count", newPodsToCreate)

		for i := 0; i < newPodsToCreate; i++ {
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
					SecurityContext: &corev1.PodSecurityContext{
						SeccompProfile: &corev1.SeccompProfile{
							Type: corev1.SeccompProfileTypeRuntimeDefault,
						},
					},
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
	}

	// Step 8: Final validation
	// Recount pods to ensure we have the correct amount
	var finalPods corev1.PodList
	if err := r.List(ctx, &finalPods, client.InNamespace(workload.Namespace), client.MatchingLabels{
		"partitionedworkload": workload.Name,
	}); err != nil {
		log.Error(err, "Failed to list Pods for final validation")
		return err
	}

	if len(finalPods.Items) != int(workload.Spec.Replicas) {
		log.Error(nil, "Mismatch in total pod count after reconciliation",
			"expected", workload.Spec.Replicas, "actual", len(finalPods.Items))
		// Adding retry mechanism in case of mismatch
		return fmt.Errorf("mismatch in total pod count: expected %d, got %d",
			workload.Spec.Replicas, len(finalPods.Items))
	} else {
		log.Info("Reconciliation successful - pod count matches desired replicas", "podCount", len(finalPods.Items))
	}

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
