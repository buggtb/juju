// Copyright 2019 Canonical Ltd.
// Licensed under the AGPLv3, see LICENCE file for details.

package instancemutater

import (
	"github.com/juju/errors"
	"gopkg.in/juju/names.v3"
	"gopkg.in/juju/worker.v1"
	"gopkg.in/juju/worker.v1/catacomb"

	"github.com/juju/juju/agent"
	"github.com/juju/juju/api/instancemutater"
	"github.com/juju/juju/core/watcher"
	"github.com/juju/juju/environs"
)

//go:generate mockgen -package mocks -destination mocks/instancebroker_mock.go github.com/juju/juju/worker/instancemutater InstanceMutaterAPI
//go:generate mockgen -package mocks -destination mocks/logger_mock.go github.com/juju/juju/worker/instancemutater Logger
//go:generate mockgen -package mocks -destination mocks/namestag_mock.go gopkg.in/juju/names.v3 Tag
//go:generate mockgen -package mocks -destination mocks/machinemutater_mock.go github.com/juju/juju/api/instancemutater MutaterMachine

type InstanceMutaterAPI interface {
	WatchMachines() (watcher.StringsWatcher, error)
	Machine(tag names.MachineTag) (instancemutater.MutaterMachine, error)
}

// Logger represents the logging methods called.
type Logger interface {
	Warningf(message string, args ...interface{})
	Debugf(message string, args ...interface{})
	Errorf(message string, args ...interface{})
	Tracef(message string, args ...interface{})
}

// Config represents the configuration required to run a new instance machineApi
// worker.
type Config struct {
	Facade InstanceMutaterAPI

	// Logger is the Logger for this worker.
	Logger Logger

	Broker environs.LXDProfiler

	AgentConfig agent.Config

	// Tag is the current MutaterMachine tag
	Tag names.Tag

	// GetMachineWatcher allows the worker to watch different "machines"
	// depending on whether this work is running with an environ broker
	// or a container broker.
	GetMachineWatcher func() (watcher.StringsWatcher, error)

	// GetRequiredLXPprofiles provides a slice of strings representing the
	// lxd profiles to be included on every LXD machine used given the
	// current model name.
	GetRequiredLXDProfiles RequiredLXDProfilesFunc

	// GetRequiredContext provides a way to override the given context
	// Note: the following is required for testing purposes when we have an
	// error case and we want to know when it's valid to kill/clean the worker.
	GetRequiredContext RequiredMutaterContextFunc
}

type RequiredLXDProfilesFunc func(string) []string

type RequiredMutaterContextFunc func(MutaterContext) MutaterContext

// Validate checks for missing values from the configuration and checks that
// they conform to a given type.
func (config Config) Validate() error {
	if config.Logger == nil {
		return errors.NotValidf("nil Logger")
	}
	if config.Facade == nil {
		return errors.NotValidf("nil Facade")
	}
	if config.Broker == nil {
		return errors.NotValidf("nil Broker")
	}
	if config.AgentConfig == nil {
		return errors.NotValidf("nil AgentConfig")
	}
	if config.Tag == nil {
		return errors.NotValidf("nil Tag")
	}
	if _, ok := config.Tag.(names.MachineTag); !ok {
		return errors.NotValidf("Tag")
	}
	if config.GetMachineWatcher == nil {
		return errors.NotValidf("nil GetMachineWatcher")
	}
	if config.GetRequiredLXDProfiles == nil {
		return errors.NotValidf("nil GetRequiredLXDProfiles")
	}
	if config.GetRequiredContext == nil {
		return errors.NotValidf("nil GetRequiredContext")
	}
	return nil
}

// NewEnvironWorker returns a worker that keeps track of
// the machines in the state and polls their instance
// for addition or removal changes.
func NewEnvironWorker(config Config) (worker.Worker, error) {
	config.GetMachineWatcher = config.Facade.WatchMachines
	config.GetRequiredLXDProfiles = func(modelName string) []string {
		return []string{"default", "juju-" + modelName}
	}
	config.GetRequiredContext = func(ctx MutaterContext) MutaterContext {
		return ctx
	}
	return newWorker(config)
}

// NewContainerWorker returns a worker that keeps track of
// the containers in the state for this machine agent and
// polls their instance for addition or removal changes.
func NewContainerWorker(config Config) (worker.Worker, error) {
	m, err := config.Facade.Machine(config.Tag.(names.MachineTag))
	if err != nil {
		return nil, errors.Trace(err)
	}
	config.GetRequiredLXDProfiles = func(_ string) []string { return []string{"default"} }
	config.GetMachineWatcher = m.WatchContainers
	config.GetRequiredContext = func(ctx MutaterContext) MutaterContext {
		return ctx
	}
	return newWorker(config)
}

func newWorker(config Config) (*mutaterWorker, error) {
	if err := config.Validate(); err != nil {
		return nil, errors.Trace(err)
	}
	watcher, err := config.GetMachineWatcher()
	if err != nil {
		return nil, errors.Trace(err)
	}
	w := &mutaterWorker{
		logger:                     config.Logger,
		facade:                     config.Facade,
		broker:                     config.Broker,
		machineTag:                 config.Tag.(names.MachineTag),
		machineWatcher:             watcher,
		getRequiredLXDProfilesFunc: config.GetRequiredLXDProfiles,
		getRequiredContextFunc:     config.GetRequiredContext,
	}
	// getRequiredContextFunc returns a MutaterContext, this is for overriding
	// during testing.
	err = catacomb.Invoke(catacomb.Plan{
		Site: &w.catacomb,
		Work: w.loop,
		Init: []worker.Worker{watcher},
	})
	if err != nil {
		return nil, errors.Trace(err)
	}
	return w, nil
}

type mutaterWorker struct {
	catacomb catacomb.Catacomb

	logger                     Logger
	broker                     environs.LXDProfiler
	machineTag                 names.MachineTag
	facade                     InstanceMutaterAPI
	machineWatcher             watcher.StringsWatcher
	getRequiredLXDProfilesFunc RequiredLXDProfilesFunc
	getRequiredContextFunc     RequiredMutaterContextFunc
}

func (w *mutaterWorker) loop() error {
	m := &mutater{
		context:     w.getRequiredContextFunc(w),
		logger:      w.logger,
		machines:    make(map[names.MachineTag]chan struct{}),
		machineDead: make(chan instancemutater.MutaterMachine),
	}
	for {
		select {
		case <-m.context.dying():
			return m.context.errDying()
		case ids, ok := <-w.machineWatcher.Changes():
			if !ok {
				return errors.New("machines watcher closed")
			}
			tags := make([]names.MachineTag, len(ids))
			for i := range ids {
				tags[i] = names.NewMachineTag(ids[i])
			}
			if err := m.startMachines(tags); err != nil {
				return err
			}
		case d := <-m.machineDead:
			delete(m.machines, d.Tag())
		}
	}
}

// Kill implements worker.Worker.Kill.
func (w *mutaterWorker) Kill() {
	w.catacomb.Kill(nil)
}

// Wait implements worker.Worker.Wait.
func (w *mutaterWorker) Wait() error {
	return w.catacomb.Wait()
}

// Stop stops the instancemutaterworker and returns any
// error it encountered when running.
func (w *mutaterWorker) Stop() error {
	w.Kill()
	return w.Wait()
}

// newMachineContext is part of the mutaterContext interface.
func (w *mutaterWorker) newMachineContext() MachineContext {
	return w.getRequiredContextFunc(w)
}

// getMachine is part of the MachineContext interface.
func (w *mutaterWorker) getMachine(tag names.MachineTag) (instancemutater.MutaterMachine, error) {
	m, err := w.facade.Machine(tag)
	return m, err
}

// getBroker is part of the MachineContext interface.
func (w *mutaterWorker) getBroker() environs.LXDProfiler {
	return w.broker
}

// getRequiredLXDProfiles part of the MachineContext interface.
func (w *mutaterWorker) getRequiredLXDProfiles(modelName string) []string {
	return w.getRequiredLXDProfilesFunc(modelName)
}

// kill is part of the lifetimeContext interface.
func (w *mutaterWorker) KillWithError(err error) {
	w.catacomb.Kill(err)
}

// dying is part of the lifetimeContext interface.
func (w *mutaterWorker) dying() <-chan struct{} {
	return w.catacomb.Dying()
}

// errDying is part of the lifetimeContext interface.
func (w *mutaterWorker) errDying() error {
	return w.catacomb.ErrDying()
}

// add is part of the lifetimeContext interface.
func (w *mutaterWorker) add(new worker.Worker) error {
	return w.catacomb.Add(new)
}
