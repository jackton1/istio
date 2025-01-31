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

package leaderelection

import (
	"context"
	"fmt"
	"testing"
	"time"

	"go.uber.org/atomic"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"istio.io/istio/pkg/revisions"
	"istio.io/istio/pkg/test/util/retry"
)

const testLock = "test-lock"

func createElection(t *testing.T, name string, revision string, watcher revisions.DefaultWatcher, expectLeader bool, client kubernetes.Interface,
	fns ...func(stop <-chan struct{})) (*LeaderElection, chan struct{}) {
	t.Helper()
	l := &LeaderElection{
		namespace:      "ns",
		name:           name,
		electionID:     testLock,
		client:         client,
		revision:       revision,
		defaultWatcher: watcher,
		ttl:            time.Second,
		cycle:          atomic.NewInt32(0),
	}
	gotLeader := make(chan struct{})
	l.AddRunFunction(func(stop <-chan struct{}) {
		gotLeader <- struct{}{}
	})
	for _, fn := range fns {
		l.AddRunFunction(fn)
	}
	stop := make(chan struct{})
	go l.Run(stop)

	if expectLeader {
		select {
		case <-gotLeader:
		case <-time.After(time.Second * 15):
			t.Fatalf("(pod %s, revision: %s) failed to acquire lease", name, revision)
		}
	} else {
		select {
		case <-gotLeader:
			t.Fatalf("(pod %s, revision: %s) unexpectedly acquired lease", name, revision)
		case <-time.After(time.Second * 1):
		}
	}
	return l, stop
}

type fakeDefaultWatcher struct {
	defaultRevision string
}

func (w *fakeDefaultWatcher) setDefaultRevision(r string) {
	w.defaultRevision = r
}

func (w *fakeDefaultWatcher) GetDefault() string {
	return w.defaultRevision
}

func (w *fakeDefaultWatcher) AddHandler(handler revisions.DefaultHandler) {
	panic("unimplemented")
}

func TestLeaderElection(t *testing.T) {
	client := fake.NewSimpleClientset()
	watcher := &fakeDefaultWatcher{}
	// First pod becomes the leader
	_, stop := createElection(t, "pod1", "", watcher, true, client)
	// A new pod is not the leader
	_, stop2 := createElection(t, "pod2", "", watcher, false, client)
	close(stop2)
	close(stop)
}

func TestPrioritizedLeaderElection(t *testing.T) {
	client := fake.NewSimpleClientset()
	watcher := &fakeDefaultWatcher{defaultRevision: "red"}

	// First pod, revision "green" becomes the leader, but is not the default revision
	_, stop := createElection(t, "pod1", "green", watcher, true, client)
	// Second pod, revision "red", steals the leader lock from "green" since it is the default revision
	_, stop2 := createElection(t, "pod2", "red", watcher, true, client)
	// Third pod with revision "red" comes in and cannot take the lock since another revision with "red" has it
	_, stop3 := createElection(t, "pod3", "red", watcher, false, client)
	// Fourth pod with revision "green" cannot take the lock since a revision with "red" has it.
	_, stop4 := createElection(t, "pod4", "green", watcher, false, client)
	close(stop2)
	close(stop3)
	close(stop4)
	// Now that revision "green" has stopped acting as leader, revision "red" should be able to claim lock.
	_, stop5 := createElection(t, "pod2", "red", watcher, true, client)
	close(stop5)
	close(stop)
	// Revision "green" can reclaim once "red" releases.
	_, stop6 := createElection(t, "pod4", "green", watcher, true, client)
	close(stop6)
}

func TestLeaderElectionConfigMapRemoved(t *testing.T) {
	client := fake.NewSimpleClientset()
	watcher := &fakeDefaultWatcher{}
	_, stop := createElection(t, "pod1", "", watcher, true, client)
	if err := client.CoreV1().ConfigMaps("ns").Delete(context.TODO(), testLock, v1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	retry.UntilSuccessOrFail(t, func() error {
		l, err := client.CoreV1().ConfigMaps("ns").List(context.TODO(), v1.ListOptions{})
		if err != nil {
			return err
		}
		if len(l.Items) != 1 {
			return fmt.Errorf("got unexpected config map entry: %v", l.Items)
		}
		return nil
	})
	close(stop)
}

func TestLeaderElectionNoPermission(t *testing.T) {
	client := fake.NewSimpleClientset()
	watcher := &fakeDefaultWatcher{}
	allowRbac := atomic.NewBool(true)
	client.Fake.PrependReactor("update", "*", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if allowRbac.Load() {
			return false, nil, nil
		}
		return true, nil, fmt.Errorf("nope, out of luck")
	})

	completions := atomic.NewInt32(0)
	l, stop := createElection(t, "pod1", "", watcher, true, client, func(stop <-chan struct{}) {
		completions.Add(1)
	})
	// Expect to run once
	expectInt(t, completions.Load, 1)

	// drop RBAC permssions to update the configmap
	// This simulates loosing an active lease
	allowRbac.Store(false)

	// We should start a new cycle at this point
	expectInt(t, l.cycle.Load, 2)

	// Add configmap permission back
	allowRbac.Store(true)

	// We should get the leader lock back
	expectInt(t, completions.Load, 2)

	close(stop)
}

func expectInt(t *testing.T, f func() int32, expected int32) {
	t.Helper()
	retry.UntilSuccessOrFail(t, func() error {
		got := f()
		if got != expected {
			return fmt.Errorf("unexpected count: %v, want %v", got, expected)
		}
		return nil
	}, retry.Timeout(time.Second))
}
