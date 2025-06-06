/*

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

package mutatorstatus

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/go-logr/logr"
	mutationsv1 "github.com/open-policy-agent/gatekeeper/v3/apis/mutations/v1"
	mutationsv1alpha1 "github.com/open-policy-agent/gatekeeper/v3/apis/mutations/v1alpha1"
	"github.com/open-policy-agent/gatekeeper/v3/apis/status/v1beta1"
	"github.com/open-policy-agent/gatekeeper/v3/pkg/logging"
	"github.com/open-policy-agent/gatekeeper/v3/pkg/operations"
	"github.com/open-policy-agent/gatekeeper/v3/pkg/readiness"
	"github.com/open-policy-agent/gatekeeper/v3/pkg/util"
	"github.com/open-policy-agent/gatekeeper/v3/pkg/watch"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

var log = logf.Log.WithName("controller").WithValues(logging.Process, "mutator_status_controller")

type Adder struct {
	WatchManager *watch.Manager
}

func (a *Adder) InjectTracker(_ *readiness.Tracker) {}

// Add creates a new Mutator Status Controller and adds it to the Manager. The Manager will set fields on the Controller
// and Start it when the Manager is Started.
func (a *Adder) Add(mgr manager.Manager) error {
	if !operations.IsAssigned(operations.MutationStatus) {
		return nil
	}
	r := newReconciler(mgr)
	return add(mgr, r)
}

// newReconciler returns a new reconcile.Reconciler.
func newReconciler(
	mgr manager.Manager,
) reconcile.Reconciler {
	return &ReconcileMutatorStatus{
		// Separate reader and writer because manager's default client bypasses the cache for unstructured resources.
		writer:       mgr.GetClient(),
		statusClient: mgr.GetClient(),
		reader:       mgr.GetCache(),
		scheme:       mgr.GetScheme(),
		log:          log,
	}
}

type PackerMap func(obj client.Object) []reconcile.Request

// PodStatusToMutatorMapper correlates a MutatorPodStatus with its corresponding mutator.
func PodStatusToMutatorMapper(selfOnly bool, kindMatch string, packerMap handler.MapFunc) handler.TypedMapFunc[*v1beta1.MutatorPodStatus, reconcile.Request] {
	return func(ctx context.Context, obj *v1beta1.MutatorPodStatus) []reconcile.Request {
		labels := obj.GetLabels()
		name, ok := labels[v1beta1.MutatorNameLabel]
		if !ok {
			log.Error(fmt.Errorf("mutator status resource with no name label: %s", obj.GetName()), "missing label while attempting to map a mutator status resource")
			return nil
		}
		kind, ok := labels[v1beta1.MutatorKindLabel]
		if !ok {
			log.Error(fmt.Errorf("mutator status resource with no kind label: %s", obj.GetName()), "missing label while attempting to map a mutator status resource")
			return nil
		}
		if kindMatch != "" && kind != kindMatch {
			return nil
		}
		if selfOnly {
			pod, ok := labels[v1beta1.PodLabel]
			if !ok {
				log.Error(fmt.Errorf("mutator status resource with no pod label: %s", obj.GetName()), "missing label while attempting to map a mutator status resource")
			}
			// Do not attempt to reconcile the resource when other pods have changed their status
			if pod != util.GetPodName() {
				return nil
			}
		}
		u := &unstructured.Unstructured{}
		// AssignImage is the only mutator in v1alpha1 still
		v := "v1"
		if kind == "AssignImage" {
			v = "v1alpha1"
		}
		u.SetGroupVersionKind(schema.GroupVersionKind{Group: v1beta1.MutationsGroup, Version: v, Kind: kind})
		u.SetName(name)
		return packerMap(ctx, u)
	}
}

func eventPackerMapFuncHardcodeGVKForAssign(gvk schema.GroupVersionKind) handler.TypedMapFunc[*mutationsv1.Assign, reconcile.Request] {
	mf := util.EventPackerMapFunc()
	return func(ctx context.Context, obj *mutationsv1.Assign) []reconcile.Request {
		u := &unstructured.Unstructured{}
		u.SetGroupVersionKind(gvk)
		u.SetNamespace(obj.GetNamespace())
		u.SetName(obj.GetName())
		return mf(ctx, u)
	}
}

func eventPackerMapFuncHardcodeGVKForAssignMetadata(gvk schema.GroupVersionKind) handler.TypedMapFunc[*mutationsv1.AssignMetadata, reconcile.Request] {
	mf := util.EventPackerMapFunc()
	return func(ctx context.Context, obj *mutationsv1.AssignMetadata) []reconcile.Request {
		u := &unstructured.Unstructured{}
		u.SetGroupVersionKind(gvk)
		u.SetNamespace(obj.GetNamespace())
		u.SetName(obj.GetName())
		return mf(ctx, u)
	}
}

func eventPackerMapFuncHardcodeGVKForAssignImage(gvk schema.GroupVersionKind) handler.TypedMapFunc[*mutationsv1alpha1.AssignImage, reconcile.Request] {
	mf := util.EventPackerMapFunc()
	return func(ctx context.Context, obj *mutationsv1alpha1.AssignImage) []reconcile.Request {
		u := &unstructured.Unstructured{}
		u.SetGroupVersionKind(gvk)
		u.SetNamespace(obj.GetNamespace())
		u.SetName(obj.GetName())
		return mf(ctx, u)
	}
}

func eventPackerMapFuncHardcodeGVKForModifySet(gvk schema.GroupVersionKind) handler.TypedMapFunc[*mutationsv1.ModifySet, reconcile.Request] {
	mf := util.EventPackerMapFunc()
	return func(ctx context.Context, obj *mutationsv1.ModifySet) []reconcile.Request {
		u := &unstructured.Unstructured{}
		u.SetGroupVersionKind(gvk)
		u.SetNamespace(obj.GetNamespace())
		u.SetName(obj.GetName())
		return mf(ctx, u)
	}
}

// add adds a new Controller to mgr with r as the reconcile.Reconciler.
func add(mgr manager.Manager, r reconcile.Reconciler) error {
	// Create a new controller
	c, err := controller.New("mutator-status-controller", mgr, controller.Options{Reconciler: r})
	if err != nil {
		return err
	}

	// Watch for changes to MutatorStatus
	err = c.Watch(
		source.Kind(mgr.GetCache(), &v1beta1.MutatorPodStatus{},
			handler.TypedEnqueueRequestsFromMapFunc(PodStatusToMutatorMapper(false, "", util.EventPackerMapFunc())),
		))
	if err != nil {
		return err
	}

	// Watch for changes to mutators
	err = c.Watch(
		source.Kind(mgr.GetCache(), &mutationsv1.Assign{},
			handler.TypedEnqueueRequestsFromMapFunc(eventPackerMapFuncHardcodeGVKForAssign(schema.GroupVersionKind{Group: v1beta1.MutationsGroup, Version: "v1", Kind: "Assign"}))))
	if err != nil {
		return err
	}
	err = c.Watch(
		source.Kind(mgr.GetCache(), &mutationsv1.AssignMetadata{},
			handler.TypedEnqueueRequestsFromMapFunc(eventPackerMapFuncHardcodeGVKForAssignMetadata(schema.GroupVersionKind{Group: v1beta1.MutationsGroup, Version: "v1", Kind: "AssignMetadata"})),
		))
	if err != nil {
		return err
	}
	err = c.Watch(
		source.Kind(mgr.GetCache(), &mutationsv1alpha1.AssignImage{},
			handler.TypedEnqueueRequestsFromMapFunc(eventPackerMapFuncHardcodeGVKForAssignImage(schema.GroupVersionKind{Group: v1beta1.MutationsGroup, Version: "v1alpha1", Kind: "AssignImage"})),
		))
	if err != nil {
		return err
	}
	return c.Watch(
		source.Kind(mgr.GetCache(), &mutationsv1.ModifySet{},
			handler.TypedEnqueueRequestsFromMapFunc(eventPackerMapFuncHardcodeGVKForModifySet(schema.GroupVersionKind{Group: v1beta1.MutationsGroup, Version: "v1", Kind: "ModifySet"})),
		))
}

var _ reconcile.Reconciler = &ReconcileMutatorStatus{}

// ReconcileMutatorStatus reconciles an arbitrary mutator object described by Kind.
type ReconcileMutatorStatus struct {
	reader       client.Reader
	writer       client.Writer
	statusClient client.StatusClient
	scheme       *runtime.Scheme
	log          logr.Logger
}

// +kubebuilder:rbac:groups=mutations.gatekeeper.sh,resources=*,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=status.gatekeeper.sh,resources=*,verbs=get;list;watch;create;update;patch;delete

// Reconcile reads that state of the cluster for a mutator object and makes changes based on the state read
// and what is in the mutator.Spec.
func (r *ReconcileMutatorStatus) Reconcile(ctx context.Context, request reconcile.Request) (reconcile.Result, error) {
	gvk, unpackedRequest, err := util.UnpackRequest(request)
	if err != nil {
		// Unrecoverable, do not retry.
		log.Error(err, "unpacking request", "request", request)
		return reconcile.Result{}, nil
	}

	// Sanity - make sure it is a mutator resource.
	if gvk.Group != v1beta1.MutationsGroup {
		// Unrecoverable, do not retry.
		log.Error(err, "invalid mutator GroupVersion", "gvk", gvk, "name", unpackedRequest.NamespacedName)
		return reconcile.Result{}, nil
	}

	instance := &unstructured.Unstructured{}
	instance.SetGroupVersionKind(gvk)
	if err := r.reader.Get(ctx, unpackedRequest.NamespacedName, instance); err != nil {
		// If the mutator does not exist, we are done
		if errors.IsNotFound(err) {
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, err
	}

	r.log.Info("handling mutator status update", "instance", instance)

	sObjs := &v1beta1.MutatorPodStatusList{}
	if err := r.reader.List(
		ctx,
		sObjs,
		client.MatchingLabels{
			v1beta1.MutatorNameLabel: instance.GetName(),
			v1beta1.MutatorKindLabel: instance.GetKind(),
		},
		client.InNamespace(util.GetNamespace()),
	); err != nil {
		return reconcile.Result{}, err
	}

	statusObjs := make(sortableStatuses, len(sObjs.Items))
	copy(statusObjs, sObjs.Items)
	sort.Sort(statusObjs)

	var s []interface{}
	for i := range statusObjs {
		// Don't report status if it's not for the correct object. This can happen
		// if a watch gets interrupted, causing the mutator status to be deleted
		// out from underneath it
		if statusObjs[i].Status.MutatorUID != instance.GetUID() {
			continue
		}
		j, err := json.Marshal(statusObjs[i].Status)
		if err != nil {
			return reconcile.Result{}, err
		}
		var o map[string]interface{}
		if err := json.Unmarshal(j, &o); err != nil {
			return reconcile.Result{}, err
		}
		s = append(s, o)
	}

	if err := unstructured.SetNestedSlice(instance.Object, s, "status", "byPod"); err != nil {
		return reconcile.Result{}, err
	}

	if err = r.statusClient.Status().Update(ctx, instance); err != nil {
		return reconcile.Result{Requeue: true}, nil
	}

	return reconcile.Result{}, nil
}

type sortableStatuses []v1beta1.MutatorPodStatus

func (s sortableStatuses) Len() int {
	return len(s)
}

func (s sortableStatuses) Less(i, j int) bool {
	return s[i].Status.ID < s[j].Status.ID
}

func (s sortableStatuses) Swap(i, j int) {
	s[i], s[j] = s[j], s[i]
}
