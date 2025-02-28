// Copyright 2018 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package controller

import (
	"context"
	"fmt"
	"strings"

	apps "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	appsinformers "k8s.io/client-go/informers/apps/v1"
	"k8s.io/client-go/kubernetes"
	appslisters "k8s.io/client-go/listers/apps/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog"
)

// StatefulSetControlInterface defines the interface that uses to create, update, and delete StatefulSets,
type StatefulSetControlInterface interface {
	CreateStatefulSet(runtime.Object, *apps.StatefulSet) error
	UpdateStatefulSet(runtime.Object, *apps.StatefulSet) (*apps.StatefulSet, error)
	DeleteStatefulSet(runtime.Object, *apps.StatefulSet) error
}

type realStatefulSetControl struct {
	kubeCli   kubernetes.Interface
	setLister appslisters.StatefulSetLister
	recorder  record.EventRecorder
}

// NewRealStatefuSetControl returns a StatefulSetControlInterface
func NewRealStatefuSetControl(kubeCli kubernetes.Interface, setLister appslisters.StatefulSetLister, recorder record.EventRecorder) StatefulSetControlInterface {
	return &realStatefulSetControl{kubeCli, setLister, recorder}
}

// CreateStatefulSet create a StatefulSet for a controller
func (c *realStatefulSetControl) CreateStatefulSet(controller runtime.Object, set *apps.StatefulSet) error {
	controllerMo, ok := controller.(metav1.Object)
	if !ok {
		return fmt.Errorf("%T is not a metav1.Object, cannot call setControllerReference", controller)
	}
	kind := controller.GetObjectKind().GroupVersionKind().Kind
	name := controllerMo.GetName()
	namespace := controllerMo.GetNamespace()

	_, err := c.kubeCli.AppsV1().StatefulSets(namespace).Create(context.TODO(), set, metav1.CreateOptions{})
	// sink already exists errors
	if apierrors.IsAlreadyExists(err) {
		return err
	}
	c.recordStatefulSetEvent("create", kind, name, controller, set, err)
	return err
}

// UpdateStatefulSet update a StatefulSet in a TidbCluster.
func (c *realStatefulSetControl) UpdateStatefulSet(controller runtime.Object, set *apps.StatefulSet) (*apps.StatefulSet, error) {
	controllerMo, ok := controller.(metav1.Object)
	if !ok {
		return nil, fmt.Errorf("%T is not a metav1.Object, cannot call setControllerReference", controller)
	}
	kind := controller.GetObjectKind().GroupVersionKind().Kind
	name := controllerMo.GetName()
	namespace := controllerMo.GetNamespace()

	setName := set.GetName()
	setSpec := set.Spec.DeepCopy()
	var updatedSS *apps.StatefulSet

	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		// TODO: verify if StatefulSet identity(name, namespace, labels) matches TidbCluster
		var updateErr error
		updatedSS, updateErr = c.kubeCli.AppsV1().StatefulSets(namespace).Update(context.TODO(), set, metav1.UpdateOptions{})
		if updateErr == nil {
			klog.Infof("%s: [%s/%s]'s StatefulSet: [%s/%s] updated successfully", kind, namespace, name, namespace, setName)
			return nil
		}
		klog.Errorf("failed to update %s: [%s/%s]'s StatefulSet: [%s/%s], error: %v", kind, namespace, name, namespace, setName, updateErr)

		if updated, err := c.setLister.StatefulSets(namespace).Get(setName); err == nil {
			// make a copy so we don't mutate the shared cache
			set = updated.DeepCopy()
			set.Spec = *setSpec
		} else {
			utilruntime.HandleError(fmt.Errorf("error getting updated StatefulSet %s/%s from lister: %v", namespace, setName, err))
		}
		return updateErr
	})

	return updatedSS, err
}

// DeleteStatefulSet delete a StatefulSet in a TidbCluster.
func (c *realStatefulSetControl) DeleteStatefulSet(controller runtime.Object, set *apps.StatefulSet) error {
	controllerMo, ok := controller.(metav1.Object)
	if !ok {
		return fmt.Errorf("%T is not a metav1.Object, cannot call setControllerReference", controller)
	}
	kind := controller.GetObjectKind().GroupVersionKind().Kind
	name := controllerMo.GetName()
	namespace := controllerMo.GetNamespace()

	err := c.kubeCli.AppsV1().StatefulSets(namespace).Delete(context.TODO(), set.Name, metav1.DeleteOptions{})
	c.recordStatefulSetEvent("delete", kind, name, controller, set, err)
	return err
}

func (c *realStatefulSetControl) recordStatefulSetEvent(verb, kind, name string, object runtime.Object, set *apps.StatefulSet, err error) {
	setName := set.Name
	if err == nil {
		reason := fmt.Sprintf("Successful%s", strings.Title(verb))
		message := fmt.Sprintf("%s StatefulSet %s in %s %s successful",
			strings.ToLower(verb), setName, kind, name)
		c.recorder.Event(object, corev1.EventTypeNormal, reason, message)
	} else {
		reason := fmt.Sprintf("Failed%s", strings.Title(verb))
		message := fmt.Sprintf("%s StatefulSet %s in %s %s failed error: %s",
			strings.ToLower(verb), setName, kind, name, err)
		c.recorder.Event(object, corev1.EventTypeWarning, reason, message)
	}
}

var _ StatefulSetControlInterface = &realStatefulSetControl{}

// FakeStatefulSetControl is a fake StatefulSetControlInterface
type FakeStatefulSetControl struct {
	SetLister                appslisters.StatefulSetLister
	SetIndexer               cache.Indexer
	createStatefulSetTracker RequestTracker
	updateStatefulSetTracker RequestTracker
	deleteStatefulSetTracker RequestTracker
	statusChange             func(set *apps.StatefulSet)
}

// NewFakeStatefulSetControl returns a FakeStatefulSetControl
func NewFakeStatefulSetControl(setInformer appsinformers.StatefulSetInformer) *FakeStatefulSetControl {
	return &FakeStatefulSetControl{
		setInformer.Lister(),
		setInformer.Informer().GetIndexer(),
		RequestTracker{},
		RequestTracker{},
		RequestTracker{},
		nil,
	}
}

// SetCreateStatefulSetError sets the error attributes of createStatefulSetTracker
func (c *FakeStatefulSetControl) SetCreateStatefulSetError(err error, after int) {
	c.createStatefulSetTracker.SetError(err).SetAfter(after)
}

// SetUpdateStatefulSetError sets the error attributes of updateStatefulSetTracker
func (c *FakeStatefulSetControl) SetUpdateStatefulSetError(err error, after int) {
	c.updateStatefulSetTracker.SetError(err).SetAfter(after)
}

// SetDeleteStatefulSetError sets the error attributes of deleteStatefulSetTracker
func (c *FakeStatefulSetControl) SetDeleteStatefulSetError(err error, after int) {
	c.deleteStatefulSetTracker.SetError(err).SetAfter(after)
}

func (c *FakeStatefulSetControl) SetStatusChange(fn func(*apps.StatefulSet)) {
	c.statusChange = fn
}

// CreateStatefulSet adds the statefulset to SetIndexer
func (c *FakeStatefulSetControl) CreateStatefulSet(controller runtime.Object, set *apps.StatefulSet) error {
	defer func() {
		c.createStatefulSetTracker.Inc()
		c.statusChange = nil
	}()
	controllerMo, ok := controller.(metav1.Object)
	if !ok {
		return fmt.Errorf("%T is not a metav1.Object, cannot call CreateStatefulSet", controller)
	}
	namespace := controllerMo.GetNamespace()
	if namespace != set.Namespace {
		return fmt.Errorf("%T is not match sts namespace:%s", controller, set.Namespace)
	}
	if c.createStatefulSetTracker.ErrorReady() {
		defer c.createStatefulSetTracker.Reset()
		return c.createStatefulSetTracker.GetError()
	}

	if c.statusChange != nil {
		c.statusChange(set)
	}

	return c.SetIndexer.Add(set)
}

// UpdateStatefulSet updates the statefulset of SetIndexer
func (c *FakeStatefulSetControl) UpdateStatefulSet(_ runtime.Object, set *apps.StatefulSet) (*apps.StatefulSet, error) {
	defer func() {
		c.updateStatefulSetTracker.Inc()
		c.statusChange = nil
	}()

	if c.updateStatefulSetTracker.ErrorReady() {
		defer c.updateStatefulSetTracker.Reset()
		return nil, c.updateStatefulSetTracker.GetError()
	}

	if c.statusChange != nil {
		c.statusChange(set)
	}
	return set, c.SetIndexer.Update(set)
}

// DeleteStatefulSet deletes the statefulset of SetIndexer
func (c *FakeStatefulSetControl) DeleteStatefulSet(_ runtime.Object, _ *apps.StatefulSet) error {
	return nil
}

var _ StatefulSetControlInterface = &FakeStatefulSetControl{}
