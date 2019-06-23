/*
Copyright 2019 The Kubernetes Authors.

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

package subnet

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/golang/glog"

	k8scorev1api "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	k8smetav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sfields "k8s.io/apimachinery/pkg/fields"
	k8stypes "k8s.io/apimachinery/pkg/types"
	k8sutilruntime "k8s.io/apimachinery/pkg/util/runtime"
	k8swait "k8s.io/apimachinery/pkg/util/wait"
	k8scache "k8s.io/client-go/tools/cache"
	k8sworkqueue "k8s.io/client-go/util/workqueue"

	netv1a1 "k8s.io/examples/staging/kos/pkg/apis/network/v1alpha1"
	kosclientv1a1 "k8s.io/examples/staging/kos/pkg/client/clientset/versioned/typed/network/v1alpha1"
	netlistv1a1 "k8s.io/examples/staging/kos/pkg/client/listers/network/v1alpha1"
	"k8s.io/examples/staging/kos/pkg/util/parse"
	"k8s.io/examples/staging/kos/pkg/util/parse/network/subnet"
)

// TODO: Add Prometheus metrics.

const subnetVNIField = "spec.vni"

// conflictsCache holds information for one subnet regarding conflicts with
// other subnets. There's no guarantee that the cache is up-to-date: a subnet Y
// stored in it might no longer be in conflict with the owning subnet, for
// instance because of an update to Y's CIDR following its addition to the
// cache.
type conflictsCache struct {
	// ownerSummary stores the data relevant to validation for the subnet owning
	// the conflicts cache.
	ownerSummary *subnet.Summary

	// rivals identifies the subnets that have a conflict with the subnet owning
	// the cache. If subnet X is the owner of the conflicts cache and a subnet Y
	// is in rivals this means that when Y was last processed by a queue worker
	// a conflict with X was found and at that time X had the VNI and CIDR
	// values in ownerSummary.
	rivals []k8stypes.NamespacedName
}

// Validator performs validation for newly-created or updated subnets, and
// writes in their status the outcome of the validation. Validation consists of
// two checks:
//
// 		(1) CIDRs for subnets with the same VNI are disjoint.
// 		(2) all subnets with the same VNI are within the same K8s namespace.
//
// If a subnet S1 does not pass validation because of a conflict with another
// subnet S2, upon deletion or modification of S2 S1 is validated again.
// Validator uses an informer on Subnets to be notified of creation or updates,
// but does a live list against the API server to retrieve the conflicting
// subnets when validating a subnet, to avoid race conditions caused by multiple
// validators running at the same time.
type Validator struct {
	netIfc         kosclientv1a1.NetworkV1alpha1Interface
	subnetInformer k8scache.SharedInformer
	subnetLister   netlistv1a1.SubnetLister
	queue          k8sworkqueue.RateLimitingInterface
	workers        int

	// conflicts associates a subnet namespaced name with its conflictsCache.
	// Always access while holding conflictsMutex.
	conflicts      map[k8stypes.NamespacedName]*conflictsCache
	conflictsMutex sync.Mutex

	// staleRVs associates a subnet X's namespaced name for which there was a
	// successful status update to X's resource version prior to the update.
	// When a worker begins processing a subnet X, it checks whether X's
	// resource version matches the resource version in staleRVs[X]. If that's
	// the case X is stale, i.e. it does not reflect the latest update yet,
	// hence processing is immediately stopped.
	// Only access while holding staleRVsMutex.
	staleRVs      map[k8stypes.NamespacedName]string
	staleRVsMutex sync.Mutex
}

func NewValidationController(netIfc kosclientv1a1.NetworkV1alpha1Interface,
	subnetInformer k8scache.SharedInformer,
	subnetLister netlistv1a1.SubnetLister,
	queue k8sworkqueue.RateLimitingInterface,
	workers int) *Validator {

	return &Validator{
		netIfc:         netIfc,
		subnetInformer: subnetInformer,
		subnetLister:   subnetLister,
		queue:          queue,
		workers:        workers,
		conflicts:      make(map[k8stypes.NamespacedName]*conflictsCache),
		staleRVs:       make(map[k8stypes.NamespacedName]string),
	}
}

// Run starts the validator and blocks until stop is closed. This entails
// starting its Informer and the worker goroutines.
func (v *Validator) Run(stop <-chan struct{}) error {
	defer k8sutilruntime.HandleCrash()
	defer v.queue.ShutDown()

	glog.Info("Starting subnet validation controller.")
	defer glog.Info("Shutting down subnet validation controller.")

	v.subnetInformer.AddEventHandler(v)

	if !k8scache.WaitForCacheSync(stop, v.subnetInformer.HasSynced) {
		return errors.New("informer cache failed to sync")
	}
	glog.V(2).Infof("Informer cache synced.")

	// Start workers.
	for i := 0; i < v.workers; i++ {
		go k8swait.Until(v.processQueue, time.Second, stop)
	}
	glog.V(2).Infof("Launched %d workers.", v.workers)

	<-stop

	return nil
}

func (v *Validator) OnAdd(obj interface{}) {
	s := obj.(*netv1a1.Subnet)
	glog.V(5).Infof("Notified of creation of %#+v.", s)
	v.queue.Add(k8stypes.NamespacedName{
		Namespace: s.Namespace,
		Name:      s.Name,
	})
}

func (v *Validator) OnUpdate(oldObj, newObj interface{}) {
	oldS, newS := oldObj.(*netv1a1.Subnet), newObj.(*netv1a1.Subnet)
	glog.V(5).Infof("Notified of update from %#+v to %#+v.", oldS, newS)

	// Process a subnet only if the fields that affect validation have changed.
	if oldS.Spec.IPv4 != newS.Spec.IPv4 || oldS.Spec.VNI != newS.Spec.VNI {
		v.queue.Add(k8stypes.NamespacedName{
			Namespace: newS.Namespace,
			Name:      newS.Name,
		})
	}
}

func (v *Validator) OnDelete(obj interface{}) {
	s := parse.Peel(obj).(*netv1a1.Subnet)
	glog.V(5).Infof("Notified of deletion of %#+v.", s)
	v.queue.Add(k8stypes.NamespacedName{
		Namespace: s.Namespace,
		Name:      s.Name,
	})
}

func (v *Validator) processQueue() {
	for {
		subnet, stop := v.queue.Get()
		if stop {
			return
		}
		v.processQueueItem(subnet.(k8stypes.NamespacedName))
	}
}

func (v *Validator) processQueueItem(subnet k8stypes.NamespacedName) {
	defer v.queue.Done(subnet)
	requeues := v.queue.NumRequeues(subnet)
	if err := v.processSubnet(subnet); err != nil {
		glog.Warningf("Failed processing %s, requeuing (%d earlier requeues): %s.", subnet, requeues, err.Error())
		v.queue.AddRateLimited(subnet)
		return
	}
	glog.V(4).Infof("Finished %s with %d requeues.", subnet, requeues)
	v.queue.Forget(subnet)
}

func (v *Validator) processSubnet(subnetNSN k8stypes.NamespacedName) error {
	subnet, err := v.subnetLister.Subnets(subnetNSN.Namespace).Get(subnetNSN.Name)

	if err != nil && !k8serrors.IsNotFound(err) {
		glog.Errorf("subnet lister failed to lookup %s: %s", subnetNSN, err.Error())
		// This should never happen. No point in retrying.
		return nil
	}

	if k8serrors.IsNotFound(err) {
		v.processDeletedSubnet(subnetNSN)
		return nil
	}

	return v.processExistingSubnet(subnet)
}

func (v *Validator) processDeletedSubnet(s k8stypes.NamespacedName) {
	v.clearStaleRV(s)

	rivals := v.clearConflictsCache(s)

	// Enqueue old rivals so that they can be re-validated: they might no longer
	// have conflicts as this subnet has been deleted.
	for _, r := range rivals {
		v.queue.Add(r)
	}
}

func (v *Validator) processExistingSubnet(s *netv1a1.Subnet) error {
	ss, parsingErrs := subnet.NewSummary(s)
	if len(parsingErrs) > 0 {
		return parsingErrs
	}

	if v.subnetIsStale(ss.NamespacedName, s.ResourceVersion) {
		return nil
	}

	// If we're here s might have been created or updated in a way that affects
	// validation. We need to update its conflicts cache accordingly and
	// reconsider old rivals because they might no longer be in conflict with
	// s.
	oldRivals := v.updateConflictsCache(ss)
	for _, r := range oldRivals {
		v.queue.Add(r)
	}

	// Keep the promise that a Subnet stays validated once it becomes validated
	// (unless and until its VNI or CIDR block changes).
	if s.Status.Validated {
		return nil
	}

	// Retrieve all the other subnets with the same VNI. Doing a live list as
	// opposed to a cache-based one (through the informer) prevents race
	// conditions that can arise in case of multiple validators running.
	potentialRivals, err := v.netIfc.Subnets(k8scorev1api.NamespaceAll).List(k8smetav1.ListOptions{
		FieldSelector: k8sfields.OneTermEqualSelector(subnetVNIField, fmt.Sprint(ss.VNI)).String(),
	})
	if err != nil {
		if malformedRequest(err) {
			glog.Errorf("live list of all subnets against API server failed while validating %s: %s. There will be no retry because of the nature of the error", ss.NamespacedName, err.Error())
			// This should never happen, no point in retrying.
			return nil
		}
		return fmt.Errorf("live list of all subnets against API server failed: %s", err.Error())
	}

	// Look for conflicts with all the other subnets with the same VNI and
	// record them in the rivals conflicts caches.
	conflictsMsgs, conflictFound, err := v.recordConflicts(ss, potentialRivals.Items)
	if err != nil {
		return err
	}

	if err := v.updateSubnetValidity(s, !conflictFound, conflictsMsgs); err != nil {
		return fmt.Errorf("failed to write validation outcome into %s's status: %s", ss.NamespacedName, err.Error())
	}

	return nil
}

func (v *Validator) clearStaleRV(s k8stypes.NamespacedName) {
	v.staleRVsMutex.Lock()
	defer v.staleRVsMutex.Unlock()

	delete(v.staleRVs, s)
}

func (v *Validator) clearConflictsCache(s k8stypes.NamespacedName) []k8stypes.NamespacedName {
	v.conflictsMutex.Lock()
	defer v.conflictsMutex.Unlock()

	if c := v.conflicts[s]; c != nil {
		delete(v.conflicts, s)
		return c.rivals
	}

	return nil
}

func (v *Validator) subnetIsStale(s k8stypes.NamespacedName, rv string) bool {
	v.staleRVsMutex.Lock()
	defer v.staleRVsMutex.Unlock()

	if v.staleRVs[s] == rv {
		return true
	}

	delete(v.staleRVs, s)
	return false
}

func (v *Validator) updateConflictsCache(s *subnet.Summary) []k8stypes.NamespacedName {
	v.conflictsMutex.Lock()
	defer v.conflictsMutex.Unlock()

	c := v.conflicts[s.NamespacedName]
	if c == nil {
		c = &conflictsCache{
			ownerSummary: s,
			rivals:       make([]k8stypes.NamespacedName, 0),
		}
		v.conflicts[s.NamespacedName] = c
		return nil
	}

	var oldRivals []k8stypes.NamespacedName
	if s.VNI != c.ownerSummary.VNI || !s.Contains(c.ownerSummary) {
		// The data that affects validation changed, return all rivals so that
		// they can be re-validated again as the conflict might have disappeared.
		oldRivals = c.rivals
		c.rivals = make([]k8stypes.NamespacedName, 0)
	}
	c.ownerSummary = s

	return oldRivals
}

func malformedRequest(e error) bool {
	return k8serrors.IsUnauthorized(e) ||
		k8serrors.IsBadRequest(e) ||
		k8serrors.IsForbidden(e) ||
		k8serrors.IsNotAcceptable(e) ||
		k8serrors.IsUnsupportedMediaType(e) ||
		k8serrors.IsMethodNotSupported(e) ||
		k8serrors.IsInvalid(e)
}

func (v *Validator) recordConflicts(candidate *subnet.Summary, potentialRivals []netv1a1.Subnet) (conflictsMsgs []string, conflictFound bool, err error) {
	for _, pr := range potentialRivals {
		potentialRival, parsingErrs := subnet.NewSummary(&pr)
		if len(parsingErrs) > 0 {
			glog.Errorf("parsing %s failed while validating %s: %s", potentialRival.NamespacedName, candidate.NamespacedName, parsingErrs.Error())
		}

		if !potentialRival.Conflict(candidate) || potentialRival.SameSubnetAs(candidate) {
			// potentialRival is not a rival to candidate or it is the same
			// subnet as candidate, hence we skip it.
			continue
		}

		// If we're here the two subnets represented by potentialRival and
		// candidate are in conflict, that is, they are rivals.
		conflictFound = true
		if potentialRival.CIDRConflict(candidate) {
			glog.V(2).Infof("CIDR conflict found between %s (%d, %d) and %s (%d, %d).", candidate.NamespacedName, candidate.BaseU, candidate.LastU, potentialRival.NamespacedName, potentialRival.BaseU, potentialRival.LastU)
			conflictsMsgs = append(conflictsMsgs, fmt.Sprintf("CIDR overlaps with %s's (%s)", potentialRival.NamespacedName, pr.Spec.IPv4))
		}
		if potentialRival.NSConflict(candidate) {
			glog.V(2).Infof("Namespace conflict found between %s and %s.", candidate.NamespacedName, potentialRival.NamespacedName)
			conflictsMsgs = append(conflictsMsgs, fmt.Sprintf("same VNI but different namespace wrt %s", potentialRival.NamespacedName))
		}

		// Record the conflict in the conflicts cache.
		if err = v.recordConflict(potentialRival, candidate); err != nil {
			return
		}
	}

	return
}

func (v *Validator) updateSubnetValidity(s *netv1a1.Subnet, validated bool, validationErrors []string) error {
	sCopy := s.DeepCopy()

	sCopy.Status.Validated = validated
	sCopy.Status.Errors.Validation = validationErrors

	_, err := v.netIfc.Subnets(sCopy.Namespace).Update(sCopy)
	switch {
	case err == nil:
		nsn := k8stypes.NamespacedName{
			Namespace: s.Namespace,
			Name:      s.Name,
		}
		v.updateStaleRV(nsn, s.ResourceVersion)
	case malformedRequest(err):
		glog.Errorf("failed to update subnet from %#+v to %#+v: %s. There will be no retry because of the nature of the error", s, sCopy, err.Error())
	default:
		return fmt.Errorf("failed to update subnet from %#+v to %#+v: %s", s, sCopy, err.Error())
	}

	return nil
}

func (v *Validator) recordConflict(enroller, enrollee *subnet.Summary) error {
	v.conflictsMutex.Lock()
	defer v.conflictsMutex.Unlock()

	c := v.conflicts[enroller.NamespacedName]

	if c == nil {
		return fmt.Errorf("registration of %s as a rival of %s failed: %s's conflicts cache not found", enrollee.NamespacedName, enroller.NamespacedName, enroller.NamespacedName)
	}

	if !enroller.Equal(c.ownerSummary) {
		// If we're here the version of the enroller recorded in its conflicts
		// cache does not match the version of the enroller we got with the live
		// list to the API server: one of the two is stale. Return an error so
		// that the caller can wait a little bit and retry (hopefully the
		// version skew has resolved by then).
		return fmt.Errorf("registration of %s as a rival of %s failed: mismatch between live data and conflicts cache data", enrollee.NamespacedName, enroller.NamespacedName)
	}

	c.rivals = append(c.rivals, enrollee.NamespacedName)
	return nil
}

func (v *Validator) updateStaleRV(s k8stypes.NamespacedName, rv string) {
	v.staleRVsMutex.Lock()
	defer v.staleRVsMutex.Unlock()

	v.staleRVs[s] = rv
}
