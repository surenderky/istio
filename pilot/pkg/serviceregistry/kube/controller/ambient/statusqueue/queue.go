// Copyright Istio Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package statusqueue

import (
	"k8s.io/apimachinery/pkg/types"

	"istio.io/istio/pilot/pkg/model"
	"istio.io/istio/pkg/kube/controllers"
	"istio.io/istio/pkg/kube/kclient"
	"istio.io/istio/pkg/kube/krt"
	istiolog "istio.io/istio/pkg/log"
)

var log = istiolog.RegisterScope("status", "status reporting")

type StatusQueue struct {
	queue controllers.Queue

	// reporters is a mapping of unique controller name -> status information.
	// Note: this is user facing in the fieldManager!
	reporters map[string]statusReporter
}

// statusItem represents the objects stored on the queue
type statusItem struct {
	Key      string
	Reporter string
}

// NewQueue builds a new status queue.
func NewQueue() *StatusQueue {
	sq := &StatusQueue{
		reporters: make(map[string]statusReporter),
	}
	sq.queue = controllers.NewQueue("ambient status",
		controllers.WithGenericReconciler(sq.reconcile),
		controllers.WithMaxAttempts(5))
	return sq
}

// Run starts the queue, which will process items until the channel is closed
func (q *StatusQueue) Run(stop <-chan struct{}) {
	for _, r := range q.reporters {
		r.start()
	}
	q.queue.Run(stop)
}

// reconcile processes a single queue item
func (q *StatusQueue) reconcile(raw any) error {
	key := raw.(statusItem)
	log := log.WithLabels("key", key.Key)
	log.Debugf("reconciling status")

	reporter, f := q.reporters[key.Reporter]
	if !f {
		log.Fatalf("impossible; an item was enqueued with an unknown reporter")
	}
	obj, f := reporter.getObject(key.Key)
	if !f {
		log.Infof("object is removed, no action needed")
		return nil
	}
	// Fetch the client to apply patches, and the set of current conditions
	patcher, currentConditions := reporter.patcher(obj)
	// Turn the conditions into a patch. Using currentConditions, this will determine whether we can skip the patch entirely
	// or if we need to send an empty patch. With an empty patch, SSA will automatically prune out anything *we* (identified by the fieldManager) wrote.
	//
	// This gives us essentially the same behavior as always writing, but saves a lot of writes... with the caveat of one race condition:
	// the object can change between when we fetch the currentConditions and apply the patch.
	// * Condition was there, but is now removed: No problem, we will at worst do a patch that wasn't needed.
	// * Condition was not there, but now it was added: clearly some other controller is writing the same type as us, which is not really allowed.
	targetObject := obj.GetStatusTarget()
	status := translateToPatch(targetObject, obj.GetConditions(), currentConditions)

	if status == nil {
		log.Debugf("no status to write")
		return nil
	}
	log.Debugf("writing patch %v", string(status))
	// Pass key.Reporter as the fieldManager. This ensures we have a unique value there.
	// This means we could have multiple unique writers for the same object, as long as they have a unique set of conditions.
	return patcher.ApplyStatus(targetObject.Name, targetObject.Namespace, types.ApplyPatchType, status, key.Reporter)
}

// StatusWriter is a type that can write status messages
type StatusWriter interface {
	// GetStatusTarget returns the metadata about the object we are writing to
	GetStatusTarget() model.TypedObject
	// GetConditions returns a set of conditions for the object
	GetConditions() model.ConditionSet
}

// statusReporter is a generics-erased object storing context on how to write status for a given type.
type statusReporter struct {
	getObject func(string) (StatusWriter, bool)
	patcher   func(StatusWriter) (kclient.Patcher, []string)
	start     func()
}

// Register registers a collection to have status reconciled.
// The Collection is expected to produce objects that implement StatusWriter, which tells us what status to write.
// The name is user facing, and ends up as a fieldManager for server-side-apply. It must be unique.
func Register[T StatusWriter](q *StatusQueue, name string, col krt.Collection[T], getPatcher func(T) (kclient.Patcher, []string)) {
	sr := statusReporter{
		getObject: func(s string) (StatusWriter, bool) {
			if o := col.GetKey(krt.Key[T](s)); o != nil {
				return *o, true
			}
			return nil, false
		},
		// Wrapper to remove generics
		patcher: func(writer StatusWriter) (kclient.Patcher, []string) {
			return getPatcher(writer.(T))
		},
		start: func() {
			col.Register(func(o krt.Event[T]) {
				ol := o.Latest()
				key := string(krt.GetKey(ol))
				log.Debugf("registering key for processing: %s", key)
				q.queue.Add(statusItem{
					Key:      key,
					Reporter: name,
				})
			})
		},
	}
	q.reporters[name] = sr
}