// Copyright 2015 Canonical Ltd.
// Licensed under the AGPLv3, see LICENCE file for details.

package context

import (
	"fmt"
	"math/rand"
	"time"

	"github.com/juju/errors"
	"github.com/juju/utils/clock"
	"gopkg.in/juju/charm.v6-unstable/hooks"
	"gopkg.in/juju/names.v2"

	"github.com/juju/juju/api/uniter"
	"github.com/juju/juju/apiserver/params"
	"github.com/juju/juju/core/leadership"
	"github.com/juju/juju/worker/uniter/hook"
	"github.com/juju/juju/worker/uniter/runner/jujuc"
)

// CommandInfo specifies the information necessary to run a command.
type CommandInfo struct {
	// RelationId is the relation context to execute the commands in.
	RelationId int
	// RemoteUnitName is the remote unit for the relation context.
	RemoteUnitName string
	// ForceRemoteUnit skips unit inference and existence validation.
	ForceRemoteUnit bool
}

// ContextFactory represents a long-lived object that can create execution contexts
// relevant to a specific unit.
type ContextFactory interface {
	// CommandContext creates a new context for running a juju command.
	CommandContext(commandInfo CommandInfo) (*HookContext, error)

	// HookContext creates a new context for running a juju hook.
	HookContext(hookInfo hook.Info) (*HookContext, error)

	// ActionContext creates a new context for running a juju action.
	ActionContext(actionData *ActionData) (*HookContext, error)
}

// StorageContextAccessor is an interface providing access to StorageContexts
// for a jujuc.Context.
type StorageContextAccessor interface {

	// StorageTags returns the tags of storage instances attached to
	// the unit.
	StorageTags() ([]names.StorageTag, error)

	// Storage returns the jujuc.ContextStorageAttachment with the
	// supplied tag if it was found, and whether it was found.
	Storage(names.StorageTag) (jujuc.ContextStorageAttachment, error)
}

// RelationsFunc is used to get snapshots of relation membership at context
// creation time.
type RelationsFunc func() map[int]*RelationInfo

type contextFactory struct {
	// API connection fields; unit should be deprecated, but isn't yet.
	unit    *uniter.Unit
	state   *uniter.State
	tracker leadership.Tracker

	// Fields that shouldn't change in a factory's lifetime.
	paths      Paths
	modelUUID  string
	envName    string
	machineTag names.MachineTag
	storage    StorageContextAccessor
	clock      clock.Clock
	zone       string
	principal  string

	// Callback to get relation state snapshot.
	getRelationInfos RelationsFunc
	relationCaches   map[int]*RelationCache

	// For generating "unique" context ids.
	rand *rand.Rand
}

// FactoryConfig contains configuration values
// for the context factory.
type FactoryConfig struct {
	State            *uniter.State
	UnitTag          names.UnitTag
	Tracker          leadership.Tracker
	GetRelationInfos RelationsFunc
	Storage          StorageContextAccessor
	Paths            Paths
	Clock            clock.Clock
}

// NewContextFactory returns a ContextFactory capable of creating execution contexts backed
// by the supplied unit's supplied API connection.
func NewContextFactory(config FactoryConfig) (ContextFactory, error) {
	unit, err := config.State.Unit(config.UnitTag)
	if err != nil {
		return nil, errors.Trace(err)
	}
	machineTag, err := unit.AssignedMachine()
	if err != nil {
		return nil, errors.Trace(err)
	}
	model, err := config.State.Model()
	if err != nil {
		return nil, errors.Trace(err)
	}

	zone, err := unit.AvailabilityZone()
	if err != nil {
		return nil, errors.Trace(err)
	}

	principal, ok, err := unit.PrincipalName()
	if err != nil {
		return nil, errors.Trace(err)
	} else if !ok {
		principal = ""
	}

	f := &contextFactory{
		unit:             unit,
		state:            config.State,
		tracker:          config.Tracker,
		paths:            config.Paths,
		modelUUID:        model.UUID(),
		envName:          model.Name(),
		machineTag:       machineTag,
		getRelationInfos: config.GetRelationInfos,
		relationCaches:   map[int]*RelationCache{},
		storage:          config.Storage,
		rand:             rand.New(rand.NewSource(time.Now().Unix())),
		clock:            config.Clock,
		zone:             zone,
		principal:        principal,
	}
	return f, nil
}

// newId returns a probably-unique identifier for a new context, containing the
// supplied string.
func (f *contextFactory) newId(name string) string {
	return fmt.Sprintf("%s-%s-%d", f.unit.Name(), name, f.rand.Int63())
}

// coreContext creates a new context with all unspecialised fields filled in.
func (f *contextFactory) coreContext() (*HookContext, error) {
	leadershipContext := newLeadershipContext(
		f.state.LeadershipSettings,
		f.tracker,
	)
	ctx := &HookContext{
		unit:               f.unit,
		state:              f.state,
		LeadershipContext:  leadershipContext,
		uuid:               f.modelUUID,
		envName:            f.envName,
		unitName:           f.unit.Name(),
		assignedMachineTag: f.machineTag,
		relations:          f.getContextRelations(),
		relationId:         -1,
		pendingPorts:       make(map[PortRange]PortRangeInfo),
		storage:            f.storage,
		clock:              f.clock,
		componentDir:       f.paths.ComponentDir,
		componentFuncs:     registeredComponentFuncs,
		availabilityzone:   f.zone,
		principal:          f.principal,
	}
	if err := f.updateContext(ctx); err != nil {
		return nil, err
	}
	return ctx, nil
}

// ActionContext is part of the ContextFactory interface.
func (f *contextFactory) ActionContext(actionData *ActionData) (*HookContext, error) {
	if actionData == nil {
		return nil, errors.New("nil actionData specified")
	}
	ctx, err := f.coreContext()
	if err != nil {
		return nil, errors.Trace(err)
	}
	ctx.actionData = actionData
	ctx.id = f.newId(actionData.Name)
	return ctx, nil
}

// HookContext is part of the ContextFactory interface.
func (f *contextFactory) HookContext(hookInfo hook.Info) (*HookContext, error) {
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
			return nil, errors.Errorf("unknown relation id: %v", hookInfo.RelationId)
		}
		if hookInfo.Kind == hooks.RelationDeparted {
			relation.cache.RemoveMember(hookInfo.RemoteUnit)
		} else if hookInfo.RemoteUnit != "" {
			// Clear remote settings cache for changing remote unit.
			relation.cache.InvalidateMember(hookInfo.RemoteUnit)
		}
		hookName = fmt.Sprintf("%s-%s", relation.Name(), hookInfo.Kind)
	}
	if hookInfo.Kind.IsStorage() {
		ctx.storageTag = names.NewStorageTag(hookInfo.StorageId)
		if _, err := ctx.storage.Storage(ctx.storageTag); err != nil {
			return nil, errors.Annotatef(err, "could not retrieve storage for id: %v", hookInfo.StorageId)
		}
		storageName, err := names.StorageName(hookInfo.StorageId)
		if err != nil {
			return nil, errors.Trace(err)
		}
		hookName = fmt.Sprintf("%s-%s", storageName, hookName)
	}
	ctx.id = f.newId(hookName)
	return ctx, nil
}

// CommandContext is part of the ContextFactory interface.
func (f *contextFactory) CommandContext(commandInfo CommandInfo) (*HookContext, error) {
	ctx, err := f.coreContext()
	if err != nil {
		return nil, errors.Trace(err)
	}
	relationId, remoteUnitName, err := inferRemoteUnit(ctx.relations, commandInfo)
	if err != nil {
		return nil, errors.Trace(err)
	}
	ctx.relationId = relationId
	ctx.remoteUnitName = remoteUnitName
	ctx.id = f.newId("run-commands")
	return ctx, nil
}

// getContextRelations updates the factory's relation caches, and uses them
// to construct ContextRelations for a fresh context.
func (f *contextFactory) getContextRelations() map[int]*ContextRelation {
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
func (f *contextFactory) updateContext(ctx *HookContext) (err error) {
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

	sla, err := f.state.SLALevel()
	if err != nil {
		return errors.Annotate(err, "could not retrieve the SLA level")
	}
	ctx.slaLevel = sla

	// TODO(fwereade) 23-10-2014 bug 1384572
	// Nothing here should ever be getting the environ config directly.
	modelConfig, err := f.state.ModelConfig()
	if err != nil {
		return err
	}
	ctx.proxySettings = modelConfig.ProxySettings()

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

func inferRemoteUnit(rctxs map[int]*ContextRelation, info CommandInfo) (int, string, error) {
	relationId := info.RelationId
	hasRelation := relationId != -1
	remoteUnit := info.RemoteUnitName
	hasRemoteUnit := remoteUnit != ""

	// Check baseline sanity of remote unit, if supplied.
	if hasRemoteUnit {
		if !names.IsValidUnit(remoteUnit) {
			return -1, "", errors.Errorf(`invalid remote unit: %s`, remoteUnit)
		} else if !hasRelation {
			return -1, "", errors.Errorf("remote unit provided without a relation: %s", remoteUnit)
		}
	}

	// Check sanity of relation, if supplied, otherwise easy early return.
	if !hasRelation {
		return relationId, remoteUnit, nil
	}
	rctx, found := rctxs[relationId]
	if !found {
		return -1, "", errors.Errorf("unknown relation id: %d", relationId)
	}

	// Past basic sanity checks; if forced, accept what we're given.
	if info.ForceRemoteUnit {
		return relationId, remoteUnit, nil
	}

	// Infer an appropriate remote unit if we can.
	possibles := rctx.UnitNames()
	if remoteUnit == "" {
		switch len(possibles) {
		case 0:
			return -1, "", errors.Errorf("cannot infer remote unit in empty relation %d", relationId)
		case 1:
			return relationId, possibles[0], nil
		}
		return -1, "", errors.Errorf("ambiguous remote unit; possibilities are %+v", possibles)
	}
	for _, possible := range possibles {
		if remoteUnit == possible {
			return relationId, remoteUnit, nil
		}
	}
	return -1, "", errors.Errorf("unknown remote unit %s; possibilities are %+v", remoteUnit, possibles)
}
