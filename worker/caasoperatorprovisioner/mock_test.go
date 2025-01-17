// Copyright 2017 Canonical Ltd.
// Licensed under the AGPLv3, see LICENCE file for details.

package caasoperatorprovisioner_test

import (
	"sync"

	"github.com/juju/juju/storage"
	"github.com/juju/testing"
	"github.com/juju/version"
	"gopkg.in/juju/names.v3"
	"gopkg.in/tomb.v2"

	"github.com/juju/juju/agent"
	apicaasprovisioner "github.com/juju/juju/api/caasoperatorprovisioner"
	"github.com/juju/juju/apiserver/params"
	"github.com/juju/juju/caas"
	"github.com/juju/juju/core/life"
	"github.com/juju/juju/core/watcher"
	coretesting "github.com/juju/juju/testing"
	"github.com/juju/juju/worker/caasoperatorprovisioner"
)

type mockProvisionerFacade struct {
	mu   sync.Mutex
	stub *testing.Stub
	caasoperatorprovisioner.CAASProvisionerFacade
	applicationsWatcher *mockStringsWatcher
	apiWatcher          *mockNotifyWatcher
	life                life.Value
}

func newMockProvisionerFacade(stub *testing.Stub) *mockProvisionerFacade {
	return &mockProvisionerFacade{
		stub:                stub,
		applicationsWatcher: newMockStringsWatcher(),
		apiWatcher:          newMockNotifyWatcher(),
	}
}

func (m *mockProvisionerFacade) WatchApplications() (watcher.StringsWatcher, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stub.MethodCall(m, "WatchApplications")
	if err := m.stub.NextErr(); err != nil {
		return nil, err
	}
	return m.applicationsWatcher, nil
}

func (m *mockProvisionerFacade) OperatorProvisioningInfo() (apicaasprovisioner.OperatorProvisioningInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stub.MethodCall(m, "OperatorProvisioningInfo")
	if err := m.stub.NextErr(); err != nil {
		return apicaasprovisioner.OperatorProvisioningInfo{}, err
	}
	return apicaasprovisioner.OperatorProvisioningInfo{
		ImagePath:    "juju-operator-image",
		Version:      version.MustParse("2.99.0"),
		APIAddresses: []string{"10.0.0.1:17070", "192.18.1.1:17070"},
		Tags:         map[string]string{"fred": "mary"},
		CharmStorage: storage.KubernetesFilesystemParams{
			Provider:     "kubernetes",
			Size:         uint64(1024),
			ResourceTags: map[string]string{"foo": "bar"},
			Attributes:   map[string]interface{}{"key": "value"},
		},
	}, nil
}

func (m *mockProvisionerFacade) Life(entityName string) (life.Value, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stub.MethodCall(m, "Life", entityName)
	if err := m.stub.NextErr(); err != nil {
		return "", err
	}
	return m.life, nil
}

func (m *mockProvisionerFacade) SetPasswords(passwords []apicaasprovisioner.ApplicationPassword) (params.ErrorResults, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stub.MethodCall(m, "SetPasswords", passwords)
	if err := m.stub.NextErr(); err != nil {
		return params.ErrorResults{}, err
	}
	return params.ErrorResults{
		Results: make([]params.ErrorResult, len(passwords)),
	}, nil
}

type mockAgentConfig struct {
	agent.Config
}

func (m *mockAgentConfig) Controller() names.ControllerTag {
	return coretesting.ControllerTag
}

func (m *mockAgentConfig) DataDir() string {
	return "/var/lib/juju"
}

func (m *mockAgentConfig) LogDir() string {
	return "/var/log/juju"
}

func (m *mockAgentConfig) OldPassword() string {
	return "old password"
}

func (m *mockAgentConfig) CACert() string {
	return coretesting.CACert
}

type mockBroker struct {
	testing.Stub
	caas.Broker

	mu             sync.Mutex
	terminating    bool
	operatorExists bool
}

func (m *mockBroker) setTerminating(terminating bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.terminating = terminating
}

func (m *mockBroker) setOperatorExists(operatorExists bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.operatorExists = operatorExists
}

func (m *mockBroker) EnsureOperator(appName, agentPath string, config *caas.OperatorConfig) error {
	m.MethodCall(m, "EnsureOperator", appName, agentPath, config)
	return m.NextErr()
}

func (m *mockBroker) OperatorExists(appName string) (caas.OperatorState, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.MethodCall(m, "OperatorExists", appName)
	return caas.OperatorState{Exists: m.operatorExists, Terminating: m.terminating}, m.NextErr()
}

func (m *mockBroker) DeleteOperator(appName string) error {
	m.MethodCall(m, "DeleteOperator", appName)
	return m.NextErr()
}

type mockWatcher struct {
	testing.Stub
	tomb.Tomb
	mu         sync.Mutex
	terminated bool
}

func (w *mockWatcher) killed() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.terminated
}

func (w *mockWatcher) Kill() {
	w.MethodCall(w, "Kill")
	w.Tomb.Kill(nil)
	w.mu.Lock()
	defer w.mu.Unlock()
	w.terminated = true
}

func (w *mockWatcher) Stop() error {
	w.MethodCall(w, "Stop")
	if err := w.NextErr(); err != nil {
		return err
	}
	w.Tomb.Kill(nil)
	return w.Tomb.Wait()
}

type mockStringsWatcher struct {
	mockWatcher
	changes chan []string
}

func newMockStringsWatcher() *mockStringsWatcher {
	w := &mockStringsWatcher{changes: make(chan []string, 5)}
	w.Tomb.Go(func() error {
		<-w.Tomb.Dying()
		return nil
	})
	return w
}

func (w *mockStringsWatcher) Changes() watcher.StringsChannel {
	return w.changes
}

type mockNotifyWatcher struct {
	mockWatcher
	changes chan struct{}
}

func newMockNotifyWatcher() *mockNotifyWatcher {
	w := &mockNotifyWatcher{changes: make(chan struct{}, 5)}
	w.Tomb.Go(func() error {
		<-w.Tomb.Dying()
		return nil
	})
	return w
}

func (m *mockNotifyWatcher) Changes() watcher.NotifyChannel {
	return m.changes
}
