// Copyright 2017 Canonical Ltd.
// Licensed under the AGPLv3, see LICENCE file for details.

package upgrades

import (
	"github.com/juju/errors"

	"github.com/juju/juju/cloud"
	"github.com/juju/juju/environs"
	"github.com/juju/juju/environs/config"
	"github.com/juju/juju/state"
	"github.com/juju/juju/state/stateenvirons"
)

// StateBackend provides an interface for upgrading the global state database.
type StateBackend interface {
	AllModels() ([]Model, error)
	ControllerUUID() string

	StripLocalUserDomain() error
	RenameAddModelPermission() error
	AddMigrationAttempt() error
	AddLocalCharmSequences() error
	UpdateLegacyLXDCloudCredentials(string, cloud.Credential) error
	UpgradeNoProxyDefaults() error
	AddNonDetachableStorageMachineId() error
	RemoveNilValueApplicationSettings() error
	AddControllerLogCollectionsSizeSettings() error
	AddStatusHistoryPruneSettings() error
	AddActionPruneSettings() error
	AddStorageInstanceConstraints() error
	SplitLogCollections() error
	AddUpdateStatusHookSettings() error
	CorrectRelationUnitCounts() error
	AddModelEnvironVersion() error
}

// Model is an interface providing access to the details of a model within the
// controller.
type Model interface {
	Config() (*config.Config, error)
	CloudSpec() (environs.CloudSpec, error)
}

// NewStateBackend returns a new StateBackend using a *state.State object.
func NewStateBackend(st *state.State, pool *state.StatePool) StateBackend {
	return stateBackend{st, pool}
}

type stateBackend struct {
	st   *state.State
	pool *state.StatePool
}

func (s stateBackend) AllModels() ([]Model, error) {
	modelUUIDs, err := s.st.AllModelUUIDs()
	if err != nil {
		return nil, errors.Trace(err)
	}
	out := make([]Model, 0, len(modelUUIDs))
	for _, modelUUID := range modelUUIDs {
		st, release, err := s.pool.Get(modelUUID)
		if err != nil {
			return nil, errors.Trace(err)
		}
		defer release()
		model, err := st.Model()
		if err != nil {
			return nil, errors.Trace(err)
		}
		out = append(out, &modelShim{st, model})
	}
	return out, nil
}

func (s stateBackend) ControllerUUID() string {
	return s.st.ControllerUUID()
}

func (s stateBackend) StripLocalUserDomain() error {
	return state.StripLocalUserDomain(s.st)
}

func (s stateBackend) RenameAddModelPermission() error {
	return state.RenameAddModelPermission(s.st)
}

func (s stateBackend) AddMigrationAttempt() error {
	return state.AddMigrationAttempt(s.st)
}

func (s stateBackend) AddLocalCharmSequences() error {
	return state.AddLocalCharmSequences(s.st)
}

func (s stateBackend) UpdateLegacyLXDCloudCredentials(endpoint string, credential cloud.Credential) error {
	return state.UpdateLegacyLXDCloudCredentials(s.st, endpoint, credential)
}

func (s stateBackend) UpgradeNoProxyDefaults() error {
	return state.UpgradeNoProxyDefaults(s.st)
}

func (s stateBackend) AddNonDetachableStorageMachineId() error {
	return state.AddNonDetachableStorageMachineId(s.st)
}

func (s stateBackend) RemoveNilValueApplicationSettings() error {
	return state.RemoveNilValueApplicationSettings(s.st)
}

func (s stateBackend) AddControllerLogCollectionsSizeSettings() error {
	return state.AddControllerLogCollectionsSizeSettings(s.st)
}

func (s stateBackend) AddStatusHistoryPruneSettings() error {
	return state.AddStatusHistoryPruneSettings(s.st)
}

func (s stateBackend) AddActionPruneSettings() error {
	return state.AddActionPruneSettings(s.st)
}

func (s stateBackend) AddUpdateStatusHookSettings() error {
	return state.AddUpdateStatusHookSettings(s.st)
}

func (s stateBackend) AddStorageInstanceConstraints() error {
	return state.AddStorageInstanceConstraints(s.st)
}

func (s stateBackend) SplitLogCollections() error {
	return state.SplitLogCollections(s.st)
}

func (s stateBackend) CorrectRelationUnitCounts() error {
	return state.CorrectRelationUnitCounts(s.st)
}

func (s stateBackend) AddModelEnvironVersion() error {
	return state.AddModelEnvironVersion(s.st)
}

type modelShim struct {
	st *state.State
	m  *state.Model
}

func (m *modelShim) Config() (*config.Config, error) {
	return m.m.Config()
}

func (m *modelShim) CloudSpec() (environs.CloudSpec, error) {
	cloudName := m.m.Cloud()
	regionName := m.m.CloudRegion()
	credentialTag, _ := m.m.CloudCredential()
	return stateenvirons.CloudSpec(m.st, cloudName, regionName, credentialTag)
}
