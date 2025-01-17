// Copyright 2014 Canonical Ltd.
// Licensed under the AGPLv3, see LICENCE file for details.

package action

import (
	"strings"

	"github.com/juju/errors"
	"gopkg.in/juju/names.v3"

	"github.com/juju/juju/apiserver/common"
	"github.com/juju/juju/apiserver/facade"
	"github.com/juju/juju/apiserver/params"
	"github.com/juju/juju/permission"
	"github.com/juju/juju/state"
)

// ActionAPI implements the client API for interacting with Actions
type ActionAPI struct {
	state      *state.State
	model      *state.Model
	resources  facade.Resources
	authorizer facade.Authorizer
	check      *common.BlockChecker
}

// APIv2 provides the Action API facade for version 2.
type APIv2 struct {
	*APIv3
}

// APIv3 provides the Action API facade for version 3.
type APIv3 struct {
	*APIv4
}

// APIv4 provides the Action API facade for version 4.
type APIv4 struct {
	*ActionAPI
}

// NewActionAPIV2 returns an initialized ActionAPI for version 2.
func NewActionAPIV2(ctx facade.Context) (*APIv2, error) {
	api, err := NewActionAPIV3(ctx)
	if err != nil {
		return nil, errors.Trace(err)
	}
	return &APIv2{api}, nil
}

// NewActionAPIV3 returns an initialized ActionAPI for version 3.
func NewActionAPIV3(ctx facade.Context) (*APIv3, error) {
	api, err := NewActionAPIV4(ctx)
	if err != nil {
		return nil, errors.Trace(err)
	}
	return &APIv3{api}, nil
}

// NewActionAPIV4 returns an initialized ActionAPI for version 4.
func NewActionAPIV4(ctx facade.Context) (*APIv4, error) {
	api, err := newActionAPI(ctx.State(), ctx.Resources(), ctx.Auth())
	if err != nil {
		return nil, errors.Trace(err)
	}
	return &APIv4{api}, nil
}

func newActionAPI(st *state.State, resources facade.Resources, authorizer facade.Authorizer) (*ActionAPI, error) {
	if !authorizer.AuthClient() {
		return nil, common.ErrPerm
	}

	m, err := st.Model()
	if err != nil {
		return nil, errors.Trace(err)
	}

	return &ActionAPI{
		state:      st,
		model:      m,
		resources:  resources,
		authorizer: authorizer,
		check:      common.NewBlockChecker(st),
	}, nil
}

func (a *ActionAPI) checkCanRead() error {
	canRead, err := a.authorizer.HasPermission(permission.ReadAccess, a.model.ModelTag())
	if err != nil {
		return errors.Trace(err)
	}
	if !canRead {
		return common.ErrPerm
	}
	return nil
}

func (a *ActionAPI) checkCanWrite() error {
	canWrite, err := a.authorizer.HasPermission(permission.WriteAccess, a.model.ModelTag())
	if err != nil {
		return errors.Trace(err)
	}
	if !canWrite {
		return common.ErrPerm
	}
	return nil
}

func (a *ActionAPI) checkCanAdmin() error {
	canAdmin, err := a.authorizer.HasPermission(permission.AdminAccess, a.model.ModelTag())
	if err != nil {
		return errors.Trace(err)
	}
	if !canAdmin {
		return common.ErrPerm
	}
	return nil
}

// Actions takes a list of ActionTags, and returns the full Action for
// each ID.
func (a *ActionAPI) Actions(arg params.Entities) (params.ActionResults, error) {
	if err := a.checkCanRead(); err != nil {
		return params.ActionResults{}, errors.Trace(err)
	}

	response := params.ActionResults{Results: make([]params.ActionResult, len(arg.Entities))}
	for i, entity := range arg.Entities {
		currentResult := &response.Results[i]
		tag, err := names.ParseTag(entity.Tag)
		if err != nil {
			currentResult.Error = common.ServerError(common.ErrBadId)
			continue
		}
		actionTag, ok := tag.(names.ActionTag)
		if !ok {
			currentResult.Error = common.ServerError(common.ErrBadId)
			continue
		}
		m, err := a.state.Model()
		if err != nil {
			return params.ActionResults{}, errors.Trace(err)
		}
		action, err := m.ActionByTag(actionTag)
		if err != nil {
			currentResult.Error = common.ServerError(common.ErrBadId)
			continue
		}
		receiverTag, err := names.ActionReceiverTag(action.Receiver())
		if err != nil {
			currentResult.Error = common.ServerError(err)
			continue
		}
		response.Results[i] = common.MakeActionResult(receiverTag, action)
	}
	return response, nil
}

// FindActionTagsByPrefix takes a list of string prefixes and finds
// corresponding ActionTags that match that prefix.
func (a *ActionAPI) FindActionTagsByPrefix(arg params.FindTags) (params.FindTagsResults, error) {
	if err := a.checkCanRead(); err != nil {
		return params.FindTagsResults{}, errors.Trace(err)
	}

	response := params.FindTagsResults{Matches: make(map[string][]params.Entity)}
	for _, prefix := range arg.Prefixes {
		m, err := a.state.Model()
		if err != nil {
			return params.FindTagsResults{}, errors.Trace(err)
		}
		found := m.FindActionTagsByPrefix(prefix)
		matches := make([]params.Entity, len(found))
		for i, tag := range found {
			matches[i] = params.Entity{Tag: tag.String()}
		}
		response.Matches[prefix] = matches
	}
	return response, nil
}

func (a *ActionAPI) FindActionsByNames(arg params.FindActionsByNames) (params.ActionsByNames, error) {
	if err := a.checkCanWrite(); err != nil {
		return params.ActionsByNames{}, errors.Trace(err)
	}

	response := params.ActionsByNames{Actions: make([]params.ActionsByName, len(arg.ActionNames))}
	for i, name := range arg.ActionNames {
		currentResult := &response.Actions[i]
		currentResult.Name = name

		m, err := a.state.Model()
		if err != nil {
			return params.ActionsByNames{}, errors.Trace(err)
		}

		actions, err := m.FindActionsByName(name)
		if err != nil {
			currentResult.Error = common.ServerError(err)
			continue
		}
		for _, action := range actions {
			recvTag, err := names.ActionReceiverTag(action.Receiver())
			if err != nil {
				currentResult.Actions = append(currentResult.Actions, params.ActionResult{Error: common.ServerError(err)})
				continue
			}
			currentAction := common.MakeActionResult(recvTag, action)
			currentResult.Actions = append(currentResult.Actions, currentAction)
		}
	}
	return response, nil
}

// Enqueue takes a list of Actions and queues them up to be executed by
// the designated ActionReceiver, returning the params.Action for each
// enqueued Action, or an error if there was a problem enqueueing the
// Action.
func (a *ActionAPI) Enqueue(arg params.Actions) (params.ActionResults, error) {
	if err := a.checkCanWrite(); err != nil {
		return params.ActionResults{}, errors.Trace(err)
	}

	var leaders map[string]string
	getLeader := func(appName string) (string, error) {
		if leaders == nil {
			var err error
			leaders, err = a.state.ApplicationLeaders()
			if err != nil {
				return "", err
			}
		}
		if leader, ok := leaders[appName]; ok {
			return leader, nil
		}
		return "", errors.Errorf("could not determine leader for %q", appName)
	}

	tagToActionReceiver := common.TagToActionReceiverFn(a.state.FindEntity)
	response := params.ActionResults{Results: make([]params.ActionResult, len(arg.Actions))}
	for i, action := range arg.Actions {
		currentResult := &response.Results[i]
		actionReceiver := action.Receiver
		if strings.HasSuffix(actionReceiver, "leader") {
			app := strings.Split(actionReceiver, "/")[0]
			receiverName, err := getLeader(app)
			if err != nil {
				currentResult.Error = common.ServerError(err)
				continue
			}
			actionReceiver = names.NewUnitTag(receiverName).String()
		}
		receiver, err := tagToActionReceiver(actionReceiver)
		if err != nil {
			currentResult.Error = common.ServerError(err)
			continue
		}
		enqueued, err := receiver.AddAction(action.Name, action.Parameters)
		if err != nil {
			currentResult.Error = common.ServerError(err)
			continue
		}

		response.Results[i] = common.MakeActionResult(receiver.Tag(), enqueued)
	}
	return response, nil
}

// ListAll takes a list of Entities representing ActionReceivers and
// returns all of the Actions that have been enqueued or run by each of
// those Entities.
func (a *ActionAPI) ListAll(arg params.Entities) (params.ActionsByReceivers, error) {
	if err := a.checkCanRead(); err != nil {
		return params.ActionsByReceivers{}, errors.Trace(err)
	}

	return a.internalList(arg, combine(pendingActions, runningActions, completedActions))
}

// ListPending takes a list of Entities representing ActionReceivers
// and returns all of the Actions that are enqueued for each of those
// Entities.
func (a *ActionAPI) ListPending(arg params.Entities) (params.ActionsByReceivers, error) {
	if err := a.checkCanRead(); err != nil {
		return params.ActionsByReceivers{}, errors.Trace(err)
	}

	return a.internalList(arg, pendingActions)
}

// ListRunning takes a list of Entities representing ActionReceivers and
// returns all of the Actions that have are running on each of those
// Entities.
func (a *ActionAPI) ListRunning(arg params.Entities) (params.ActionsByReceivers, error) {
	if err := a.checkCanRead(); err != nil {
		return params.ActionsByReceivers{}, errors.Trace(err)
	}

	return a.internalList(arg, runningActions)
}

// ListCompleted takes a list of Entities representing ActionReceivers
// and returns all of the Actions that have been run on each of those
// Entities.
func (a *ActionAPI) ListCompleted(arg params.Entities) (params.ActionsByReceivers, error) {
	if err := a.checkCanRead(); err != nil {
		return params.ActionsByReceivers{}, errors.Trace(err)
	}

	return a.internalList(arg, completedActions)
}

// Cancel attempts to cancel enqueued Actions from running.
func (a *ActionAPI) Cancel(arg params.Entities) (params.ActionResults, error) {
	if err := a.checkCanWrite(); err != nil {
		return params.ActionResults{}, errors.Trace(err)
	}

	response := params.ActionResults{Results: make([]params.ActionResult, len(arg.Entities))}

	for i, entity := range arg.Entities {
		currentResult := &response.Results[i]
		currentResult.Action = &params.Action{Tag: entity.Tag}
		tag, err := names.ParseTag(entity.Tag)
		if err != nil {
			currentResult.Error = common.ServerError(common.ErrBadId)
			continue
		}
		actionTag, ok := tag.(names.ActionTag)
		if !ok {
			currentResult.Error = common.ServerError(common.ErrBadId)
			continue
		}

		m, err := a.state.Model()
		if err != nil {
			return params.ActionResults{}, errors.Trace(err)
		}

		action, err := m.ActionByTag(actionTag)
		if err != nil {
			currentResult.Error = common.ServerError(err)
			continue
		}
		result, err := action.Finish(state.ActionResults{Status: state.ActionCancelled, Message: "action cancelled via the API"})
		if err != nil {
			currentResult.Error = common.ServerError(err)
			continue
		}
		receiverTag, err := names.ActionReceiverTag(result.Receiver())
		if err != nil {
			currentResult.Error = common.ServerError(err)
			continue
		}

		response.Results[i] = common.MakeActionResult(receiverTag, result)
	}
	return response, nil
}

// ApplicationsCharmsActions returns a slice of charm Actions for a slice of
// services.
func (a *ActionAPI) ApplicationsCharmsActions(args params.Entities) (params.ApplicationsCharmActionsResults, error) {
	result := params.ApplicationsCharmActionsResults{Results: make([]params.ApplicationCharmActionsResult, len(args.Entities))}
	if err := a.checkCanWrite(); err != nil {
		return result, errors.Trace(err)
	}

	for i, entity := range args.Entities {
		currentResult := &result.Results[i]
		svcTag, err := names.ParseApplicationTag(entity.Tag)
		if err != nil {
			currentResult.Error = common.ServerError(common.ErrBadId)
			continue
		}
		currentResult.ApplicationTag = svcTag.String()
		svc, err := a.state.Application(svcTag.Id())
		if err != nil {
			currentResult.Error = common.ServerError(err)
			continue
		}
		ch, _, err := svc.Charm()
		if err != nil {
			currentResult.Error = common.ServerError(err)
			continue
		}
		if actions := ch.Actions(); actions != nil {
			charmActions := make(map[string]params.ActionSpec)
			for key, value := range actions.ActionSpecs {
				charmActions[key] = params.ActionSpec{
					Description: value.Description,
					Params:      value.Params,
				}
			}
			currentResult.Actions = charmActions
		}
	}
	return result, nil
}

// internalList takes a list of Entities representing ActionReceivers
// and returns all of the Actions the extractorFn can get out of the
// ActionReceiver.
func (a *ActionAPI) internalList(arg params.Entities, fn extractorFn) (params.ActionsByReceivers, error) {
	tagToActionReceiver := common.TagToActionReceiverFn(a.state.FindEntity)
	response := params.ActionsByReceivers{Actions: make([]params.ActionsByReceiver, len(arg.Entities))}
	for i, entity := range arg.Entities {
		currentResult := &response.Actions[i]
		receiver, err := tagToActionReceiver(entity.Tag)
		if err != nil {
			currentResult.Error = common.ServerError(common.ErrBadId)
			continue
		}
		currentResult.Receiver = receiver.Tag().String()

		results, err := fn(receiver)
		if err != nil {
			currentResult.Error = common.ServerError(err)
			continue
		}
		currentResult.Actions = results
	}
	return response, nil
}

// extractorFn is the generic signature for functions that extract
// state.Actions from an ActionReceiver, and return them as a slice of
// params.ActionResult.
type extractorFn func(state.ActionReceiver) ([]params.ActionResult, error)

// combine takes multiple extractorFn's and combines them into one
// function.
func combine(funcs ...extractorFn) extractorFn {
	return func(ar state.ActionReceiver) ([]params.ActionResult, error) {
		result := []params.ActionResult{}
		for _, fn := range funcs {
			items, err := fn(ar)
			if err != nil {
				return result, errors.Trace(err)
			}
			result = append(result, items...)
		}
		return result, nil
	}
}

// pendingActions iterates through the Actions() enqueued for an
// ActionReceiver, and converts them to a slice of params.ActionResult.
func pendingActions(ar state.ActionReceiver) ([]params.ActionResult, error) {
	return common.ConvertActions(ar, ar.PendingActions)
}

// runningActions iterates through the Actions() running on an
// ActionReceiver, and converts them to a slice of params.ActionResult.
func runningActions(ar state.ActionReceiver) ([]params.ActionResult, error) {
	return common.ConvertActions(ar, ar.RunningActions)
}

// completedActions iterates through the Actions() that have run to
// completion for an ActionReceiver, and converts them to a slice of
// params.ActionResult.
func completedActions(ar state.ActionReceiver) ([]params.ActionResult, error) {
	return common.ConvertActions(ar, ar.CompletedActions)
}
