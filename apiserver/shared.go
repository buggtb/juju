// Copyright 2018 Canonical Ltd.
// Licensed under the AGPLv3, see LICENCE file for details.

package apiserver

import (
	"sync"
	"time"

	"github.com/juju/collections/set"
	"github.com/juju/errors"
	"github.com/juju/loggo"

	jujucontroller "github.com/juju/juju/controller"
	"github.com/juju/juju/core/cache"
	"github.com/juju/juju/core/lease"
	"github.com/juju/juju/core/presence"
	"github.com/juju/juju/feature"
	"github.com/juju/juju/pubsub/apiserver"
	"github.com/juju/juju/pubsub/controller"
	"github.com/juju/juju/state"
)

// SharedHub represents the methods of the pubsub.StructuredHub
// that are used. The context uses an interface to allow mocking
// of the hub.
type SharedHub interface {
	Publish(topic string, data interface{}) (<-chan struct{}, error)
	Subscribe(topic string, handler interface{}) (func(), error)
}

// sharedServerContext contains a number of components that are unchangeable in the API server.
// These components need to be exposed through the facade.Context. Instead of having the methods
// of newAPIHandler and newAPIRoot take ever increasing numbers of parameters, they will instead
// have a pointer to the sharedServerContext.
//
// All attributes in the context should be goroutine aware themselves, like the state pool, hub, and
// presence, or protected and only accessed through methods on this context object.
type sharedServerContext struct {
	statePool    *state.StatePool
	controller   *cache.Controller
	centralHub   SharedHub
	presence     presence.Recorder
	leaseManager lease.Manager
	logger       loggo.Logger

	configMutex      sync.RWMutex
	controllerConfig jujucontroller.Config
	features         set.Strings

	unsubscribe func()
}

type sharedServerConfig struct {
	statePool    *state.StatePool
	controller   *cache.Controller
	centralHub   SharedHub
	presence     presence.Recorder
	leaseManager lease.Manager
	logger       loggo.Logger
}

func (c *sharedServerConfig) validate() error {
	if c.statePool == nil {
		return errors.NotValidf("nil statePool")
	}
	if c.controller == nil {
		return errors.NotValidf("nil controller")
	}
	if c.centralHub == nil {
		return errors.NotValidf("nil centralHub")
	}
	if c.presence == nil {
		return errors.NotValidf("nil presence")
	}
	if c.leaseManager == nil {
		return errors.NotValidf("nil leaseManager")
	}
	return nil
}

func newSharedServerContex(config sharedServerConfig) (*sharedServerContext, error) {
	if err := config.validate(); err != nil {
		return nil, errors.Trace(err)
	}
	controllerConfig, err := config.statePool.SystemState().ControllerConfig()
	if err != nil {
		return nil, errors.Annotate(err, "unable to get controller config")
	}
	ctx := &sharedServerContext{
		statePool:        config.statePool,
		controller:       config.controller,
		centralHub:       config.centralHub,
		presence:         config.presence,
		leaseManager:     config.leaseManager,
		logger:           config.logger,
		controllerConfig: controllerConfig,
	}
	ctx.features = controllerConfig.Features()
	// We are able to get the current controller config before subscribing to changes
	// because the changes are only ever published in response to an API call, and
	// this function is called in the newServer call to create the API server,
	// and we know that we can't make any API calls until the server has started.
	ctx.unsubscribe, err = ctx.centralHub.Subscribe(controller.ConfigChanged, ctx.onConfigChanged)
	if err != nil {
		ctx.logger.Criticalf("programming error in subscribe function: %v", err)
		return nil, errors.Trace(err)
	}
	return ctx, nil
}

func (c *sharedServerContext) Close() {
	c.unsubscribe()
}

func (c *sharedServerContext) onConfigChanged(topic string, data controller.ConfigChangedMessage, err error) {
	if err != nil {
		c.logger.Criticalf("programming error in %s message data: %v", topic, err)
		return
	}

	features := data.Config.Features()

	c.configMutex.Lock()
	c.controllerConfig = data.Config
	removed := c.features.Difference(features)
	added := features.Difference(c.features)
	c.features = features
	values := features.SortedValues()
	c.configMutex.Unlock()

	if removed.Size() != 0 || added.Size() != 0 {
		c.logger.Infof("updating features to %v", values)
	}
	// If the presence implementation changes we need to restart
	// the apiserver. So if the old presence feature flag is in either
	// added or removed, we need to publish the restart message.
	if removed.Contains(feature.OldPresence) || added.Contains(feature.OldPresence) {
		_, err := c.centralHub.Publish(apiserver.RestartTopic, apiserver.Restart{
			LocalOnly: true,
		})
		if err != nil {
			c.logger.Errorf("unable to publish restart message: %v", err)
		}
	}
}

func (c *sharedServerContext) featureEnabled(flag string) bool {
	c.configMutex.RLock()
	defer c.configMutex.RUnlock()
	return c.features.Contains(flag)
}

func (c *sharedServerContext) maxDebugLogDuration() time.Duration {
	c.configMutex.RLock()
	defer c.configMutex.RUnlock()
	return c.controllerConfig.MaxDebugLogDuration()
}
