// Copyright 2015 Canonical Ltd.
// Licensed under the AGPLv3, see LICENCE file for details.

package state

import (
	"time"

	"github.com/juju/errors"
	jujutxn "github.com/juju/txn"
	"github.com/juju/utils/set"
	charmresource "gopkg.in/juju/charm.v6-unstable/resource"
	"gopkg.in/juju/names.v2"
	"gopkg.in/mgo.v2/txn"

	"github.com/juju/juju/resource"
)

// ResourcePersistenceBase exposes the core persistence functionality
// needed for resources.
type ResourcePersistenceBase interface {
	// One populates doc with the document corresponding to the given
	// ID. Missing documents result in errors.NotFound.
	One(collName, id string, doc interface{}) error

	// All populates docs with the list of the documents corresponding
	// to the provided query.
	All(collName string, query, docs interface{}) error

	// Run runs the transaction generated by the provided factory
	// function. It may be retried several times.
	Run(transactions jujutxn.TransactionSource) error

	// ApplicationExistsOps returns the operations that verify that the
	// identified application exists.
	ApplicationExistsOps(applicationID string) []txn.Op

	// IncCharmModifiedVersionOps returns the operations necessary to increment
	// the CharmModifiedVersion field for the given application.
	IncCharmModifiedVersionOps(applicationID string) []txn.Op
}

// ResourcePersistence provides the persistence functionality for the
// Juju environment as a whole.
type ResourcePersistence struct {
	base ResourcePersistenceBase
}

// NewResourcePersistence wraps the base in a new ResourcePersistence.
func NewResourcePersistence(base ResourcePersistenceBase) *ResourcePersistence {
	return &ResourcePersistence{
		base: base,
	}
}

// ListResources returns the info for each non-pending resource of the
// identified service.
func (p ResourcePersistence) ListResources(applicationID string) (resource.ServiceResources, error) {
	logger.Tracef("listing all resources for application %q", applicationID)

	docs, err := p.resources(applicationID)
	if err != nil {
		return resource.ServiceResources{}, errors.Trace(err)
	}

	store := map[string]charmresource.Resource{}
	units := map[names.UnitTag][]resource.Resource{}
	downloadProgress := make(map[names.UnitTag]map[string]int64)

	var results resource.ServiceResources
	for _, doc := range docs {
		if doc.PendingID != "" {
			continue
		}

		res, err := doc2basicResource(doc)
		if err != nil {
			return resource.ServiceResources{}, errors.Trace(err)
		}
		if !doc.LastPolled.IsZero() {
			store[res.Name] = res.Resource
			continue
		}
		if doc.UnitID == "" {
			results.Resources = append(results.Resources, res)
			continue
		}
		tag := names.NewUnitTag(doc.UnitID)
		if doc.PendingID == "" {
			units[tag] = append(units[tag], res)
		}
		if doc.DownloadProgress != nil {
			if downloadProgress[tag] == nil {
				downloadProgress[tag] = make(map[string]int64)
			}
			downloadProgress[tag][doc.Name] = *doc.DownloadProgress
		}
	}
	for _, res := range results.Resources {
		storeRes := store[res.Name]
		results.CharmStoreResources = append(results.CharmStoreResources, storeRes)
	}
	for tag, res := range units {
		results.UnitResources = append(results.UnitResources, resource.UnitResources{
			Tag:              tag,
			Resources:        res,
			DownloadProgress: downloadProgress[tag],
		})
	}
	return results, nil
}

// ListPendingResources returns the extended, model-related info for
// each pending resource of the identifies service.
func (p ResourcePersistence) ListPendingResources(applicationID string) ([]resource.Resource, error) {
	docs, err := p.resources(applicationID)
	if err != nil {
		return nil, errors.Trace(err)
	}

	var resources []resource.Resource
	for _, doc := range docs {
		if doc.PendingID == "" {
			continue
		}
		// doc.UnitID will always be empty here.

		res, err := doc2basicResource(doc)
		if err != nil {
			return nil, errors.Trace(err)
		}
		resources = append(resources, res)
	}
	return resources, nil
}

// GetResource returns the extended, model-related info for the non-pending
// resource.
func (p ResourcePersistence) GetResource(id string) (res resource.Resource, storagePath string, _ error) {
	doc, err := p.getOne(id)
	if err != nil {
		return res, "", errors.Trace(err)
	}

	stored, err := doc2resource(doc)
	if err != nil {
		return res, "", errors.Trace(err)
	}

	return stored.Resource, stored.storagePath, nil
}

// StageResource adds the resource in a separate staging area
// if the resource isn't already staged. If it is then
// errors.AlreadyExists is returned. A wrapper around the staged
// resource is returned which supports both finalizing and removing
// the staged resource.
func (p ResourcePersistence) StageResource(res resource.Resource, storagePath string) (*StagedResource, error) {
	if storagePath == "" {
		return nil, errors.Errorf("missing storage path")
	}

	if err := res.Validate(); err != nil {
		return nil, errors.Annotate(err, "bad resource")
	}

	stored := storedResource{
		Resource:    res,
		storagePath: storagePath,
	}
	staged := &StagedResource{
		base:   p.base,
		id:     res.ID,
		stored: stored,
	}
	if err := staged.stage(); err != nil {
		return nil, errors.Trace(err)
	}
	return staged, nil
}

// SetResource sets the info for the resource.
func (p ResourcePersistence) SetResource(res resource.Resource) error {
	stored, err := p.getStored(res)
	if errors.IsNotFound(err) {
		stored = storedResource{Resource: res}
	} else if err != nil {
		return errors.Trace(err)
	}
	// TODO(ericsnow) Ensure that stored.Resource matches res? If we do
	// so then the following line is unnecessary.
	stored.Resource = res

	if err := res.Validate(); err != nil {
		return errors.Annotate(err, "bad resource")
	}

	buildTxn := func(attempt int) ([]txn.Op, error) {
		// This is an "upsert".
		var ops []txn.Op
		switch attempt {
		case 0:
			ops = newInsertResourceOps(stored)
		case 1:
			ops = newUpdateResourceOps(stored)
		default:
			// Either insert or update will work so we should not get here.
			return nil, errors.New("setting the resource failed")
		}
		if stored.PendingID == "" {
			// Only non-pending resources must have an existing service.
			ops = append(ops, p.base.ApplicationExistsOps(res.ApplicationID)...)
		}
		return ops, nil
	}
	if err := p.base.Run(buildTxn); err != nil {
		return errors.Trace(err)
	}
	return nil
}

// SetCharmStoreResource stores the resource info that was retrieved
// from the charm store.
func (p ResourcePersistence) SetCharmStoreResource(id, applicationID string, res charmresource.Resource, lastPolled time.Time) error {
	if err := res.Validate(); err != nil {
		return errors.Annotate(err, "bad resource")
	}

	csRes := charmStoreResource{
		Resource:      res,
		id:            id,
		applicationID: applicationID,
		lastPolled:    lastPolled,
	}

	buildTxn := func(attempt int) ([]txn.Op, error) {
		// This is an "upsert".
		var ops []txn.Op
		switch attempt {
		case 0:
			ops = newInsertCharmStoreResourceOps(csRes)
		case 1:
			ops = newUpdateCharmStoreResourceOps(csRes)
		default:
			// Either insert or update will work so we should not get here.
			return nil, errors.New("setting the resource failed")
		}
		// No pending resources so we always do this here.
		ops = append(ops, p.base.ApplicationExistsOps(applicationID)...)
		return ops, nil
	}
	if err := p.base.Run(buildTxn); err != nil {
		return errors.Trace(err)
	}
	return nil
}

// SetUnitResource stores the resource info for a particular unit. The
// resource must already be set for the application.
func (p ResourcePersistence) SetUnitResource(unitID string, res resource.Resource) error {
	if res.PendingID != "" {
		return errors.Errorf("pending resources not allowed")
	}
	return p.setUnitResource(unitID, res, nil)
}

// SetUnitResource stores the resource info for a particular unit. The
// resource must already be set for the application. The provided progress
// is stored in the DB.
func (p ResourcePersistence) SetUnitResourceProgress(unitID string, res resource.Resource, progress int64) error {
	if res.PendingID == "" {
		return errors.Errorf("only pending resources may track progress")
	}
	return p.setUnitResource(unitID, res, &progress)
}

func (p ResourcePersistence) setUnitResource(unitID string, res resource.Resource, progress *int64) error {
	stored, err := p.getStored(res)
	if err != nil {
		return errors.Trace(err)
	}
	// TODO(ericsnow) Ensure that stored.Resource matches res? If we do
	// so then the following line is unnecessary.
	stored.Resource = res

	if err := res.Validate(); err != nil {
		return errors.Annotate(err, "bad resource")
	}

	buildTxn := func(attempt int) ([]txn.Op, error) {
		// This is an "upsert".
		var ops []txn.Op
		switch attempt {
		case 0:
			ops = newInsertUnitResourceOps(unitID, stored, progress)
		case 1:
			ops = newUpdateUnitResourceOps(unitID, stored, progress)
		default:
			// Either insert or update will work so we should not get here.
			return nil, errors.New("setting the resource failed")
		}
		// No pending resources so we always do this here.
		ops = append(ops, p.base.ApplicationExistsOps(res.ApplicationID)...)
		return ops, nil
	}
	if err := p.base.Run(buildTxn); err != nil {
		return errors.Trace(err)
	}
	return nil
}

func (p ResourcePersistence) getStored(res resource.Resource) (storedResource, error) {
	doc, err := p.getOne(res.ID)
	if errors.IsNotFound(err) {
		err = errors.NotFoundf("resource %q", res.Name)
	}
	if err != nil {
		return storedResource{}, errors.Trace(err)
	}

	stored, err := doc2resource(doc)
	if err != nil {
		return stored, errors.Trace(err)
	}

	return stored, nil
}

// NewResolvePendingResourceOps generates mongo transaction operations
// to set the identified resource as active.
//
// Leaking mongo details (transaction ops) is a necessary evil since we
// do not have any machinery to facilitate transactions between
// different components.
func (p ResourcePersistence) NewResolvePendingResourceOps(resID, pendingID string) ([]txn.Op, error) {
	if pendingID == "" {
		return nil, errors.New("missing pending ID")
	}

	oldDoc, err := p.getOnePending(resID, pendingID)
	if errors.IsNotFound(err) {
		return nil, errors.NotFoundf("pending resource %q (%s)", resID, pendingID)
	}
	if err != nil {
		return nil, errors.Trace(err)
	}
	pending, err := doc2resource(oldDoc)
	if err != nil {
		return nil, errors.Trace(err)
	}

	exists := true
	if _, err := p.getOne(resID); errors.IsNotFound(err) {
		exists = false
	} else if err != nil {
		return nil, errors.Trace(err)
	}

	ops := newResolvePendingResourceOps(pending, exists)
	return ops, nil
}

// NewRemoveUnitResourcesOps returns mgo transaction operations
// that remove resource information specific to the unit from state.
func (p ResourcePersistence) NewRemoveUnitResourcesOps(unitID string) ([]txn.Op, error) {
	docs, err := p.unitResources(unitID)
	if err != nil {
		return nil, errors.Trace(err)
	}

	ops := newRemoveResourcesOps(docs)
	// We do not remove the resource from the blob store here. That is
	// an application-level matter.
	return ops, nil
}

// NewRemoveResourcesOps returns mgo transaction operations that
// remove all the applications's resources from state.
func (p ResourcePersistence) NewRemoveResourcesOps(applicationID string) ([]txn.Op, error) {
	docs, err := p.resources(applicationID)
	if err != nil {
		return nil, errors.Trace(err)
	}

	ops := newRemoveResourcesOps(docs)
	seenPaths := set.NewStrings()
	for _, doc := range docs {
		// Don't schedule cleanups for placeholder resources, or multiple for a given path.
		if doc.StoragePath == "" || seenPaths.Contains(doc.StoragePath) {
			continue
		}
		ops = append(ops, newCleanupOp(cleanupResourceBlob, doc.StoragePath))
		seenPaths.Add(doc.StoragePath)
	}
	return ops, nil
}
