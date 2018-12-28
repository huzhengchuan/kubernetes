/*
Copyright 2015 The Kubernetes Authors.

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

package cache

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"k8s.io/api/core/v1"
	"k8s.io/api/policy/v1beta1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/diff"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/wait"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/kubernetes/pkg/features"
	"k8s.io/kubernetes/pkg/scheduler/api"
	priorityutil "k8s.io/kubernetes/pkg/scheduler/algorithm/priorities/util"
	schedutil "k8s.io/kubernetes/pkg/scheduler/util"
)

func deepEqualWithoutGeneration(t *testing.T, testcase int, actual, expected *NodeInfo) {
	// Ignore generation field.
	if actual != nil {
		actual.generation = 0
	}
	if !reflect.DeepEqual(actual, expected) {
		t.Errorf("#%d: node info get=%s, want=%s", testcase, actual, expected)
	}
}

type hostPortInfoParam struct {
	protocol, ip string
	port         int32
}

type hostPortInfoBuilder struct {
	inputs []hostPortInfoParam
}

func newHostPortInfoBuilder() *hostPortInfoBuilder {
	return &hostPortInfoBuilder{}
}

func (b *hostPortInfoBuilder) add(protocol, ip string, port int32) *hostPortInfoBuilder {
	b.inputs = append(b.inputs, hostPortInfoParam{protocol, ip, port})
	return b
}

func (b *hostPortInfoBuilder) build() schedutil.HostPortInfo {
	res := make(schedutil.HostPortInfo)
	for _, param := range b.inputs {
		res.Add(param.ip, param.protocol, param.port)
	}
	return res
}

// TestAssumePodScheduled tests that after a pod is assumed, its information is aggregated
// on node level.
func TestAssumePodScheduled(t *testing.T) {
	// Enable volumesOnNodeForBalancing to do balanced resource allocation
	utilfeature.DefaultFeatureGate.Set(fmt.Sprintf("%s=true", features.BalanceAttachedNodeVolumes))
	nodeName := "node"
	testPods := []*v1.Pod{
		makeBasePod(t, nodeName, "test", "100m", "500", "", []v1.ContainerPort{{HostIP: "127.0.0.1", HostPort: 80, Protocol: "TCP"}}),
		makeBasePod(t, nodeName, "test-1", "100m", "500", "", []v1.ContainerPort{{HostIP: "127.0.0.1", HostPort: 80, Protocol: "TCP"}}),
		makeBasePod(t, nodeName, "test-2", "200m", "1Ki", "", []v1.ContainerPort{{HostIP: "127.0.0.1", HostPort: 8080, Protocol: "TCP"}}),
		makeBasePod(t, nodeName, "test-nonzero", "", "", "", []v1.ContainerPort{{HostIP: "127.0.0.1", HostPort: 80, Protocol: "TCP"}}),
		makeBasePod(t, nodeName, "test", "100m", "500", "example.com/foo:3", []v1.ContainerPort{{HostIP: "127.0.0.1", HostPort: 80, Protocol: "TCP"}}),
		makeBasePod(t, nodeName, "test-2", "200m", "1Ki", "example.com/foo:5", []v1.ContainerPort{{HostIP: "127.0.0.1", HostPort: 8080, Protocol: "TCP"}}),
		makeBasePod(t, nodeName, "test", "100m", "500", "random-invalid-extended-key:100", []v1.ContainerPort{{}}),
	}

	tests := []struct {
		pods []*v1.Pod

		wNodeInfo *NodeInfo
	}{{
		pods: []*v1.Pod{testPods[0]},
		wNodeInfo: &NodeInfo{
			requestedResource: &Resource{
				MilliCPU: 100,
				Memory:   500,
			},
			nonzeroRequest: &Resource{
				MilliCPU: 100,
				Memory:   500,
			},
			TransientInfo:       newTransientSchedulerInfo(),
			allocatableResource: &Resource{},
			pods:                []*v1.Pod{testPods[0]},
			usedPorts:           newHostPortInfoBuilder().add("TCP", "127.0.0.1", 80).build(),
			imageSizes:          map[string]int64{},
		},
	}, {
		pods: []*v1.Pod{testPods[1], testPods[2]},
		wNodeInfo: &NodeInfo{
			requestedResource: &Resource{
				MilliCPU: 300,
				Memory:   1524,
			},
			nonzeroRequest: &Resource{
				MilliCPU: 300,
				Memory:   1524,
			},
			TransientInfo:       newTransientSchedulerInfo(),
			allocatableResource: &Resource{},
			pods:                []*v1.Pod{testPods[1], testPods[2]},
			usedPorts:           newHostPortInfoBuilder().add("TCP", "127.0.0.1", 80).add("TCP", "127.0.0.1", 8080).build(),
			imageSizes:          map[string]int64{},
		},
	}, { // test non-zero request
		pods: []*v1.Pod{testPods[3]},
		wNodeInfo: &NodeInfo{
			requestedResource: &Resource{
				MilliCPU: 0,
				Memory:   0,
			},
			nonzeroRequest: &Resource{
				MilliCPU: priorityutil.DefaultMilliCPURequest,
				Memory:   priorityutil.DefaultMemoryRequest,
			},
			TransientInfo:       newTransientSchedulerInfo(),
			allocatableResource: &Resource{},
			pods:                []*v1.Pod{testPods[3]},
			usedPorts:           newHostPortInfoBuilder().add("TCP", "127.0.0.1", 80).build(),
			imageSizes:          map[string]int64{},
		},
	}, {
		pods: []*v1.Pod{testPods[4]},
		wNodeInfo: &NodeInfo{
			requestedResource: &Resource{
				MilliCPU:        100,
				Memory:          500,
				ScalarResources: map[v1.ResourceName]int64{"example.com/foo": 3},
			},
			nonzeroRequest: &Resource{
				MilliCPU: 100,
				Memory:   500,
			},
			TransientInfo:       newTransientSchedulerInfo(),
			allocatableResource: &Resource{},
			pods:                []*v1.Pod{testPods[4]},
			usedPorts:           newHostPortInfoBuilder().add("TCP", "127.0.0.1", 80).build(),
			imageSizes:          map[string]int64{},
		},
	}, {
		pods: []*v1.Pod{testPods[4], testPods[5]},
		wNodeInfo: &NodeInfo{
			requestedResource: &Resource{
				MilliCPU:        300,
				Memory:          1524,
				ScalarResources: map[v1.ResourceName]int64{"example.com/foo": 8},
			},
			nonzeroRequest: &Resource{
				MilliCPU: 300,
				Memory:   1524,
			},
			TransientInfo:       newTransientSchedulerInfo(),
			allocatableResource: &Resource{},
			pods:                []*v1.Pod{testPods[4], testPods[5]},
			usedPorts:           newHostPortInfoBuilder().add("TCP", "127.0.0.1", 80).add("TCP", "127.0.0.1", 8080).build(),
			imageSizes:          map[string]int64{},
		},
	}, {
		pods: []*v1.Pod{testPods[6]},
		wNodeInfo: &NodeInfo{
			requestedResource: &Resource{
				MilliCPU: 100,
				Memory:   500,
			},
			nonzeroRequest: &Resource{
				MilliCPU: 100,
				Memory:   500,
			},
			TransientInfo:       newTransientSchedulerInfo(),
			allocatableResource: &Resource{},
			pods:                []*v1.Pod{testPods[6]},
			usedPorts:           newHostPortInfoBuilder().build(),
			imageSizes:          map[string]int64{},
		},
	},
	}

	for i, tt := range tests {
		cache := newSchedulerCache(time.Second, time.Second, nil)
		for _, pod := range tt.pods {
			if err := cache.AssumePod(pod); err != nil {
				t.Fatalf("AssumePod failed: %v", err)
			}
		}
		n := cache.nodes[nodeName]
		deepEqualWithoutGeneration(t, i, n, tt.wNodeInfo)

		for _, pod := range tt.pods {
			if err := cache.ForgetPod(pod); err != nil {
				t.Fatalf("ForgetPod failed: %v", err)
			}
		}
		if cache.nodes[nodeName] != nil {
			t.Errorf("NodeInfo should be cleaned for %s", nodeName)
		}
	}
}

type testExpirePodStruct struct {
	pod         *v1.Pod
	assumedTime time.Time
}

func assumeAndFinishBinding(cache *schedulerCache, pod *v1.Pod, assumedTime time.Time) error {
	if err := cache.AssumePod(pod); err != nil {
		return err
	}
	return cache.finishBinding(pod, assumedTime)
}

// TestExpirePod tests that assumed pods will be removed if expired.
// The removal will be reflected in node info.
func TestExpirePod(t *testing.T) {
	// Enable volumesOnNodeForBalancing to do balanced resource allocation
	utilfeature.DefaultFeatureGate.Set(fmt.Sprintf("%s=true", features.BalanceAttachedNodeVolumes))
	nodeName := "node"
	testPods := []*v1.Pod{
		makeBasePod(t, nodeName, "test-1", "100m", "500", "", []v1.ContainerPort{{HostIP: "127.0.0.1", HostPort: 80, Protocol: "TCP"}}),
		makeBasePod(t, nodeName, "test-2", "200m", "1Ki", "", []v1.ContainerPort{{HostIP: "127.0.0.1", HostPort: 8080, Protocol: "TCP"}}),
	}
	now := time.Now()
	ttl := 10 * time.Second
	tests := []struct {
		pods        []*testExpirePodStruct
		cleanupTime time.Time

		wNodeInfo *NodeInfo
	}{{ // assumed pod would expires
		pods: []*testExpirePodStruct{
			{pod: testPods[0], assumedTime: now},
		},
		cleanupTime: now.Add(2 * ttl),
		wNodeInfo:   nil,
	}, { // first one would expire, second one would not.
		pods: []*testExpirePodStruct{
			{pod: testPods[0], assumedTime: now},
			{pod: testPods[1], assumedTime: now.Add(3 * ttl / 2)},
		},
		cleanupTime: now.Add(2 * ttl),
		wNodeInfo: &NodeInfo{
			requestedResource: &Resource{
				MilliCPU: 200,
				Memory:   1024,
			},
			nonzeroRequest: &Resource{
				MilliCPU: 200,
				Memory:   1024,
			},
			TransientInfo:       newTransientSchedulerInfo(),
			allocatableResource: &Resource{},
			pods:                []*v1.Pod{testPods[1]},
			usedPorts:           newHostPortInfoBuilder().add("TCP", "127.0.0.1", 8080).build(),
			imageSizes:          map[string]int64{},
		},
	}}

	for i, tt := range tests {
		cache := newSchedulerCache(ttl, time.Second, nil)

		for _, pod := range tt.pods {
			if err := assumeAndFinishBinding(cache, pod.pod, pod.assumedTime); err != nil {
				t.Fatalf("assumePod failed: %v", err)
			}
		}
		// pods that have assumedTime + ttl < cleanupTime will get expired and removed
		cache.cleanupAssumedPods(tt.cleanupTime)
		n := cache.nodes[nodeName]
		deepEqualWithoutGeneration(t, i, n, tt.wNodeInfo)
	}
}

// TestAddPodWillConfirm tests that a pod being Add()ed will be confirmed if assumed.
// The pod info should still exist after manually expiring unconfirmed pods.
func TestAddPodWillConfirm(t *testing.T) {
	// Enable volumesOnNodeForBalancing to do balanced resource allocation
	utilfeature.DefaultFeatureGate.Set(fmt.Sprintf("%s=true", features.BalanceAttachedNodeVolumes))
	nodeName := "node"
	now := time.Now()
	ttl := 10 * time.Second

	testPods := []*v1.Pod{
		makeBasePod(t, nodeName, "test-1", "100m", "500", "", []v1.ContainerPort{{HostIP: "127.0.0.1", HostPort: 80, Protocol: "TCP"}}),
		makeBasePod(t, nodeName, "test-2", "200m", "1Ki", "", []v1.ContainerPort{{HostIP: "127.0.0.1", HostPort: 8080, Protocol: "TCP"}}),
	}
	tests := []struct {
		podsToAssume []*v1.Pod
		podsToAdd    []*v1.Pod

		wNodeInfo *NodeInfo
	}{{ // two pod were assumed at same time. But first one is called Add() and gets confirmed.
		podsToAssume: []*v1.Pod{testPods[0], testPods[1]},
		podsToAdd:    []*v1.Pod{testPods[0]},
		wNodeInfo: &NodeInfo{
			requestedResource: &Resource{
				MilliCPU: 100,
				Memory:   500,
			},
			nonzeroRequest: &Resource{
				MilliCPU: 100,
				Memory:   500,
			},
			TransientInfo:       newTransientSchedulerInfo(),
			allocatableResource: &Resource{},
			pods:                []*v1.Pod{testPods[0]},
			usedPorts:           newHostPortInfoBuilder().add("TCP", "127.0.0.1", 80).build(),
			imageSizes:          map[string]int64{},
		},
	}}

	for i, tt := range tests {
		cache := newSchedulerCache(ttl, time.Second, nil)
		for _, podToAssume := range tt.podsToAssume {
			if err := assumeAndFinishBinding(cache, podToAssume, now); err != nil {
				t.Fatalf("assumePod failed: %v", err)
			}
		}
		for _, podToAdd := range tt.podsToAdd {
			if err := cache.AddPod(podToAdd); err != nil {
				t.Fatalf("AddPod failed: %v", err)
			}
		}
		cache.cleanupAssumedPods(now.Add(2 * ttl))
		// check after expiration. confirmed pods shouldn't be expired.
		n := cache.nodes[nodeName]
		deepEqualWithoutGeneration(t, i, n, tt.wNodeInfo)
	}
}

func TestSnapshot(t *testing.T) {
	nodeName := "node"
	now := time.Now()
	ttl := 10 * time.Second

	testPods := []*v1.Pod{
		makeBasePod(t, nodeName, "test-1", "100m", "500", "", []v1.ContainerPort{{HostIP: "127.0.0.1", HostPort: 80, Protocol: "TCP"}}),
		makeBasePod(t, nodeName, "test-2", "200m", "1Ki", "", []v1.ContainerPort{{HostIP: "127.0.0.1", HostPort: 80, Protocol: "TCP"}}),
	}
	tests := []struct {
		podsToAssume []*v1.Pod
		podsToAdd    []*v1.Pod
	}{{ // two pod were assumed at same time. But first one is called Add() and gets confirmed.
		podsToAssume: []*v1.Pod{testPods[0], testPods[1]},
		podsToAdd:    []*v1.Pod{testPods[0]},
	}}

	for _, tt := range tests {
		cache := newSchedulerCache(ttl, time.Second, nil)
		for _, podToAssume := range tt.podsToAssume {
			if err := assumeAndFinishBinding(cache, podToAssume, now); err != nil {
				t.Fatalf("assumePod failed: %v", err)
			}
		}
		for _, podToAdd := range tt.podsToAdd {
			if err := cache.AddPod(podToAdd); err != nil {
				t.Fatalf("AddPod failed: %v", err)
			}
		}

		snapshot := cache.Snapshot()
		if !reflect.DeepEqual(snapshot.Nodes, cache.nodes) {
			t.Fatalf("expect \n%+v; got \n%+v", cache.nodes, snapshot.Nodes)
		}
		if !reflect.DeepEqual(snapshot.AssumedPods, cache.assumedPods) {
			t.Fatalf("expect \n%+v; got \n%+v", cache.assumedPods, snapshot.AssumedPods)
		}
	}
}

// TestAddPodWillReplaceAssumed tests that a pod being Add()ed will replace any assumed pod.
func TestAddPodWillReplaceAssumed(t *testing.T) {
	now := time.Now()
	ttl := 10 * time.Second

	assumedPod := makeBasePod(t, "assumed-node-1", "test-1", "100m", "500", "", []v1.ContainerPort{{HostPort: 80}})
	addedPod := makeBasePod(t, "actual-node", "test-1", "100m", "500", "", []v1.ContainerPort{{HostPort: 80}})
	updatedPod := makeBasePod(t, "actual-node", "test-1", "200m", "500", "", []v1.ContainerPort{{HostPort: 90}})

	tests := []struct {
		podsToAssume []*v1.Pod
		podsToAdd    []*v1.Pod
		podsToUpdate [][]*v1.Pod

		wNodeInfo map[string]*NodeInfo
	}{{
		podsToAssume: []*v1.Pod{assumedPod.DeepCopy()},
		podsToAdd:    []*v1.Pod{addedPod.DeepCopy()},
		podsToUpdate: [][]*v1.Pod{{addedPod.DeepCopy(), updatedPod.DeepCopy()}},
		wNodeInfo: map[string]*NodeInfo{
			"assumed-node": nil,
			"actual-node": {
				requestedResource: &Resource{
					MilliCPU: 200,
					Memory:   500,
				},
				nonzeroRequest: &Resource{
					MilliCPU: 200,
					Memory:   500,
				},
				TransientInfo:       newTransientSchedulerInfo(),
				allocatableResource: &Resource{},
				pods:                []*v1.Pod{updatedPod.DeepCopy()},
				usedPorts:           newHostPortInfoBuilder().add("TCP", "0.0.0.0", 90).build(),
				imageSizes:          map[string]int64{},
			},
		},
	}}

	for i, tt := range tests {
		cache := newSchedulerCache(ttl, time.Second, nil)
		for _, podToAssume := range tt.podsToAssume {
			if err := assumeAndFinishBinding(cache, podToAssume, now); err != nil {
				t.Fatalf("assumePod failed: %v", err)
			}
		}
		for _, podToAdd := range tt.podsToAdd {
			if err := cache.AddPod(podToAdd); err != nil {
				t.Fatalf("AddPod failed: %v", err)
			}
		}
		for _, podToUpdate := range tt.podsToUpdate {
			if err := cache.UpdatePod(podToUpdate[0], podToUpdate[1]); err != nil {
				t.Fatalf("UpdatePod failed: %v", err)
			}
		}
		for nodeName, expected := range tt.wNodeInfo {
			t.Log(nodeName)
			n := cache.nodes[nodeName]
			deepEqualWithoutGeneration(t, i, n, expected)
		}
	}
}

// TestAddPodAfterExpiration tests that a pod being Add()ed will be added back if expired.
func TestAddPodAfterExpiration(t *testing.T) {
	// Enable volumesOnNodeForBalancing to do balanced resource allocation
	utilfeature.DefaultFeatureGate.Set(fmt.Sprintf("%s=true", features.BalanceAttachedNodeVolumes))
	nodeName := "node"
	ttl := 10 * time.Second
	basePod := makeBasePod(t, nodeName, "test", "100m", "500", "", []v1.ContainerPort{{HostIP: "127.0.0.1", HostPort: 80, Protocol: "TCP"}})
	tests := []struct {
		pod *v1.Pod

		wNodeInfo *NodeInfo
	}{{
		pod: basePod,
		wNodeInfo: &NodeInfo{
			requestedResource: &Resource{
				MilliCPU: 100,
				Memory:   500,
			},
			nonzeroRequest: &Resource{
				MilliCPU: 100,
				Memory:   500,
			},
			TransientInfo:       newTransientSchedulerInfo(),
			allocatableResource: &Resource{},
			pods:                []*v1.Pod{basePod},
			usedPorts:           newHostPortInfoBuilder().add("TCP", "127.0.0.1", 80).build(),
			imageSizes:          map[string]int64{},
		},
	}}

	now := time.Now()
	for i, tt := range tests {
		cache := newSchedulerCache(ttl, time.Second, nil)
		if err := assumeAndFinishBinding(cache, tt.pod, now); err != nil {
			t.Fatalf("assumePod failed: %v", err)
		}
		cache.cleanupAssumedPods(now.Add(2 * ttl))
		// It should be expired and removed.
		n := cache.nodes[nodeName]
		if n != nil {
			t.Errorf("#%d: expecting nil node info, but get=%v", i, n)
		}
		if err := cache.AddPod(tt.pod); err != nil {
			t.Fatalf("AddPod failed: %v", err)
		}
		// check after expiration. confirmed pods shouldn't be expired.
		n = cache.nodes[nodeName]
		deepEqualWithoutGeneration(t, i, n, tt.wNodeInfo)
	}
}

// TestUpdatePod tests that a pod will be updated if added before.
func TestUpdatePod(t *testing.T) {
	// Enable volumesOnNodeForBalancing to do balanced resource allocation
	utilfeature.DefaultFeatureGate.Set(fmt.Sprintf("%s=true", features.BalanceAttachedNodeVolumes))
	nodeName := "node"
	ttl := 10 * time.Second
	testPods := []*v1.Pod{
		makeBasePod(t, nodeName, "test", "100m", "500", "", []v1.ContainerPort{{HostIP: "127.0.0.1", HostPort: 80, Protocol: "TCP"}}),
		makeBasePod(t, nodeName, "test", "200m", "1Ki", "", []v1.ContainerPort{{HostIP: "127.0.0.1", HostPort: 8080, Protocol: "TCP"}}),
	}
	tests := []struct {
		podsToAdd    []*v1.Pod
		podsToUpdate []*v1.Pod

		wNodeInfo []*NodeInfo
	}{{ // add a pod and then update it twice
		podsToAdd:    []*v1.Pod{testPods[0]},
		podsToUpdate: []*v1.Pod{testPods[0], testPods[1], testPods[0]},
		wNodeInfo: []*NodeInfo{{
			requestedResource: &Resource{
				MilliCPU: 200,
				Memory:   1024,
			},
			nonzeroRequest: &Resource{
				MilliCPU: 200,
				Memory:   1024,
			},
			TransientInfo:       newTransientSchedulerInfo(),
			allocatableResource: &Resource{},
			pods:                []*v1.Pod{testPods[1]},
			usedPorts:           newHostPortInfoBuilder().add("TCP", "127.0.0.1", 8080).build(),
			imageSizes:          map[string]int64{},
		}, {
			requestedResource: &Resource{
				MilliCPU: 100,
				Memory:   500,
			},
			nonzeroRequest: &Resource{
				MilliCPU: 100,
				Memory:   500,
			},
			TransientInfo:       newTransientSchedulerInfo(),
			allocatableResource: &Resource{},
			pods:                []*v1.Pod{testPods[0]},
			usedPorts:           newHostPortInfoBuilder().add("TCP", "127.0.0.1", 80).build(),
			imageSizes:          map[string]int64{},
		}},
	}}

	for _, tt := range tests {
		cache := newSchedulerCache(ttl, time.Second, nil)
		for _, podToAdd := range tt.podsToAdd {
			if err := cache.AddPod(podToAdd); err != nil {
				t.Fatalf("AddPod failed: %v", err)
			}
		}

		for i := range tt.podsToUpdate {
			if i == 0 {
				continue
			}
			if err := cache.UpdatePod(tt.podsToUpdate[i-1], tt.podsToUpdate[i]); err != nil {
				t.Fatalf("UpdatePod failed: %v", err)
			}
			// check after expiration. confirmed pods shouldn't be expired.
			n := cache.nodes[nodeName]
			deepEqualWithoutGeneration(t, i, n, tt.wNodeInfo[i-1])
		}
	}
}

func TestGetPodResizeRequirements(t *testing.T) {
	testPod := makeBasePod(t, "node", "test", "2", "2Gi", "", []v1.ContainerPort{{HostIP: "127.0.0.1", HostPort: 80, Protocol: "TCP"}})
	testPod.Spec.Containers[0].Name = "tc"

	tests := []struct {
		TestCaseDesc                string
		ResizeRequestAnnotation     string
		ExpectedResizeContainersMap map[string]v1.Container
		ExpectedPodResource         *Resource
	}{
		{
			"Request and Limits - both CPU and memory",
			`[{"name":"tc","resources":{"limits":{"cpu":"4","memory":"4Gi"},"requests":{"cpu":"4","memory":"4Gi"}}}]`,
			map[string]v1.Container{
				"tc": makeContainer(t, "tc", "4", "4Gi", "4", "4Gi"),
			},
			NewResource(v1.ResourceList{
						v1.ResourceCPU:    resource.MustParse("4"),
						v1.ResourceMemory: resource.MustParse("4Gi"),
					}),
		},
		{
			"Request and Limits - CPU only",
			`[{"name":"tc","resources":{"limits":{"cpu":"5"},"requests":{"cpu":"5"}}}]`,
			map[string]v1.Container{
				"tc": makeContainer(t, "tc", "5", "", "5", ""),
			},
			NewResource(v1.ResourceList{
						v1.ResourceCPU:    resource.MustParse("5"),
						v1.ResourceMemory: resource.MustParse("2Gi"),
					}),
		},
		{
			"Request and Limits - memory only",
			`[{"name":"tc","resources":{"limits":{"memory":"6Gi"},"requests":{"memory":"6Gi"}}}]`,
			map[string]v1.Container{
				"tc": makeContainer(t, "tc", "", "6Gi", "", "6Gi"),
			},
			NewResource(v1.ResourceList{
						v1.ResourceCPU:    resource.MustParse("2"),
						v1.ResourceMemory: resource.MustParse("6Gi"),
					}),
		},
	}

	for _, tt := range tests {
		if resizeContainersMap, podResource, err := getPodResizeRequirements(testPod, tt.ResizeRequestAnnotation); err != nil {
			t.Fatalf("Testcase '%s' - getPodResizeRequirements failed: %v", tt.TestCaseDesc, err)
		} else {
			if !reflect.DeepEqual(resizeContainersMap, tt.ExpectedResizeContainersMap) {
				t.Fatalf("Testcase '%s' - resize container map mismatch.\nExpected: %#v.\nActual:   %#v\nDiff: %s",
						tt.TestCaseDesc, tt.ExpectedResizeContainersMap, resizeContainersMap,
						diff.ObjectDiff(tt.ExpectedResizeContainersMap, resizeContainersMap))
			}
			if !reflect.DeepEqual(podResource, tt.ExpectedPodResource) {
				t.Fatalf("Testcase '%s' - pod resource mismatch.\nExpected: %#v.\nActual:   %#v\n",
						tt.TestCaseDesc, tt.ExpectedPodResource, podResource)
			}
		}
	}
}

func TestRestorePodResources(t *testing.T) {
	nodeName := "node"
	ttl := 10 * time.Second
	testNode := &v1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: nodeName,
			},
			Status: v1.NodeStatus{
					Allocatable: v1.ResourceList{
					v1.ResourceCPU:         resource.MustParse("8"),
					v1.ResourceMemory:	resource.MustParse("8Gi"),
					v1.ResourceName("foo"): resource.MustParse("1"),
				},
			},
		}

	oldPod := makeBasePod(t, nodeName, "test", "", "", "", []v1.ContainerPort{{HostIP: "127.0.0.1", HostPort: 80, Protocol: "TCP"}})
	newPod := makeBasePod(t, nodeName, "test", "", "", "", []v1.ContainerPort{{HostIP: "127.0.0.1", HostPort: 80, Protocol: "TCP"}})
	oldPod.Name = "test"
	newPod.Name = "test"

	tests := []struct {
		TestCaseDesc       string
		CurrentContainers  []v1.Container
		RestoreResources   string
		ExpectedContainers []v1.Container
	}{
		{
			"Guaranteed QoS - Restore CPU, memory requests and limits",
			[]v1.Container{
				makeContainer(t, "c1", "4", "5Gi", "4", "5Gi"),
			},
			`{"c1":{"name":"c1","resources":{"limits":{"cpu":"2", "memory":"3Gi"}, "requests":{"cpu":"2", "memory":"3Gi"}}}}`,
			[]v1.Container{
				makeContainer(t, "c1", "2", "3Gi", "2", "3Gi"),
			},
		},
		{
			"Burstable QoS - Restore c1 CPU and c2 memory",
			[]v1.Container{
				makeContainer(t, "c1", "3", "", "4", ""),
				makeContainer(t, "c2", "", "5Gi", "", "6Gi"),
			},
			`{"c1":{"name":"c1","resources":{"limits":{"cpu":"3"},"requests":{"cpu":"2"}}}, "c2":{"name":"c2","resources":{"limits":{"memory":"5Gi"},"requests":{"memory":"3Gi"}}}}`,
			[]v1.Container{
				makeContainer(t, "c1", "2", "", "3", ""),
				makeContainer(t, "c2", "", "3Gi", "", "5Gi"),
			},
		},
	}

	cache := newSchedulerCache(ttl, time.Second, nil)
	if err := cache.AddPod(oldPod); err != nil {
		t.Fatalf("AddPod failed: %v", err)
	}
	ni := cache.nodes[nodeName]
	ni.SetNode(testNode)
	podKey, _ := getPodKey(oldPod)
	currPodState, _ := cache.podStates[podKey]
	cachedPod := currPodState.pod

	for _, tt := range tests {
		oldPod.Spec.Containers = tt.CurrentContainers
		newPod.Spec.Containers = tt.CurrentContainers
		cachedPod.Spec.Containers = tt.CurrentContainers

		if err := cache.restorePodResources(oldPod, newPod, tt.RestoreResources); err != nil {
			t.Fatalf("Testcase '%s' - RestorePodResources failed: %v", tt.TestCaseDesc, err)
		}
		if !reflect.DeepEqual(newPod.Spec.Containers, tt.ExpectedContainers) {
			t.Fatalf("Testcase '%s' - Container spec mismatch.\nExpected: %#v.\nActual:   %#v\nDiff: %s",
					tt.TestCaseDesc, tt.ExpectedContainers, newPod.Spec.Containers,
					diff.ObjectDiff(tt.ExpectedContainers, newPod.Spec.Containers))
		}
		if !reflect.DeepEqual(cachedPod.Spec.Containers, tt.ExpectedContainers) {
			t.Fatalf("Testcase '%s' - Cached container spec mismatch.\nExpected: %#v.\nActual:   %#v\nDiff: %s",
					tt.TestCaseDesc, tt.ExpectedContainers, cachedPod.Spec.Containers,
					diff.ObjectDiff(tt.ExpectedContainers, cachedPod.Spec.Containers))
		}
	}
}

func TestSetupInPlaceResizeAction(t *testing.T) {
	nodeName := "node"
	ttl := 10 * time.Second
	testNode := &v1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: nodeName,
			},
			Status: v1.NodeStatus{
					Allocatable: v1.ResourceList{
					v1.ResourceCPU:         resource.MustParse("1000m"),
					v1.ResourceMemory:	resource.MustParse("2000"),
					v1.ResourceName("foo"): resource.MustParse("1"),
				},
			},
		}

	oldPod := makeBasePod(t, nodeName, "test", "", "", "", []v1.ContainerPort{{HostIP: "127.0.0.1", HostPort: 80, Protocol: "TCP"}})
	newPod := makeBasePod(t, nodeName, "test", "", "", "", []v1.ContainerPort{{HostIP: "127.0.0.1", HostPort: 80, Protocol: "TCP"}})
	oldPod.Name = "test"
	oldPod.ResourceVersion = "123"
	newPod.Name = "test"
	newPod.ResourceVersion = "456"
	newPod.Annotations = make(map[string]string)

	tests := []struct {
		TestCaseDesc             string
		CurrentContainers        []v1.Container
		ResizeContainersMap      map[string]v1.Container
		ExpectedContainers       []v1.Container
		ExpectedRestoreResources string
	}{
		{
			"Guaranteed QoS - Update CPU and memory requests, limits",
			[]v1.Container{
				makeContainer(t, "c1", "2", "3Gi", "2", "3Gi"),
			},
			map[string]v1.Container{
				"c1": makeContainer(t, "c1", "4", "6Gi", "4", "6Gi"),
			},
			[]v1.Container{
				makeContainer(t, "c1", "4", "6Gi", "4", "6Gi"),
			},
			`{"c1":{"name":"c1","resources":{"limits":{"cpu":"2","memory":"3Gi"},"requests":{"cpu":"2","memory":"3Gi"}}}}`,
		},
		{
			"Burstable QoS - Update c1 CPU and c2 memory requests, limits",
			[]v1.Container{
				makeContainer(t, "c1", "1", "", "2", ""),
				makeContainer(t, "c2", "", "3Gi", "", "4Gi"),
			},
			map[string]v1.Container{
				"c1": makeContainer(t, "c1", "3", "", "4", ""),
				"c2": makeContainer(t, "c2", "", "5Gi", "", "6Gi"),
			},
			[]v1.Container{
				makeContainer(t, "c1", "3", "", "4", ""),
				makeContainer(t, "c2", "", "5Gi", "", "6Gi"),
			},
			`{"c1":{"name":"c1","resources":{"limits":{"cpu":"2"},"requests":{"cpu":"1"}}},"c2":{"name":"c2","resources":{"limits":{"memory":"4Gi"},"requests":{"memory":"3Gi"}}}}`,
		},
	}

	cache := newSchedulerCache(ttl, time.Second, nil)
	if err := cache.AddPod(oldPod); err != nil {
		t.Fatalf("AddPod failed: %v", err)
	}
	ni := cache.nodes[nodeName]
	ni.SetNode(testNode)
	podKey, _ := getPodKey(oldPod)
	currPodState, _ := cache.podStates[podKey]
	cachedPod := currPodState.pod

	for i, tt := range tests {
		oldPod.Spec.Containers = tt.CurrentContainers
		newPod.ResourceVersion = fmt.Sprintf("456%d", i)
		newPod.Spec.Containers = tt.CurrentContainers
		cachedPod.Spec.Containers = tt.CurrentContainers

		if err := cache.setupInPlaceResizeAction(oldPod, newPod, tt.ResizeContainersMap); err != nil {
			t.Fatalf("Testcase '%s' - setupInPlaceResizeAction failed: %v", tt.TestCaseDesc, err)
		}
		if !reflect.DeepEqual(newPod.Spec.Containers, tt.ExpectedContainers) {
			t.Fatalf("Testcase '%s' - Container spec mismatch.\nExpected: %#v.\nActual:   %#v\nDiff: %s",
					tt.TestCaseDesc, tt.ExpectedContainers, newPod.Spec.Containers,
					diff.ObjectDiff(tt.ExpectedContainers, newPod.Spec.Containers))
		}
		if !reflect.DeepEqual(cachedPod.Spec.Containers, tt.ExpectedContainers) {
			t.Fatalf("Testcase '%s' - Cached container spec mismatch.\nExpected: %#v.\nActual:   %#v\nDiff: %s",
					tt.TestCaseDesc, tt.ExpectedContainers, cachedPod.Spec.Containers,
					diff.ObjectDiff(tt.ExpectedContainers, cachedPod.Spec.Containers))
		}
		if newPod.Annotations[api.AnnotationResizeResourcesAction] != string(api.ResizeActionUpdate) {
			t.Fatalf("Testcase '%s' - resize action mismatch. Expected: %s. Actual: %s\n", tt.TestCaseDesc,
					string(api.ResizeActionUpdate), newPod.Annotations[api.AnnotationResizeResourcesAction])
		}
		if newPod.Annotations[api.AnnotationResizeResourcesActionVer] != newPod.ResourceVersion {
			t.Fatalf("Testcase '%s' - resize action version mismatch. Expected: %s. Actual: %s\n", tt.TestCaseDesc,
					newPod.ResourceVersion, newPod.Annotations[api.AnnotationResizeResourcesActionVer])
		}
		if newPod.Annotations[api.AnnotationResizeResourcesPrevious] != tt.ExpectedRestoreResources {
			t.Fatalf("Testcase '%s' - restore resources value mismatch.\nExpected: %s\n  Actual: %s\n", tt.TestCaseDesc,
					tt.ExpectedRestoreResources, newPod.Annotations[api.AnnotationResizeResourcesPrevious])
		}
	}
}

func TestProcessPodResizeStatus(t *testing.T) {
	nodeName := "node"
	ttl := 10 * time.Second
	testNode := &v1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: nodeName,
			},
			Status: v1.NodeStatus{
					Allocatable: v1.ResourceList{
					v1.ResourceCPU:         resource.MustParse("8"),
					v1.ResourceMemory:	resource.MustParse("8Gi"),
					v1.ResourceName("foo"): resource.MustParse("1"),
				},
			},
		}

	oldPod := makeBasePod(t, nodeName, "test", "", "", "", []v1.ContainerPort{{HostIP: "127.0.0.1", HostPort: 80, Protocol: "TCP"}})
	newPod := makeBasePod(t, nodeName, "test", "", "", "", []v1.ContainerPort{{HostIP: "127.0.0.1", HostPort: 80, Protocol: "TCP"}})
	actionVer := "345"
	oldPod.Name = "test"
	oldPod.ResourceVersion = "123"
	newPod.Name = "test"
	newPod.ResourceVersion = "456"
	newPod.Annotations = make(map[string]string)

	tests := []struct {
		TestCaseDesc             string
		CurrentContainers        []v1.Container
		ResizeStatusConditions   []v1.PodCondition
		PodRestoreResources      string
		ExpectedContainers       []v1.Container
		ExpectedResizeAction     string
		ExpectedRestoreResources string
	}{
		{
			"No resize status condition - expect no changes to pod",
			[]v1.Container{
				makeContainer(t, "c1", "4", "6Gi", "4", "6Gi"),
			},
			[]v1.PodCondition{},
			`{"c1":{"name":"c1","resources":{"limits":{"cpu":"2","memory":"4Gi"},"requests":{"cpu":"2","memory":"4Gi"}}}}`,
			[]v1.Container{
				makeContainer(t, "c1", "4", "6Gi", "4", "6Gi"),
			},
			string(api.ResizeActionUpdate),
			`{"c1":{"name":"c1","resources":{"limits":{"cpu":"2","memory":"4Gi"},"requests":{"cpu":"2","memory":"4Gi"}}}}`,
		},
		{
			"Resize status condition success - expect update done, no changes to resources",
			[]v1.Container{
				makeContainer(t, "c1", "4", "6Gi", "4", "6Gi"),
			},
			[]v1.PodCondition{
				{
					Type:    v1.PodResourcesResizeStatus,
					Status:  v1.ConditionTrue,
					Message: actionVer,
				},
			},
			`{"c1":{"name":"c1","resources":{"limits":{"cpu":"2","memory":"4Gi"},"requests":{"cpu":"2","memory":"4Gi"}}}}`,
			[]v1.Container{
				makeContainer(t, "c1", "4", "6Gi", "4", "6Gi"),
			},
			string(api.ResizeActionUpdateDone),
			"",
		},
		{
			"Resize status condition failed - expect update done, resources restored to previous values",
			[]v1.Container{
				makeContainer(t, "c1", "4", "6Gi", "4", "6Gi"),
			},
			[]v1.PodCondition{
				{
					Type:    v1.PodResourcesResizeStatus,
					Status:  v1.ConditionFalse,
					Message: actionVer,
				},
			},
			`{"c1":{"name":"c1","resources":{"limits":{"cpu":"2","memory":"4Gi"},"requests":{"cpu":"2","memory":"4Gi"}}}}`,
			[]v1.Container{
				makeContainer(t, "c1", "2", "4Gi", "2", "4Gi"),
			},
			string(api.ResizeActionUpdateDone),
			"",
		},
	}

	cache := newSchedulerCache(ttl, time.Second, nil)
	if err := cache.AddPod(oldPod); err != nil {
		t.Fatalf("AddPod failed: %v", err)
	}
	ni := cache.nodes[nodeName]
	ni.SetNode(testNode)
	podKey, _ := getPodKey(oldPod)
	currPodState, _ := cache.podStates[podKey]
	cachedPod := currPodState.pod

	for i, tt := range tests {
		oldPod.Spec.Containers = tt.CurrentContainers
		cachedPod.Spec.Containers = tt.CurrentContainers
		newPod.ResourceVersion = fmt.Sprintf("456%d", i)
		newPod.Spec.Containers = tt.CurrentContainers
		newPod.Annotations[api.AnnotationResizeResourcesPrevious] = tt.PodRestoreResources
		newPod.Annotations[api.AnnotationResizeResourcesActionVer] = actionVer
		newPod.Annotations[api.AnnotationResizeResourcesAction] = string(api.ResizeActionUpdate)
		newPod.Status.Conditions = tt.ResizeStatusConditions

		cache.processPodResizeStatus(oldPod, newPod)
		if !reflect.DeepEqual(newPod.Spec.Containers, tt.ExpectedContainers) {
			t.Fatalf("Testcase '%s' - Container spec mismatch.\nExpected: %#v.\nActual:   %#v\nDiff: %s",
					tt.TestCaseDesc, tt.ExpectedContainers, newPod.Spec.Containers,
					diff.ObjectDiff(tt.ExpectedContainers, newPod.Spec.Containers))
		}
		if !reflect.DeepEqual(cachedPod.Spec.Containers, tt.ExpectedContainers) {
			t.Fatalf("Testcase '%s' - Cached container spec mismatch.\nExpected: %#v.\nActual:   %#v\nDiff: %s",
					tt.TestCaseDesc, tt.ExpectedContainers, cachedPod.Spec.Containers,
					diff.ObjectDiff(tt.ExpectedContainers, cachedPod.Spec.Containers))
		}
		if newPod.Annotations[api.AnnotationResizeResourcesAction] != tt.ExpectedResizeAction {
			t.Fatalf("Testcase '%s' - resize action mismatch. Expected: %s. Actual: %s\n", tt.TestCaseDesc,
					tt.ExpectedResizeAction, newPod.Annotations[api.AnnotationResizeResourcesAction])
		}
		if newPod.Annotations[api.AnnotationResizeResourcesPrevious] != tt.ExpectedRestoreResources {
			t.Fatalf("Testcase '%s' - restore resources value mismatch.\nExpected: %s\n  Actual: %s\n", tt.TestCaseDesc,
					tt.ExpectedRestoreResources, newPod.Annotations[api.AnnotationResizeResourcesPrevious])
		}
	}
}

func TestCheckPodDisruptionBudgetOk(t *testing.T) {
	nodeName := "node"
	ttl := 10 * time.Second
	testNode := &v1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: nodeName,
			},
			Status: v1.NodeStatus{
					Allocatable: v1.ResourceList{
					v1.ResourceCPU:            resource.MustParse("4"),
					v1.ResourceMemory:	   resource.MustParse("4Gi"),
					v1.ResourceName("foores"): resource.MustParse("1"),
				},
			},
		}

	testPod := makeBasePod(t, nodeName, "test", "2", "2Gi", "", []v1.ContainerPort{{HostIP: "127.0.0.1", HostPort: 80, Protocol: "TCP"}})
	testPod.Labels = map[string]string{"foo":"bar", "foo2":"bar2"}

	tests := []struct {
		TestCaseDesc    string
		TestPDB         *v1beta1.PodDisruptionBudget
		ExpectedValue   bool
	}{
		{
			"No pod disruption budget is specified",
			nil,
			true,
		},
		{
			"Pod is within pod disruption budget",
			&v1beta1.PodDisruptionBudget{
				ObjectMeta: metav1.ObjectMeta{
							Name:      "foopdb",
							UID:       "foouid",
						},
				Spec:       v1beta1.PodDisruptionBudgetSpec{
							Selector:       &metav1.LabelSelector{MatchLabels: map[string]string{"foo":"bar"}},
						},
				Status:     v1beta1.PodDisruptionBudgetStatus{
							PodDisruptionsAllowed: 1,
						},
			},
			true,
		},
		{
			"Pod is facing pod disruption budget violation",
			&v1beta1.PodDisruptionBudget{
				ObjectMeta: metav1.ObjectMeta{
							Name:      "foopdb",
							UID:       "foouid",
						},
				Spec:       v1beta1.PodDisruptionBudgetSpec{
							Selector:       &metav1.LabelSelector{MatchLabels: map[string]string{"foo":"bar"}},
						},
				Status:     v1beta1.PodDisruptionBudgetStatus{
							PodDisruptionsAllowed: 0,
						},
			},
			false,
		},
	}

	cache := newSchedulerCache(ttl, time.Second, nil)
	if err := cache.AddPod(testPod); err != nil {
		t.Fatalf("AddPod failed: %v", err)
	}
	ni := cache.nodes[nodeName]
	ni.SetNode(testNode)

	for _, tt := range tests {
		if tt.TestPDB != nil {
			if pdbErr := cache.AddPDB(tt.TestPDB); pdbErr != nil {
				t.Fatalf("AddPDB failed: %v", pdbErr)
			}
		}
		if ok, err := cache.checkPodDisruptionBudgetOk(testPod); err != nil {
			t.Fatalf("Testcase '%s' - checkPodDisruptionBudgetOk error: %v", tt.TestCaseDesc, err)
		} else {
			if ok != tt.ExpectedValue {
				t.Fatalf("Testcase '%s' Expected: %v. Actual: %v", tt.TestCaseDesc, tt.ExpectedValue, ok)
			}
		}
		if tt.TestPDB != nil {
			cache.RemovePDB(tt.TestPDB)
		}
	}
}

// TestUpdatePodResources tests updatePod that requests resource update.
func TestUpdatePodResources(t *testing.T) {
	// Enable volumesOnNodeForBalancing to do balanced resource allocation
	utilfeature.DefaultFeatureGate.Set(fmt.Sprintf("%s=true", features.BalanceAttachedNodeVolumes))
	utilfeature.DefaultFeatureGate.Set(fmt.Sprintf("%s=true", features.VerticalScaling))
	nodeName := "node"
	ttl := 10 * time.Second
	testNode := &v1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: nodeName,
			},
			Status: v1.NodeStatus{
					Allocatable: v1.ResourceList{
					v1.ResourceCPU:         resource.MustParse("1000m"),
					v1.ResourceMemory:	resource.MustParse("2000"),
					v1.ResourceName("foo"): resource.MustParse("1"),
				},
			},
		}

	oldPod := makeBasePod(t, nodeName, "test", "100m", "500", "", []v1.ContainerPort{{HostIP: "127.0.0.1", HostPort: 80, Protocol: "TCP"}})
	newPod := makeBasePod(t, nodeName, "test", "100m", "500", "", []v1.ContainerPort{{HostIP: "127.0.0.1", HostPort: 80, Protocol: "TCP"}})
	oldPod.ResourceVersion = "123"
	oldPod.Name = "test"
	oldPod.Spec.Containers[0].Name = "test"
	oldPod.Status.Phase = v1.PodRunning
	newPod.ResourceVersion = "456"
	newPod.Name = "test"
	newPod.Spec.Containers[0].Name = "test"
	newPod.Status.Phase = v1.PodRunning
	newPod.Annotations = make(map[string]string)

	tests := []struct {
		TestCaseDesc   string
		ResizePolicy   string
		ResizeRequest  string
		ExpectedPod    *v1.Pod
		ExpectedAction string
	}{
		{
			"InPlacePreferred policy - update CPU only, expect in-place resizing",
			"InPlacePreferred",
			`[{"name":"test","resources":{"requests":{"cpu":"200m"}}}]`,
			makeBasePod(t, nodeName, "test", "200m", "500", "", []v1.ContainerPort{{HostIP: "127.0.0.1", HostPort: 80, Protocol: "TCP"}}),
			"UpdatePodForResizing",
		},
		{
			"InPlacePreferred policy - update memory only, expect in-place resizing",
			"InPlacePreferred",
			`[{"name":"test","resources":{"requests":{"memory":"800"}}}]`,
			makeBasePod(t, nodeName, "test", "200m", "800", "", []v1.ContainerPort{{HostIP: "127.0.0.1", HostPort: 80, Protocol: "TCP"}}),
			"UpdatePodForResizing",
		},
		{
			"InPlacePreferred policy - update CPU and memory, expect in-place resizing",
			"InPlacePreferred",
			`[{"name":"test","resources":{"requests":{"cpu":"500m","memory":"1000"}}}]`,
			makeBasePod(t, nodeName, "test", "500m", "1000", "", []v1.ContainerPort{{HostIP: "127.0.0.1", HostPort: 80, Protocol: "TCP"}}),
			"UpdatePodForResizing",
		},
		{
			"InPlaceOnly policy - update CPU and memory, expect pod not rescheduled due to policy",
			"InPlaceOnly",
			`[{"name":"test","resources":{"requests":{"cpu":"800m","memory":"3000"}}}]`,
			makeBasePod(t, nodeName, "test", "500m", "1000", "", []v1.ContainerPort{{HostIP: "127.0.0.1", HostPort: 80, Protocol: "TCP"}}),
			"PodNotResizedDueToPolicy",
		},
		{
			"Restart policy - update memory, expect pod reschedule for resizing",
			"Restart",
			`[{"name":"test","resources":{"requests":{"memory":"1500"}}}]`,
			makeBasePod(t, nodeName, "test", "500m", "1000", "", []v1.ContainerPort{{HostIP: "127.0.0.1", HostPort: 80, Protocol: "TCP"}}),
			"DeletePodForResizing",
		},
		{
			"InPlacePreferred policy - update CPU and memory, expect pod reschedule",
			"InPlacePreferred",
			`[{"name":"test","resources":{"requests":{"cpu":"800m","memory":"3000"}}}]`,
			makeBasePod(t, nodeName, "test", "500m", "1000", "", []v1.ContainerPort{{HostIP: "127.0.0.1", HostPort: 80, Protocol: "TCP"}}),
			"DeletePodForResizing",
		},
	}

	cache := newSchedulerCache(ttl, time.Second, nil)
	if err := cache.AddPod(oldPod); err != nil {
		t.Fatalf("AddPod failed: %v", err)
	}
	ni := cache.nodes[nodeName]
	ni.SetNode(testNode)

	for i, tt := range tests {
		newPod.ResourceVersion = fmt.Sprintf("456%d", i)
		newPod.Annotations[api.AnnotationResizeResourcesPolicy] = tt.ResizePolicy
		newPod.Annotations[api.AnnotationResizeResourcesRequest] = tt.ResizeRequest

		for k, _ := range oldPod.Spec.Containers[0].Resources.Requests {
			oldPod.Spec.Containers[0].Resources.Requests[k] = newPod.Spec.Containers[0].Resources.Requests[k]
		}
		tt.ExpectedPod.Spec.Containers[0].Name = "test"

		if err := cache.UpdatePod(oldPod, newPod); err != nil {
			t.Fatalf("Testcase '%s' - UpdatePod failed: %v", tt.TestCaseDesc, err)
		}
		if !reflect.DeepEqual(newPod.Spec.Containers, tt.ExpectedPod.Spec.Containers) {
			t.Fatalf("Testcase '%s' - Container spec mismatch.\nExpected: %#v.\nActual:   %#v\nDiff: %s",
					tt.TestCaseDesc, tt.ExpectedPod.Spec.Containers, newPod.Spec.Containers,
					diff.ObjectDiff(tt.ExpectedPod.Spec.Containers, newPod.Spec.Containers))
		}
		if newPod.Annotations[api.AnnotationResizeResourcesActionVer] != newPod.ResourceVersion {
			t.Fatalf("Testcase '%s' - resize action version mismatch. Expected: %s. Actual: %s\n", tt.TestCaseDesc,
					newPod.ResourceVersion, newPod.Annotations[api.AnnotationResizeResourcesActionVer])
		}
		if newPod.Annotations[api.AnnotationResizeResourcesAction] != tt.ExpectedAction {
			t.Fatalf("Testcase '%s' - resource update action mismatch. Expected: %s. Actual: %s\n",
					tt.TestCaseDesc, tt.ExpectedAction, newPod.Annotations[api.AnnotationResizeResourcesAction])
		}
	}
}

// TestExpireAddUpdatePod test the sequence that a pod is expired, added, then updated
func TestExpireAddUpdatePod(t *testing.T) {
	// Enable volumesOnNodeForBalancing to do balanced resource allocation
	utilfeature.DefaultFeatureGate.Set(fmt.Sprintf("%s=true", features.BalanceAttachedNodeVolumes))
	nodeName := "node"
	ttl := 10 * time.Second
	testPods := []*v1.Pod{
		makeBasePod(t, nodeName, "test", "100m", "500", "", []v1.ContainerPort{{HostIP: "127.0.0.1", HostPort: 80, Protocol: "TCP"}}),
		makeBasePod(t, nodeName, "test", "200m", "1Ki", "", []v1.ContainerPort{{HostIP: "127.0.0.1", HostPort: 8080, Protocol: "TCP"}}),
	}
	tests := []struct {
		podsToAssume []*v1.Pod
		podsToAdd    []*v1.Pod
		podsToUpdate []*v1.Pod

		wNodeInfo []*NodeInfo
	}{{ // Pod is assumed, expired, and added. Then it would be updated twice.
		podsToAssume: []*v1.Pod{testPods[0]},
		podsToAdd:    []*v1.Pod{testPods[0]},
		podsToUpdate: []*v1.Pod{testPods[0], testPods[1], testPods[0]},
		wNodeInfo: []*NodeInfo{{
			requestedResource: &Resource{
				MilliCPU: 200,
				Memory:   1024,
			},
			nonzeroRequest: &Resource{
				MilliCPU: 200,
				Memory:   1024,
			},
			TransientInfo:       newTransientSchedulerInfo(),
			allocatableResource: &Resource{},
			pods:                []*v1.Pod{testPods[1]},
			usedPorts:           newHostPortInfoBuilder().add("TCP", "127.0.0.1", 8080).build(),
			imageSizes:          map[string]int64{},
		}, {
			requestedResource: &Resource{
				MilliCPU: 100,
				Memory:   500,
			},
			nonzeroRequest: &Resource{
				MilliCPU: 100,
				Memory:   500,
			},
			TransientInfo:       newTransientSchedulerInfo(),
			allocatableResource: &Resource{},
			pods:                []*v1.Pod{testPods[0]},
			usedPorts:           newHostPortInfoBuilder().add("TCP", "127.0.0.1", 80).build(),
			imageSizes:          map[string]int64{},
		}},
	}}

	now := time.Now()
	for _, tt := range tests {
		cache := newSchedulerCache(ttl, time.Second, nil)
		for _, podToAssume := range tt.podsToAssume {
			if err := assumeAndFinishBinding(cache, podToAssume, now); err != nil {
				t.Fatalf("assumePod failed: %v", err)
			}
		}
		cache.cleanupAssumedPods(now.Add(2 * ttl))

		for _, podToAdd := range tt.podsToAdd {
			if err := cache.AddPod(podToAdd); err != nil {
				t.Fatalf("AddPod failed: %v", err)
			}
		}

		for i := range tt.podsToUpdate {
			if i == 0 {
				continue
			}
			if err := cache.UpdatePod(tt.podsToUpdate[i-1], tt.podsToUpdate[i]); err != nil {
				t.Fatalf("UpdatePod failed: %v", err)
			}
			// check after expiration. confirmed pods shouldn't be expired.
			n := cache.nodes[nodeName]
			deepEqualWithoutGeneration(t, i, n, tt.wNodeInfo[i-1])
		}
	}
}

func makePodWithEphemeralStorage(nodeName, ephemeralStorage string) *v1.Pod {
	req := v1.ResourceList{
		v1.ResourceEphemeralStorage: resource.MustParse(ephemeralStorage),
	}
	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default-namespace",
			Name:      "pod-with-ephemeral-storage",
			UID:       types.UID("pod-with-ephemeral-storage"),
		},
		Spec: v1.PodSpec{
			Containers: []v1.Container{{
				Resources: v1.ResourceRequirements{
					Requests: req,
				},
			}},
			NodeName: nodeName,
		},
	}
}

func TestEphemeralStorageResource(t *testing.T) {
	// Enable volumesOnNodeForBalancing to do balanced resource allocation
	utilfeature.DefaultFeatureGate.Set(fmt.Sprintf("%s=true", features.BalanceAttachedNodeVolumes))
	nodeName := "node"
	podE := makePodWithEphemeralStorage(nodeName, "500")
	tests := []struct {
		pod       *v1.Pod
		wNodeInfo *NodeInfo
	}{
		{
			pod: podE,
			wNodeInfo: &NodeInfo{
				requestedResource: &Resource{
					EphemeralStorage: 500,
				},
				nonzeroRequest: &Resource{
					MilliCPU: priorityutil.DefaultMilliCPURequest,
					Memory:   priorityutil.DefaultMemoryRequest,
				},
				TransientInfo:       newTransientSchedulerInfo(),
				allocatableResource: &Resource{},
				pods:                []*v1.Pod{podE},
				usedPorts:           schedutil.HostPortInfo{},
				imageSizes:          map[string]int64{},
			},
		},
	}
	for i, tt := range tests {
		cache := newSchedulerCache(time.Second, time.Second, nil)
		if err := cache.AddPod(tt.pod); err != nil {
			t.Fatalf("AddPod failed: %v", err)
		}
		n := cache.nodes[nodeName]
		deepEqualWithoutGeneration(t, i, n, tt.wNodeInfo)

		if err := cache.RemovePod(tt.pod); err != nil {
			t.Fatalf("RemovePod failed: %v", err)
		}

		n = cache.nodes[nodeName]
		if n != nil {
			t.Errorf("#%d: expecting pod deleted and nil node info, get=%s", i, n)
		}
	}
}

// TestRemovePod tests after added pod is removed, its information should also be subtracted.
func TestRemovePod(t *testing.T) {
	// Enable volumesOnNodeForBalancing to do balanced resource allocation
	utilfeature.DefaultFeatureGate.Set(fmt.Sprintf("%s=true", features.BalanceAttachedNodeVolumes))
	nodeName := "node"
	basePod := makeBasePod(t, nodeName, "test", "100m", "500", "", []v1.ContainerPort{{HostIP: "127.0.0.1", HostPort: 80, Protocol: "TCP"}})
	tests := []struct {
		pod       *v1.Pod
		wNodeInfo *NodeInfo
	}{{
		pod: basePod,
		wNodeInfo: &NodeInfo{
			requestedResource: &Resource{
				MilliCPU: 100,
				Memory:   500,
			},
			nonzeroRequest: &Resource{
				MilliCPU: 100,
				Memory:   500,
			},
			TransientInfo:       newTransientSchedulerInfo(),
			allocatableResource: &Resource{},
			pods:                []*v1.Pod{basePod},
			usedPorts:           newHostPortInfoBuilder().add("TCP", "127.0.0.1", 80).build(),
			imageSizes:          map[string]int64{},
		},
	}}

	for i, tt := range tests {
		cache := newSchedulerCache(time.Second, time.Second, nil)
		if err := cache.AddPod(tt.pod); err != nil {
			t.Fatalf("AddPod failed: %v", err)
		}
		n := cache.nodes[nodeName]
		deepEqualWithoutGeneration(t, i, n, tt.wNodeInfo)

		if err := cache.RemovePod(tt.pod); err != nil {
			t.Fatalf("RemovePod failed: %v", err)
		}

		n = cache.nodes[nodeName]
		if n != nil {
			t.Errorf("#%d: expecting pod deleted and nil node info, get=%s", i, n)
		}
	}
}

func TestForgetPod(t *testing.T) {
	nodeName := "node"
	basePod := makeBasePod(t, nodeName, "test", "100m", "500", "", []v1.ContainerPort{{HostIP: "127.0.0.1", HostPort: 80, Protocol: "TCP"}})
	tests := []struct {
		pods []*v1.Pod
	}{{
		pods: []*v1.Pod{basePod},
	}}
	now := time.Now()
	ttl := 10 * time.Second

	for i, tt := range tests {
		cache := newSchedulerCache(ttl, time.Second, nil)
		for _, pod := range tt.pods {
			if err := assumeAndFinishBinding(cache, pod, now); err != nil {
				t.Fatalf("assumePod failed: %v", err)
			}
			isAssumed, err := cache.IsAssumedPod(pod)
			if err != nil {
				t.Fatalf("IsAssumedPod failed: %v.", err)
			}
			if !isAssumed {
				t.Fatalf("Pod is expected to be assumed.")
			}
			assumedPod, err := cache.GetPod(pod)
			if err != nil {
				t.Fatalf("GetPod failed: %v.", err)
			}
			if assumedPod.Namespace != pod.Namespace {
				t.Errorf("assumedPod.Namespace != pod.Namespace (%s != %s)", assumedPod.Namespace, pod.Namespace)
			}
			if assumedPod.Name != pod.Name {
				t.Errorf("assumedPod.Name != pod.Name (%s != %s)", assumedPod.Name, pod.Name)
			}
		}
		for _, pod := range tt.pods {
			if err := cache.ForgetPod(pod); err != nil {
				t.Fatalf("ForgetPod failed: %v", err)
			}
			isAssumed, err := cache.IsAssumedPod(pod)
			if err != nil {
				t.Fatalf("IsAssumedPod failed: %v.", err)
			}
			if isAssumed {
				t.Fatalf("Pod is expected to be unassumed.")
			}
		}
		cache.cleanupAssumedPods(now.Add(2 * ttl))
		if n := cache.nodes[nodeName]; n != nil {
			t.Errorf("#%d: expecting pod deleted and nil node info, get=%s", i, n)
		}
	}
}

// getResourceRequest returns the resource request of all containers in Pods;
// excuding initContainers.
func getResourceRequest(pod *v1.Pod) v1.ResourceList {
	result := &Resource{}
	for _, container := range pod.Spec.Containers {
		result.Add(container.Resources.Requests)
	}

	return result.ResourceList()
}

// buildNodeInfo creates a NodeInfo by simulating node operations in cache.
func buildNodeInfo(node *v1.Node, pods []*v1.Pod) *NodeInfo {
	expected := NewNodeInfo()

	// Simulate SetNode.
	expected.node = node
	expected.allocatableResource = NewResource(node.Status.Allocatable)
	expected.taints = node.Spec.Taints
	expected.generation++

	for _, pod := range pods {
		// Simulate AddPod
		expected.pods = append(expected.pods, pod)
		expected.requestedResource.Add(getResourceRequest(pod))
		expected.nonzeroRequest.Add(getResourceRequest(pod))
		expected.updateUsedPorts(pod, true)
		expected.generation++
	}

	return expected
}

// TestNodeOperators tests node operations of cache, including add, update
// and remove.
func TestNodeOperators(t *testing.T) {
	// Test datas
	nodeName := "test-node"
	cpu1 := resource.MustParse("1000m")
	mem100m := resource.MustParse("100m")
	cpuHalf := resource.MustParse("500m")
	mem50m := resource.MustParse("50m")
	resourceFooName := "example.com/foo"
	resourceFoo := resource.MustParse("1")

	tests := []struct {
		node *v1.Node
		pods []*v1.Pod
	}{
		{
			node: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: nodeName,
				},
				Status: v1.NodeStatus{
					Allocatable: v1.ResourceList{
						v1.ResourceCPU:                   cpu1,
						v1.ResourceMemory:                mem100m,
						v1.ResourceName(resourceFooName): resourceFoo,
					},
				},
				Spec: v1.NodeSpec{
					Taints: []v1.Taint{
						{
							Key:    "test-key",
							Value:  "test-value",
							Effect: v1.TaintEffectPreferNoSchedule,
						},
					},
				},
			},
			pods: []*v1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "pod1",
						UID:  types.UID("pod1"),
					},
					Spec: v1.PodSpec{
						NodeName: nodeName,
						Containers: []v1.Container{
							{
								Resources: v1.ResourceRequirements{
									Requests: v1.ResourceList{
										v1.ResourceCPU:    cpuHalf,
										v1.ResourceMemory: mem50m,
									},
								},
								Ports: []v1.ContainerPort{
									{
										Name:          "http",
										HostPort:      80,
										ContainerPort: 80,
									},
								},
							},
						},
					},
				},
			},
		},
		{
			node: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: nodeName,
				},
				Status: v1.NodeStatus{
					Allocatable: v1.ResourceList{
						v1.ResourceCPU:                   cpu1,
						v1.ResourceMemory:                mem100m,
						v1.ResourceName(resourceFooName): resourceFoo,
					},
				},
				Spec: v1.NodeSpec{
					Taints: []v1.Taint{
						{
							Key:    "test-key",
							Value:  "test-value",
							Effect: v1.TaintEffectPreferNoSchedule,
						},
					},
				},
			},
			pods: []*v1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "pod1",
						UID:  types.UID("pod1"),
					},
					Spec: v1.PodSpec{
						NodeName: nodeName,
						Containers: []v1.Container{
							{
								Resources: v1.ResourceRequirements{
									Requests: v1.ResourceList{
										v1.ResourceCPU:    cpuHalf,
										v1.ResourceMemory: mem50m,
									},
								},
							},
						},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "pod2",
						UID:  types.UID("pod2"),
					},
					Spec: v1.PodSpec{
						NodeName: nodeName,
						Containers: []v1.Container{
							{
								Resources: v1.ResourceRequirements{
									Requests: v1.ResourceList{
										v1.ResourceCPU:    cpuHalf,
										v1.ResourceMemory: mem50m,
									},
								},
							},
						},
					},
				},
			},
		},
	}

	for _, test := range tests {
		expected := buildNodeInfo(test.node, test.pods)
		node := test.node

		cache := newSchedulerCache(time.Second, time.Second, nil)
		cache.AddNode(node)
		for _, pod := range test.pods {
			cache.AddPod(pod)
		}

		// Case 1: the node was added into cache successfully.
		got, found := cache.nodes[node.Name]
		if !found {
			t.Errorf("Failed to find node %v in schedulercache.", node.Name)
		}

		// Generations are globally unique. We check in our unit tests that they are incremented correctly.
		expected.generation = got.generation
		if !reflect.DeepEqual(got, expected) {
			t.Errorf("Failed to add node into schedulercache:\n got: %+v \nexpected: %+v", got, expected)
		}

		// Case 2: dump cached nodes successfully.
		cachedNodes := map[string]*NodeInfo{}
		cache.UpdateNodeNameToInfoMap(cachedNodes)
		newNode, found := cachedNodes[node.Name]
		if !found || len(cachedNodes) != 1 {
			t.Errorf("failed to dump cached nodes:\n got: %v \nexpected: %v", cachedNodes, cache.nodes)
		}
		expected.generation = newNode.generation
		if !reflect.DeepEqual(newNode, expected) {
			t.Errorf("Failed to clone node:\n got: %+v, \n expected: %+v", newNode, expected)
		}

		// Case 3: update node attribute successfully.
		node.Status.Allocatable[v1.ResourceMemory] = mem50m
		expected.allocatableResource.Memory = mem50m.Value()
		cache.UpdateNode(nil, node)
		got, found = cache.nodes[node.Name]
		if !found {
			t.Errorf("Failed to find node %v in schedulercache after UpdateNode.", node.Name)
		}
		if got.generation <= expected.generation {
			t.Errorf("generation is not incremented. got: %v, expected: %v", got.generation, expected.generation)
		}
		expected.generation = got.generation

		if !reflect.DeepEqual(got, expected) {
			t.Errorf("Failed to update node in schedulercache:\n got: %+v \nexpected: %+v", got, expected)
		}

		// Case 4: the node can not be removed if pods is not empty.
		cache.RemoveNode(node)
		if _, found := cache.nodes[node.Name]; !found {
			t.Errorf("The node %v should not be removed if pods is not empty.", node.Name)
		}
	}
}

func BenchmarkList1kNodes30kPods(b *testing.B) {
	cache := setupCacheOf1kNodes30kPods(b)
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		cache.List(labels.Everything())
	}
}

func BenchmarkUpdate1kNodes30kPods(b *testing.B) {
	// Enable volumesOnNodeForBalancing to do balanced resource allocation
	utilfeature.DefaultFeatureGate.Set(fmt.Sprintf("%s=true", features.BalanceAttachedNodeVolumes))
	cache := setupCacheOf1kNodes30kPods(b)
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		cachedNodes := map[string]*NodeInfo{}
		cache.UpdateNodeNameToInfoMap(cachedNodes)
	}
}

func BenchmarkExpire100Pods(b *testing.B) {
	benchmarkExpire(b, 100)
}

func BenchmarkExpire1kPods(b *testing.B) {
	benchmarkExpire(b, 1000)
}

func BenchmarkExpire10kPods(b *testing.B) {
	benchmarkExpire(b, 10000)
}

func benchmarkExpire(b *testing.B, podNum int) {
	now := time.Now()
	for n := 0; n < b.N; n++ {
		b.StopTimer()
		cache := setupCacheWithAssumedPods(b, podNum, now)
		b.StartTimer()
		cache.cleanupAssumedPods(now.Add(2 * time.Second))
	}
}

type testingMode interface {
	Fatalf(format string, args ...interface{})
}

func makeBasePod(t testingMode, nodeName, objName, cpu, mem, extended string, ports []v1.ContainerPort) *v1.Pod {
	req := v1.ResourceList{}
	if cpu != "" {
		req = v1.ResourceList{
			v1.ResourceCPU:    resource.MustParse(cpu),
			v1.ResourceMemory: resource.MustParse(mem),
		}
		if extended != "" {
			parts := strings.Split(extended, ":")
			if len(parts) != 2 {
				t.Fatalf("Invalid extended resource string: \"%s\"", extended)
			}
			req[v1.ResourceName(parts[0])] = resource.MustParse(parts[1])
		}
	}
	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			UID:       types.UID(objName),
			Namespace: "node_info_cache_test",
			Name:      objName,
		},
		Spec: v1.PodSpec{
			Containers: []v1.Container{{
				Resources: v1.ResourceRequirements{
					Requests: req,
				},
				Ports: ports,
			}},
			NodeName: nodeName,
		},
	}
}

func makeContainer(t testingMode, name, cpuReq, memReq, cpuLim, memLim string) v1.Container {
	req := v1.ResourceList{}
	lim := v1.ResourceList{}
	if cpuReq != "" {
		req[v1.ResourceCPU] = resource.MustParse(cpuReq)
	}
	if memReq != "" {
		req[v1.ResourceMemory] = resource.MustParse(memReq)
	}
	if cpuLim != "" {
		lim[v1.ResourceCPU] = resource.MustParse(cpuLim)
	}
	if memLim != "" {
		lim[v1.ResourceMemory] = resource.MustParse(memLim)
	}
	return v1.Container{
		Name:      name,
		Resources: v1.ResourceRequirements{
				Requests: req,
				Limits:   lim,
			},
		}
}

func setupCacheOf1kNodes30kPods(b *testing.B) Cache {
	cache := newSchedulerCache(time.Second, time.Second, nil)
	for i := 0; i < 1000; i++ {
		nodeName := fmt.Sprintf("node-%d", i)
		for j := 0; j < 30; j++ {
			objName := fmt.Sprintf("%s-pod-%d", nodeName, j)
			pod := makeBasePod(b, nodeName, objName, "0", "0", "", nil)

			if err := cache.AddPod(pod); err != nil {
				b.Fatalf("AddPod failed: %v", err)
			}
		}
	}
	return cache
}

func setupCacheWithAssumedPods(b *testing.B, podNum int, assumedTime time.Time) *schedulerCache {
	cache := newSchedulerCache(time.Second, time.Second, nil)
	for i := 0; i < podNum; i++ {
		nodeName := fmt.Sprintf("node-%d", i/10)
		objName := fmt.Sprintf("%s-pod-%d", nodeName, i%10)
		pod := makeBasePod(b, nodeName, objName, "0", "0", "", nil)

		err := assumeAndFinishBinding(cache, pod, assumedTime)
		if err != nil {
			b.Fatalf("assumePod failed: %v", err)
		}
	}
	return cache
}

func makePDB(name, namespace string, uid types.UID, labels map[string]string, minAvailable int) *v1beta1.PodDisruptionBudget {
	intstrMin := intstr.FromInt(minAvailable)
	pdb := &v1beta1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      name,
			Labels:    labels,
			UID:       uid,
		},
		Spec: v1beta1.PodDisruptionBudgetSpec{
			MinAvailable: &intstrMin,
			Selector:     &metav1.LabelSelector{MatchLabels: labels},
		},
	}

	return pdb
}

// TestPDBOperations tests that a PDB will be add/updated/deleted correctly.
func TestPDBOperations(t *testing.T) {
	ttl := 10 * time.Second
	testPDBs := []*v1beta1.PodDisruptionBudget{
		makePDB("pdb0", "ns1", "uid0", map[string]string{"tkey1": "tval1"}, 3),
		makePDB("pdb1", "ns1", "uid1", map[string]string{"tkey1": "tval1", "tkey2": "tval2"}, 1),
		makePDB("pdb2", "ns3", "uid2", map[string]string{"tkey3": "tval3", "tkey2": "tval2"}, 10),
	}
	updatedPDBs := []*v1beta1.PodDisruptionBudget{
		makePDB("pdb0", "ns1", "uid0", map[string]string{"tkey4": "tval4"}, 8),
		makePDB("pdb1", "ns1", "uid1", map[string]string{"tkey1": "tval1"}, 1),
		makePDB("pdb2", "ns3", "uid2", map[string]string{"tkey3": "tval3", "tkey1": "tval1", "tkey2": "tval2"}, 10),
	}
	tests := []struct {
		pdbsToAdd    []*v1beta1.PodDisruptionBudget
		pdbsToUpdate []*v1beta1.PodDisruptionBudget
		pdbsToDelete []*v1beta1.PodDisruptionBudget
		expectedPDBs []*v1beta1.PodDisruptionBudget // Expected PDBs after all operations
	}{
		{
			pdbsToAdd:    []*v1beta1.PodDisruptionBudget{testPDBs[0]},
			pdbsToUpdate: []*v1beta1.PodDisruptionBudget{testPDBs[0], testPDBs[1], testPDBs[0]},
			expectedPDBs: []*v1beta1.PodDisruptionBudget{testPDBs[0], testPDBs[1]}, // both will be in the cache as they have different names
		},
		{
			pdbsToAdd:    []*v1beta1.PodDisruptionBudget{testPDBs[0]},
			pdbsToUpdate: []*v1beta1.PodDisruptionBudget{testPDBs[0], updatedPDBs[0]},
			expectedPDBs: []*v1beta1.PodDisruptionBudget{updatedPDBs[0]},
		},
		{
			pdbsToAdd:    []*v1beta1.PodDisruptionBudget{testPDBs[0], testPDBs[2]},
			pdbsToUpdate: []*v1beta1.PodDisruptionBudget{testPDBs[0], updatedPDBs[0]},
			pdbsToDelete: []*v1beta1.PodDisruptionBudget{testPDBs[0]},
			expectedPDBs: []*v1beta1.PodDisruptionBudget{testPDBs[2]},
		},
	}

	for _, test := range tests {
		cache := newSchedulerCache(ttl, time.Second, nil)
		for _, pdbToAdd := range test.pdbsToAdd {
			if err := cache.AddPDB(pdbToAdd); err != nil {
				t.Fatalf("AddPDB failed: %v", err)
			}
		}

		for i := range test.pdbsToUpdate {
			if i == 0 {
				continue
			}
			if err := cache.UpdatePDB(test.pdbsToUpdate[i-1], test.pdbsToUpdate[i]); err != nil {
				t.Fatalf("UpdatePDB failed: %v", err)
			}
		}

		for _, pdb := range test.pdbsToDelete {
			if err := cache.RemovePDB(pdb); err != nil {
				t.Fatalf("RemovePDB failed: %v", err)
			}
		}

		cachedPDBs, err := cache.ListPDBs(labels.Everything())
		if err != nil {
			t.Fatalf("ListPDBs failed: %v", err)
		}
		if len(cachedPDBs) != len(test.expectedPDBs) {
			t.Errorf("Expected %d PDBs, got %d", len(test.expectedPDBs), len(cachedPDBs))
		}
		for _, pdb := range test.expectedPDBs {
			found := false
			// find it among the cached ones
			for _, cpdb := range cachedPDBs {
				if pdb.UID == cpdb.UID {
					found = true
					if !reflect.DeepEqual(pdb, cpdb) {
						t.Errorf("%v is not equal to %v", pdb, cpdb)
					}
					break
				}
			}
			if !found {
				t.Errorf("PDB with uid '%v' was not found in the cache.", pdb.UID)
			}

		}
	}
}

func TestIsUpToDate(t *testing.T) {
	cache := New(time.Duration(0), wait.NeverStop)
	if err := cache.AddNode(&v1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"}}); err != nil {
		t.Errorf("Could not add node: %v", err)
	}
	s := cache.Snapshot()
	node := s.Nodes["n1"]
	if !cache.IsUpToDate(node) {
		t.Errorf("Node incorrectly marked as stale")
	}
	pod := &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p1", UID: "p1"}, Spec: v1.PodSpec{NodeName: "n1"}}
	if err := cache.AddPod(pod); err != nil {
		t.Errorf("Could not add pod: %v", err)
	}
	if cache.IsUpToDate(node) {
		t.Errorf("Node incorrectly marked as up to date")
	}
	badNode := &NodeInfo{node: &v1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n2"}}}
	if cache.IsUpToDate(badNode) {
		t.Errorf("Nonexistant node incorrectly marked as up to date")
	}
}
