//go:build linux
// +build linux

/*
Copyright 2020 The Kubernetes Authors.

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

package nodeshutdown

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/client-go/tools/record"
	featuregatetesting "k8s.io/component-base/featuregate/testing"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/apis/scheduling"
	pkgfeatures "k8s.io/kubernetes/pkg/features"
	kubeletconfig "k8s.io/kubernetes/pkg/kubelet/apis/config"
	"k8s.io/kubernetes/pkg/kubelet/eviction"
	"k8s.io/kubernetes/pkg/kubelet/nodeshutdown/systemd"
	"k8s.io/kubernetes/pkg/kubelet/prober"
	probetest "k8s.io/kubernetes/pkg/kubelet/prober/testing"
	"k8s.io/utils/clock"
	testingclock "k8s.io/utils/clock/testing"
)

// lock is to prevent systemDbus from being modified in the case of concurrency.
var lock sync.Mutex

type fakeDbus struct {
	currentInhibitDelay        time.Duration
	overrideSystemInhibitDelay time.Duration
	shutdownChan               chan bool

	didInhibitShutdown      bool
	didOverrideInhibitDelay bool
}

func (f *fakeDbus) CurrentInhibitDelay() (time.Duration, error) {
	if f.didOverrideInhibitDelay {
		return f.overrideSystemInhibitDelay, nil
	}
	return f.currentInhibitDelay, nil
}

func (f *fakeDbus) InhibitShutdown() (systemd.InhibitLock, error) {
	f.didInhibitShutdown = true
	return systemd.InhibitLock(0), nil
}

func (f *fakeDbus) ReleaseInhibitLock(lock systemd.InhibitLock) error {
	return nil
}

func (f *fakeDbus) ReloadLogindConf() error {
	return nil
}

func (f *fakeDbus) MonitorShutdown() (<-chan bool, error) {
	return f.shutdownChan, nil
}

func (f *fakeDbus) OverrideInhibitDelay(inhibitDelayMax time.Duration) error {
	f.didOverrideInhibitDelay = true
	return nil
}

func makePod(name string, priority int32, terminationGracePeriod *int64) *v1.Pod {
	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			UID:  types.UID(name),
		},
		Spec: v1.PodSpec{
			Priority:                      &priority,
			TerminationGracePeriodSeconds: terminationGracePeriod,
		},
	}
}

func TestManager(t *testing.T) {
	systemDbusTmp := systemDbus
	defer func() {
		systemDbus = systemDbusTmp
	}()
	normalPodNoGracePeriod := makePod("normal-pod-nil-grace-period", scheduling.DefaultPriorityWhenNoDefaultClassExists, nil /* terminationGracePeriod */)
	criticalPodNoGracePeriod := makePod("critical-pod-nil-grace-period", scheduling.SystemCriticalPriority, nil /* terminationGracePeriod */)

	shortGracePeriod := int64(2)
	normalPodGracePeriod := makePod("normal-pod-grace-period", scheduling.DefaultPriorityWhenNoDefaultClassExists, &shortGracePeriod /* terminationGracePeriod */)
	criticalPodGracePeriod := makePod("critical-pod-grace-period", scheduling.SystemCriticalPriority, &shortGracePeriod /* terminationGracePeriod */)

	longGracePeriod := int64(1000)
	normalPodLongGracePeriod := makePod("normal-pod-long-grace-period", scheduling.DefaultPriorityWhenNoDefaultClassExists, &longGracePeriod /* terminationGracePeriod */)

	var tests = []struct {
		desc                             string
		activePods                       []*v1.Pod
		shutdownGracePeriodRequested     time.Duration
		shutdownGracePeriodCriticalPods  time.Duration
		systemInhibitDelay               time.Duration
		overrideSystemInhibitDelay       time.Duration
		expectedDidOverrideInhibitDelay  bool
		expectedPodToGracePeriodOverride map[string]int64
		expectedError                    error
	}{
		{
			desc:                             "no override (total=30s, critical=10s)",
			activePods:                       []*v1.Pod{normalPodNoGracePeriod, criticalPodNoGracePeriod},
			shutdownGracePeriodRequested:     time.Duration(30 * time.Second),
			shutdownGracePeriodCriticalPods:  time.Duration(10 * time.Second),
			systemInhibitDelay:               time.Duration(40 * time.Second),
			overrideSystemInhibitDelay:       time.Duration(40 * time.Second),
			expectedDidOverrideInhibitDelay:  false,
			expectedPodToGracePeriodOverride: map[string]int64{"normal-pod-nil-grace-period": 20, "critical-pod-nil-grace-period": 10},
		},
		{
			desc:                             "no override (total=30s, critical=10s) pods with terminationGracePeriod and without",
			activePods:                       []*v1.Pod{normalPodNoGracePeriod, criticalPodNoGracePeriod, normalPodGracePeriod, criticalPodGracePeriod},
			shutdownGracePeriodRequested:     time.Duration(30 * time.Second),
			shutdownGracePeriodCriticalPods:  time.Duration(10 * time.Second),
			systemInhibitDelay:               time.Duration(40 * time.Second),
			overrideSystemInhibitDelay:       time.Duration(40 * time.Second),
			expectedDidOverrideInhibitDelay:  false,
			expectedPodToGracePeriodOverride: map[string]int64{"normal-pod-nil-grace-period": 20, "critical-pod-nil-grace-period": 10, "normal-pod-grace-period": 2, "critical-pod-grace-period": 2},
		},
		{
			desc:                             "no override (total=30s, critical=10s) pod with long terminationGracePeriod is overridden",
			activePods:                       []*v1.Pod{normalPodNoGracePeriod, criticalPodNoGracePeriod, normalPodGracePeriod, criticalPodGracePeriod, normalPodLongGracePeriod},
			shutdownGracePeriodRequested:     time.Duration(30 * time.Second),
			shutdownGracePeriodCriticalPods:  time.Duration(10 * time.Second),
			systemInhibitDelay:               time.Duration(40 * time.Second),
			overrideSystemInhibitDelay:       time.Duration(40 * time.Second),
			expectedDidOverrideInhibitDelay:  false,
			expectedPodToGracePeriodOverride: map[string]int64{"normal-pod-nil-grace-period": 20, "critical-pod-nil-grace-period": 10, "normal-pod-grace-period": 2, "critical-pod-grace-period": 2, "normal-pod-long-grace-period": 20},
		},
		{
			desc:                             "no override (total=30, critical=0)",
			activePods:                       []*v1.Pod{normalPodNoGracePeriod, criticalPodNoGracePeriod},
			shutdownGracePeriodRequested:     time.Duration(30 * time.Second),
			shutdownGracePeriodCriticalPods:  time.Duration(0 * time.Second),
			systemInhibitDelay:               time.Duration(40 * time.Second),
			overrideSystemInhibitDelay:       time.Duration(40 * time.Second),
			expectedDidOverrideInhibitDelay:  false,
			expectedPodToGracePeriodOverride: map[string]int64{"normal-pod-nil-grace-period": 30, "critical-pod-nil-grace-period": 0},
		},
		{
			desc:                             "override successful (total=30, critical=10)",
			activePods:                       []*v1.Pod{normalPodNoGracePeriod, criticalPodNoGracePeriod},
			shutdownGracePeriodRequested:     time.Duration(30 * time.Second),
			shutdownGracePeriodCriticalPods:  time.Duration(10 * time.Second),
			systemInhibitDelay:               time.Duration(5 * time.Second),
			overrideSystemInhibitDelay:       time.Duration(30 * time.Second),
			expectedDidOverrideInhibitDelay:  true,
			expectedPodToGracePeriodOverride: map[string]int64{"normal-pod-nil-grace-period": 20, "critical-pod-nil-grace-period": 10},
		},
		{
			desc:                             "override unsuccessful",
			activePods:                       []*v1.Pod{normalPodNoGracePeriod, criticalPodNoGracePeriod},
			shutdownGracePeriodRequested:     time.Duration(30 * time.Second),
			shutdownGracePeriodCriticalPods:  time.Duration(10 * time.Second),
			systemInhibitDelay:               time.Duration(5 * time.Second),
			overrideSystemInhibitDelay:       time.Duration(5 * time.Second),
			expectedDidOverrideInhibitDelay:  true,
			expectedPodToGracePeriodOverride: map[string]int64{"normal-pod-nil-grace-period": 5, "critical-pod-nil-grace-period": 0},
			expectedError:                    fmt.Errorf("unable to update logind InhibitDelayMaxSec to 30s (ShutdownGracePeriod), current value of InhibitDelayMaxSec (5s) is less than requested ShutdownGracePeriod"),
		},
		{
			desc:                            "override unsuccessful, zero time",
			activePods:                      []*v1.Pod{normalPodNoGracePeriod, criticalPodNoGracePeriod},
			shutdownGracePeriodRequested:    time.Duration(5 * time.Second),
			shutdownGracePeriodCriticalPods: time.Duration(5 * time.Second),
			systemInhibitDelay:              time.Duration(0 * time.Second),
			overrideSystemInhibitDelay:      time.Duration(0 * time.Second),
			expectedError:                   fmt.Errorf("unable to update logind InhibitDelayMaxSec to 5s (ShutdownGracePeriod), current value of InhibitDelayMaxSec (0s) is less than requested ShutdownGracePeriod"),
		},
		{
			desc:                             "no override, all time to critical pods",
			activePods:                       []*v1.Pod{normalPodNoGracePeriod, criticalPodNoGracePeriod},
			shutdownGracePeriodRequested:     time.Duration(5 * time.Second),
			shutdownGracePeriodCriticalPods:  time.Duration(5 * time.Second),
			systemInhibitDelay:               time.Duration(5 * time.Second),
			overrideSystemInhibitDelay:       time.Duration(5 * time.Second),
			expectedDidOverrideInhibitDelay:  false,
			expectedPodToGracePeriodOverride: map[string]int64{"normal-pod-nil-grace-period": 0, "critical-pod-nil-grace-period": 5},
		},
	}

	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			activePodsFunc := func() []*v1.Pod {
				return tc.activePods
			}

			type PodKillInfo struct {
				Name        string
				GracePeriod int64
			}

			podKillChan := make(chan PodKillInfo, 1)
			killPodsFunc := func(pod *v1.Pod, evict bool, gracePeriodOverride *int64, fn func(podStatus *v1.PodStatus)) error {
				var gracePeriod int64
				if gracePeriodOverride != nil {
					gracePeriod = *gracePeriodOverride
				}
				podKillChan <- PodKillInfo{Name: pod.Name, GracePeriod: gracePeriod}
				return nil
			}

			fakeShutdownChan := make(chan bool)
			fakeDbus := &fakeDbus{currentInhibitDelay: tc.systemInhibitDelay, shutdownChan: fakeShutdownChan, overrideSystemInhibitDelay: tc.overrideSystemInhibitDelay}

			lock.Lock()
			systemDbus = func() (dbusInhibiter, error) {
				return fakeDbus, nil
			}
			defer featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, pkgfeatures.GracefulNodeShutdown, true)()

			proberManager := probetest.FakeManager{}
			fakeRecorder := &record.FakeRecorder{}
			nodeRef := &v1.ObjectReference{Kind: "Node", Name: "test", UID: types.UID("test"), Namespace: ""}
			manager, _ := NewManager(&Config{
				ProbeManager:                    proberManager,
				Recorder:                        fakeRecorder,
				NodeRef:                         nodeRef,
				GetPodsFunc:                     activePodsFunc,
				KillPodFunc:                     killPodsFunc,
				SyncNodeStatusFunc:              func() {},
				ShutdownGracePeriodRequested:    tc.shutdownGracePeriodRequested,
				ShutdownGracePeriodCriticalPods: tc.shutdownGracePeriodCriticalPods,
				Clock:                           testingclock.NewFakeClock(time.Now()),
				StateDirectory:                  os.TempDir(),
			})

			err := manager.Start()
			lock.Unlock()

			if tc.expectedError != nil {
				if err == nil {
					t.Errorf("unexpected error message. Got: <nil> want %s", tc.expectedError.Error())
				} else if !strings.Contains(err.Error(), tc.expectedError.Error()) {
					t.Errorf("unexpected error message. Got: %s want %s", err.Error(), tc.expectedError.Error())
				}
			} else {
				assert.NoError(t, err, "expected manager.Start() to not return error")
				assert.True(t, fakeDbus.didInhibitShutdown, "expected that manager inhibited shutdown")
				assert.NoError(t, manager.ShutdownStatus(), "expected that manager does not return error since shutdown is not active")
				assert.Equal(t, manager.Admit(nil).Admit, true)

				// Send fake shutdown event
				select {
				case fakeShutdownChan <- true:
				case <-time.After(1 * time.Second):
					t.Fatal()
				}

				// Wait for all the pods to be killed
				killedPodsToGracePeriods := map[string]int64{}
				for i := 0; i < len(tc.activePods); i++ {
					select {
					case podKillInfo := <-podKillChan:
						killedPodsToGracePeriods[podKillInfo.Name] = podKillInfo.GracePeriod
						continue
					case <-time.After(1 * time.Second):
						t.Fatal()
					}
				}

				assert.Error(t, manager.ShutdownStatus(), "expected that manager returns error since shutdown is active")
				assert.Equal(t, manager.Admit(nil).Admit, false)
				assert.Equal(t, tc.expectedPodToGracePeriodOverride, killedPodsToGracePeriods)
				assert.Equal(t, tc.expectedDidOverrideInhibitDelay, fakeDbus.didOverrideInhibitDelay, "override system inhibit delay differs")
			}
		})
	}
}

func TestFeatureEnabled(t *testing.T) {
	var tests = []struct {
		desc                         string
		shutdownGracePeriodRequested time.Duration
		featureGateEnabled           bool
		expectEnabled                bool
	}{
		{
			desc:                         "shutdownGracePeriodRequested 0; disables feature",
			shutdownGracePeriodRequested: time.Duration(0 * time.Second),
			featureGateEnabled:           true,
			expectEnabled:                false,
		},
		{
			desc:                         "feature gate disabled; disables feature",
			shutdownGracePeriodRequested: time.Duration(100 * time.Second),
			featureGateEnabled:           false,
			expectEnabled:                false,
		},
		{
			desc:                         "feature gate enabled; shutdownGracePeriodRequested > 0; enables feature",
			shutdownGracePeriodRequested: time.Duration(100 * time.Second),
			featureGateEnabled:           true,
			expectEnabled:                true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			activePodsFunc := func() []*v1.Pod {
				return nil
			}
			killPodsFunc := func(pod *v1.Pod, evict bool, gracePeriodOverride *int64, fn func(*v1.PodStatus)) error {
				return nil
			}
			defer featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, pkgfeatures.GracefulNodeShutdown, tc.featureGateEnabled)()

			proberManager := probetest.FakeManager{}
			fakeRecorder := &record.FakeRecorder{}
			nodeRef := &v1.ObjectReference{Kind: "Node", Name: "test", UID: types.UID("test"), Namespace: ""}

			manager, _ := NewManager(&Config{
				ProbeManager:                    proberManager,
				Recorder:                        fakeRecorder,
				NodeRef:                         nodeRef,
				GetPodsFunc:                     activePodsFunc,
				KillPodFunc:                     killPodsFunc,
				SyncNodeStatusFunc:              func() {},
				ShutdownGracePeriodRequested:    tc.shutdownGracePeriodRequested,
				ShutdownGracePeriodCriticalPods: 0,
				StateDirectory:                  os.TempDir(),
			})
			assert.Equal(t, tc.expectEnabled, manager != managerStub{})
		})
	}
}

func TestRestart(t *testing.T) {
	systemDbusTmp := systemDbus
	defer func() {
		systemDbus = systemDbusTmp
	}()

	shutdownGracePeriodRequested := 30 * time.Second
	shutdownGracePeriodCriticalPods := 10 * time.Second
	systemInhibitDelay := 40 * time.Second
	overrideSystemInhibitDelay := 40 * time.Second
	activePodsFunc := func() []*v1.Pod {
		return nil
	}
	killPodsFunc := func(pod *v1.Pod, isEvicted bool, gracePeriodOverride *int64, fn func(*v1.PodStatus)) error {
		return nil
	}
	syncNodeStatus := func() {}

	var shutdownChan chan bool
	var shutdownChanMut sync.Mutex
	var connChan = make(chan struct{}, 1)

	lock.Lock()
	systemDbus = func() (dbusInhibiter, error) {
		defer func() {
			connChan <- struct{}{}
		}()
		ch := make(chan bool)
		shutdownChanMut.Lock()
		shutdownChan = ch
		shutdownChanMut.Unlock()
		dbus := &fakeDbus{currentInhibitDelay: systemInhibitDelay, shutdownChan: ch, overrideSystemInhibitDelay: overrideSystemInhibitDelay}
		return dbus, nil
	}

	proberManager := probetest.FakeManager{}
	fakeRecorder := &record.FakeRecorder{}
	nodeRef := &v1.ObjectReference{Kind: "Node", Name: "test", UID: types.UID("test"), Namespace: ""}
	manager, _ := NewManager(&Config{
		ProbeManager:                    proberManager,
		Recorder:                        fakeRecorder,
		NodeRef:                         nodeRef,
		GetPodsFunc:                     activePodsFunc,
		KillPodFunc:                     killPodsFunc,
		SyncNodeStatusFunc:              syncNodeStatus,
		ShutdownGracePeriodRequested:    shutdownGracePeriodRequested,
		ShutdownGracePeriodCriticalPods: shutdownGracePeriodCriticalPods,
		StateDirectory:                  os.TempDir(),
	})

	err := manager.Start()
	lock.Unlock()

	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}

	for i := 0; i != 3; i++ {
		select {
		case <-time.After(dbusReconnectPeriod * 5):
			t.Fatal("wait dbus connect timeout")
		case <-connChan:
		}

		shutdownChanMut.Lock()
		close(shutdownChan)
		shutdownChanMut.Unlock()
	}
}

func Test_migrateConfig(t *testing.T) {
	type shutdownConfig struct {
		shutdownGracePeriodRequested    time.Duration
		shutdownGracePeriodCriticalPods time.Duration
	}
	tests := []struct {
		name string
		args shutdownConfig
		want []kubeletconfig.ShutdownGracePeriodByPodPriority
	}{
		{
			name: "both shutdownGracePeriodRequested and shutdownGracePeriodCriticalPods",
			args: shutdownConfig{
				shutdownGracePeriodRequested:    300 * time.Second,
				shutdownGracePeriodCriticalPods: 120 * time.Second,
			},
			want: []kubeletconfig.ShutdownGracePeriodByPodPriority{
				{
					Priority:                   scheduling.DefaultPriorityWhenNoDefaultClassExists,
					ShutdownGracePeriodSeconds: 180,
				},
				{
					Priority:                   scheduling.SystemCriticalPriority,
					ShutdownGracePeriodSeconds: 120,
				},
			},
		},
		{
			name: "only shutdownGracePeriodRequested",
			args: shutdownConfig{
				shutdownGracePeriodRequested:    100 * time.Second,
				shutdownGracePeriodCriticalPods: 0 * time.Second,
			},
			want: []kubeletconfig.ShutdownGracePeriodByPodPriority{
				{
					Priority:                   scheduling.DefaultPriorityWhenNoDefaultClassExists,
					ShutdownGracePeriodSeconds: 100,
				},
				{
					Priority:                   scheduling.SystemCriticalPriority,
					ShutdownGracePeriodSeconds: 0,
				},
			},
		},
		{
			name: "empty configuration",
			args: shutdownConfig{
				shutdownGracePeriodRequested:    0 * time.Second,
				shutdownGracePeriodCriticalPods: 0 * time.Second,
			},
			want: nil,
		},
		{
			name: "wrong configuration",
			args: shutdownConfig{
				shutdownGracePeriodRequested:    1 * time.Second,
				shutdownGracePeriodCriticalPods: 100 * time.Second,
			},
			want: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := migrateConfig(tt.args.shutdownGracePeriodRequested, tt.args.shutdownGracePeriodCriticalPods); !assert.Equal(t, tt.want, got) {
				t.Errorf("migrateConfig() = %v, want %v", got, tt.want)
			}
		})
	}
}

func Test_groupByPriority(t *testing.T) {
	type args struct {
		shutdownGracePeriodByPodPriority []kubeletconfig.ShutdownGracePeriodByPodPriority
		pods                             []*v1.Pod
	}
	tests := []struct {
		name string
		args args
		want []podShutdownGroup
	}{
		{
			name: "migrate config",
			args: args{
				shutdownGracePeriodByPodPriority: migrateConfig(300*time.Second /* shutdownGracePeriodRequested */, 120*time.Second /* shutdownGracePeriodCriticalPods */),
				pods: []*v1.Pod{
					makePod("normal-pod", scheduling.DefaultPriorityWhenNoDefaultClassExists, nil),
					makePod("highest-user-definable-pod", scheduling.HighestUserDefinablePriority, nil),
					makePod("critical-pod", scheduling.SystemCriticalPriority, nil),
				},
			},
			want: []podShutdownGroup{
				{
					ShutdownGracePeriodByPodPriority: kubeletconfig.ShutdownGracePeriodByPodPriority{
						Priority:                   scheduling.DefaultPriorityWhenNoDefaultClassExists,
						ShutdownGracePeriodSeconds: 180,
					},
					Pods: []*v1.Pod{
						makePod("normal-pod", scheduling.DefaultPriorityWhenNoDefaultClassExists, nil),
						makePod("highest-user-definable-pod", scheduling.HighestUserDefinablePriority, nil),
					},
				},
				{
					ShutdownGracePeriodByPodPriority: kubeletconfig.ShutdownGracePeriodByPodPriority{
						Priority:                   scheduling.SystemCriticalPriority,
						ShutdownGracePeriodSeconds: 120,
					},
					Pods: []*v1.Pod{
						makePod("critical-pod", scheduling.SystemCriticalPriority, nil),
					},
				},
			},
		},
		{
			name: "pod priority",
			args: args{
				shutdownGracePeriodByPodPriority: []kubeletconfig.ShutdownGracePeriodByPodPriority{
					{
						Priority:                   1,
						ShutdownGracePeriodSeconds: 10,
					},
					{
						Priority:                   2,
						ShutdownGracePeriodSeconds: 20,
					},
					{
						Priority:                   3,
						ShutdownGracePeriodSeconds: 30,
					},
					{
						Priority:                   4,
						ShutdownGracePeriodSeconds: 40,
					},
				},
				pods: []*v1.Pod{
					makePod("pod-0", 0, nil),
					makePod("pod-1", 1, nil),
					makePod("pod-2", 2, nil),
					makePod("pod-3", 3, nil),
					makePod("pod-4", 4, nil),
					makePod("pod-5", 5, nil),
				},
			},
			want: []podShutdownGroup{
				{
					ShutdownGracePeriodByPodPriority: kubeletconfig.ShutdownGracePeriodByPodPriority{
						Priority:                   1,
						ShutdownGracePeriodSeconds: 10,
					},
					Pods: []*v1.Pod{
						makePod("pod-0", 0, nil),
						makePod("pod-1", 1, nil),
					},
				},
				{
					ShutdownGracePeriodByPodPriority: kubeletconfig.ShutdownGracePeriodByPodPriority{
						Priority:                   2,
						ShutdownGracePeriodSeconds: 20,
					},
					Pods: []*v1.Pod{
						makePod("pod-2", 2, nil),
					},
				},
				{
					ShutdownGracePeriodByPodPriority: kubeletconfig.ShutdownGracePeriodByPodPriority{
						Priority:                   3,
						ShutdownGracePeriodSeconds: 30,
					},
					Pods: []*v1.Pod{
						makePod("pod-3", 3, nil),
					},
				},
				{
					ShutdownGracePeriodByPodPriority: kubeletconfig.ShutdownGracePeriodByPodPriority{
						Priority:                   4,
						ShutdownGracePeriodSeconds: 40,
					},
					Pods: []*v1.Pod{
						makePod("pod-4", 4, nil),
						makePod("pod-5", 5, nil),
					},
				},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := groupByPriority(tt.args.shutdownGracePeriodByPodPriority, tt.args.pods); !assert.Equal(t, tt.want, got) {
				t.Errorf("groupByPriority() = %v, want %v", got, tt.want)
			}
		})
	}
}

func Test_managerImpl_processShutdownEvent(t *testing.T) {
	var (
		probeManager   = probetest.FakeManager{}
		fakeRecorder   = &record.FakeRecorder{}
		syncNodeStatus = func() {}
		nodeRef        = &v1.ObjectReference{Kind: "Node", Name: "test", UID: types.UID("test"), Namespace: ""}
		fakeclock      = testingclock.NewFakeClock(time.Now())
	)

	type fields struct {
		recorder                         record.EventRecorder
		nodeRef                          *v1.ObjectReference
		probeManager                     prober.Manager
		shutdownGracePeriodByPodPriority []kubeletconfig.ShutdownGracePeriodByPodPriority
		getPods                          eviction.ActivePodsFunc
		killPodFunc                      eviction.KillPodFunc
		syncNodeStatus                   func()
		dbusCon                          dbusInhibiter
		inhibitLock                      systemd.InhibitLock
		nodeShuttingDownNow              bool
		clock                            clock.Clock
	}
	tests := []struct {
		name                 string
		fields               fields
		wantErr              bool
		exceptOutputContains string
	}{
		{
			name: "kill pod func take too long",
			fields: fields{
				recorder:     fakeRecorder,
				nodeRef:      nodeRef,
				probeManager: probeManager,
				shutdownGracePeriodByPodPriority: []kubeletconfig.ShutdownGracePeriodByPodPriority{
					{
						Priority:                   1,
						ShutdownGracePeriodSeconds: 10,
					},
					{
						Priority:                   2,
						ShutdownGracePeriodSeconds: 20,
					},
				},
				getPods: func() []*v1.Pod {
					return []*v1.Pod{
						makePod("normal-pod", 1, nil),
						makePod("critical-pod", 2, nil),
					}
				},
				killPodFunc: func(pod *v1.Pod, isEvicted bool, gracePeriodOverride *int64, fn func(*v1.PodStatus)) error {
					fakeclock.Step(60 * time.Second)
					return nil
				},
				syncNodeStatus: syncNodeStatus,
				clock:          fakeclock,
				dbusCon:        &fakeDbus{},
			},
			wantErr:              false,
			exceptOutputContains: "Shutdown manager pod killing time out",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			l := klog.Level(1)
			l.Set("1")
			// hijack the klog output
			tmpWriteBuffer := bytes.NewBuffer(nil)
			klog.SetOutput(tmpWriteBuffer)
			klog.LogToStderr(false)

			m := &managerImpl{
				recorder:                         tt.fields.recorder,
				nodeRef:                          tt.fields.nodeRef,
				probeManager:                     tt.fields.probeManager,
				shutdownGracePeriodByPodPriority: tt.fields.shutdownGracePeriodByPodPriority,
				getPods:                          tt.fields.getPods,
				killPodFunc:                      tt.fields.killPodFunc,
				syncNodeStatus:                   tt.fields.syncNodeStatus,
				dbusCon:                          tt.fields.dbusCon,
				inhibitLock:                      tt.fields.inhibitLock,
				nodeShuttingDownMutex:            sync.Mutex{},
				nodeShuttingDownNow:              tt.fields.nodeShuttingDownNow,
				clock:                            tt.fields.clock,
			}
			if err := m.processShutdownEvent(); (err != nil) != tt.wantErr {
				t.Errorf("managerImpl.processShutdownEvent() error = %v, wantErr %v", err, tt.wantErr)
			}
			klog.Flush()

			log := tmpWriteBuffer.String()
			if !strings.Contains(log, tt.exceptOutputContains) {
				t.Errorf("managerImpl.processShutdownEvent() should log %s, got %s", tt.exceptOutputContains, log)
			}
		})
	}
}
