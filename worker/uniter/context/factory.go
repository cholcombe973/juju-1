// Copyright 2012-2014 Canonical Ltd.
// Licensed under the AGPLv3, see LICENCE file for details.

package context

import (
	"fmt"
	"math/rand"
	"time"

	"github.com/juju/errors"
	"github.com/juju/names"
	"gopkg.in/juju/charm.v4"
	"gopkg.in/juju/charm.v4/hooks"

	"github.com/juju/juju/api/uniter"
	"github.com/juju/juju/apiserver/params"
	"github.com/juju/juju/worker/uniter/hook"
)

// Factory represents a long-lived object that can create execution contexts
// relevant to a specific unit. In its current state, it is somewhat bizarre
// and inconsistent; its main value is as an evolutionary step towards a better
// division of responsibilities across worker/uniter and its subpackages.
type Factory interface {

	// NewRunContext returns an execution context suitable for running an
	// arbitrary script.
	NewRunContext() (*HookContext, error)

	// NewHookContext returns an execution context suitable for running the
	// supplied hook definition (which must be valid).
	NewHookContext(hookInfo hook.Info) (*HookContext, error)

	// NewActionContext returns an execution context suitable for running the
	// action identified by the supplied id.
	NewActionContext(actionId string) (*HookContext, error)
}

// CharmFunc is used to get a snapshot of the charm at context creation time.
type CharmFunc func() (charm.Charm, error)

// RelationsFunc is used to get snapshots of relation membership at context
// creation time.
type RelationsFunc func() map[int]*RelationInfo

// NewFactory returns a Factory capable of creating execution contexts backed
// by the supplied unit's supplied API connection.
func NewFactory(
	state *uniter.State, unitTag names.UnitTag, getRelationInfos RelationsFunc, getCharm CharmFunc,
) (
	Factory, error,
) {
	unit, err := state.Unit(unitTag)
	if err != nil {
		return nil, errors.Trace(err)
	}
	service, err := state.Service(unit.ServiceTag())
	if err != nil {
		return nil, errors.Trace(err)
	}
	ownerTag, err := service.OwnerTag()
	if err != nil {
		return nil, errors.Trace(err)
	}
	machineTag, err := unit.AssignedMachine()
	if err != nil {
		return nil, errors.Trace(err)
	}
	environment, err := state.Environment()
	if err != nil {
		return nil, errors.Trace(err)
	}
	return &factory{
		unit:             unit,
		state:            state,
		envUUID:          environment.UUID(),
		envName:          environment.Name(),
		machineTag:       machineTag,
		ownerTag:         ownerTag,
		getRelationInfos: getRelationInfos,
		getCharm:         getCharm,
		relationCaches:   map[int]*RelationCache{},
		rand:             rand.New(rand.NewSource(time.Now().Unix())),
	}, nil
}

type factory struct {
	// API connection fields; unit should be deprecated, but isn't yet.
	unit  *uniter.Unit
	state *uniter.State

	// Fields that shouldn't change in a factory's lifetime.
	envUUID    string
	envName    string
	machineTag names.MachineTag
	ownerTag   names.UserTag

	// Callback to get relation state snapshot.
	getRelationInfos RelationsFunc
	relationCaches   map[int]*RelationCache

	// Callback to get charm snapshot.
	getCharm CharmFunc

	// For generating "unique" context ids.
	rand *rand.Rand
}

// NewRunContext exists to satisfy the Factory interface.
func (f *factory) NewRunContext() (*HookContext, error) {
	ctx, err := f.coreContext()
	if err != nil {
		return nil, errors.Trace(err)
	}
	ctx.id = f.newId("run-commands")
	return ctx, nil
}

// NewHookContext exists to satisfy the Factory interface.
func (f *factory) NewHookContext(hookInfo hook.Info) (*HookContext, error) {
	if err := hookInfo.Validate(); err != nil {
		return nil, errors.Trace(err)
	}

	ctx, err := f.coreContext()
	if err != nil {
		return nil, errors.Trace(err)
	}

	hookName := string(hookInfo.Kind)
	if hookInfo.Kind.IsRelation() {
		ctx.relationId = hookInfo.RelationId
		ctx.remoteUnitName = hookInfo.RemoteUnit
		relation, found := ctx.relations[hookInfo.RelationId]
		if !found {
			return nil, fmt.Errorf("unknown relation id: %v", hookInfo.RelationId)
		}
		if hookInfo.Kind == hooks.RelationDeparted {
			relation.cache.RemoveMember(hookInfo.RemoteUnit)
		} else if hookInfo.RemoteUnit != "" {
			// Clear remote settings cache for changing remote unit.
			relation.cache.InvalidateMember(hookInfo.RemoteUnit)
		}
		hookName = fmt.Sprintf("%s-%s", relation.Name(), hookInfo.Kind)
	}
	// Metrics are only sent from the collect-metrics hook.
	if hookInfo.Kind == hooks.CollectMetrics {
		ctx.canAddMetrics = true
		ch, err := f.getCharm()
		if err != nil {
			return nil, errors.Trace(err)
		}
		ctx.definedMetrics = ch.Metrics()
	}
	ctx.id = f.newId(hookName)
	return ctx, nil
}

// NewActionContext exists to satisfy the Factory interface.
func (f *factory) NewActionContext(actionId string) (*HookContext, error) {
	ch, err := f.getCharm()
	if err != nil {
		return nil, errors.Trace(err)
	}

	tag, ok := names.ParseActionTagFromId(actionId)
	if !ok {
		return nil, &badActionError{actionId, "not valid actionId"}
	}
	action, err := f.state.Action(tag)
	if params.IsCodeNotFoundOrCodeUnauthorized(errors.Cause(err)) {
		return nil, ErrActionNotAvailable
	} else if err != nil {
		return nil, errors.Trace(err)
	}
	name := action.Name()
	spec, ok := ch.Actions().ActionSpecs[name]
	if !ok {
		return nil, &badActionError{name, "not defined"}
	}
	params := action.Params()
	if _, err := spec.ValidateParams(params); err != nil {
		return nil, &badActionError{name, err.Error()}
	}

	ctx, err := f.coreContext()
	if err != nil {
		return nil, errors.Trace(err)
	}
	ctx.actionData = newActionData(name, &tag, params)
	ctx.id = f.newId(name)
	return ctx, nil
}

// newId returns a probably-unique identifier for a new context, containing the
// supplied string.
func (f *factory) newId(name string) string {
	return fmt.Sprintf("%s-%s-%d", f.unit.Name(), name, f.rand.Int63())
}

// coreContext creates a new context with all unspecialised fields filled in.
func (f *factory) coreContext() (*HookContext, error) {
	ctx := &HookContext{
		unit:               f.unit,
		state:              f.state,
		uuid:               f.envUUID,
		envName:            f.envName,
		unitName:           f.unit.Name(),
		assignedMachineTag: f.machineTag,
		serviceOwner:       f.ownerTag,
		relations:          f.getContextRelations(),
		relationId:         -1,
		canAddMetrics:      false,
		definedMetrics:     nil,
		pendingPorts:       make(map[PortRange]PortRangeInfo),
	}
	if err := f.updateContext(ctx); err != nil {
		return nil, err
	}
	return ctx, nil
}

// getContextRelations updates the factory's relation caches, and uses them
// to construct contextRelations for a fresh context.
func (f *factory) getContextRelations() map[int]*ContextRelation {
	contextRelations := map[int]*ContextRelation{}
	relationInfos := f.getRelationInfos()
	relationCaches := map[int]*RelationCache{}
	for id, info := range relationInfos {
		relationUnit := info.RelationUnit
		memberNames := info.MemberNames
		cache, found := f.relationCaches[id]
		if found {
			cache.Prune(memberNames)
		} else {
			cache = NewRelationCache(relationUnit.ReadSettings, memberNames)
		}
		relationCaches[id] = cache
		contextRelations[id] = NewContextRelation(relationUnit, cache)
	}
	f.relationCaches = relationCaches
	return contextRelations
}

// updateContext fills in all unspecialized fields that require an API call to
// discover.
//
// Approximately *every* line of code in this function represents a bug: ie, some
// piece of information we expose to the charm but which we fail to report changes
// to via hooks. Furthermore, the fact that we make multiple API calls at this
// time, rather than grabbing everything we need in one go, is unforgivably yucky.
func (f *factory) updateContext(ctx *HookContext) (err error) {
	defer errors.Trace(err)

	ctx.apiAddrs, err = f.state.APIAddresses()
	if err != nil {
		return err
	}
	ctx.machinePorts, err = f.state.AllMachinePorts(f.machineTag)
	if err != nil {
		return errors.Trace(err)
	}

	statusCode, statusInfo, err := f.unit.MeterStatus()
	if err != nil {
		return errors.Annotate(err, "could not retrieve meter status for unit")
	}
	ctx.meterStatus = &meterStatus{
		code: statusCode,
		info: statusInfo,
	}

	// TODO(fwereade) 23-10-2014 bug 1384572
	// Nothing here should ever be getting the environ config directly.
	environConfig, err := f.state.EnvironConfig()
	if err != nil {
		return err
	}
	ctx.proxySettings = environConfig.ProxySettings()

	// Calling these last, because there's a potential race: they're not guaranteed
	// to be set in time to be needed for a hook. If they're not, we just leave them
	// unset as we always have; this isn't great but it's about behaviour preservation.
	ctx.publicAddress, err = f.unit.PublicAddress()
	if err != nil && !params.IsCodeNoAddressSet(err) {
		return err
	}
	ctx.privateAddress, err = f.unit.PrivateAddress()
	if err != nil && !params.IsCodeNoAddressSet(err) {
		return err
	}
	return nil
}