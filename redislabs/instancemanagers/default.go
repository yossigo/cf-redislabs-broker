package instancemanagers

import (
	"fmt"
	"sync"
	"time"

	"github.com/pivotal-cf/brokerapi"
	"github.com/pivotal-golang/lager"

	"github.com/RedisLabs/cf-redislabs-broker/redislabs/apiclient"
	"github.com/RedisLabs/cf-redislabs-broker/redislabs/cluster"
	"github.com/RedisLabs/cf-redislabs-broker/redislabs/config"
	"github.com/RedisLabs/cf-redislabs-broker/redislabs/persisters"
)

type defaultCreator struct {
	lock      sync.Mutex
	logger    lager.Logger
	apiClient apiclient.Client
}

var (
	WaitingForDatabaseTimeout = 15 //seconds
)

func NewDefault(conf config.Config, logger lager.Logger) *defaultCreator {
	return &defaultCreator{
		logger:    logger,
		apiClient: apiclient.New(conf, logger),
	}
}

func (d *defaultCreator) Create(instanceID string, settings map[string]interface{}, persister persisters.StatePersister) error {
	d.lock.Lock()
	defer d.lock.Unlock()

	// Load the broker state.
	d.logger.Info("Loading the broker state", lager.Data{
		"instance-id": instanceID,
	})
	state, err := persister.Load()
	if err != nil {
		d.logger.Fatal("Failed to load the broker state", err)
		return ErrFailedToLoadState
	}

	// Check whether the instance already exists.
	for _, s := range (*state).AvailableInstances {
		if s.ID == instanceID {
			d.logger.Error(fmt.Sprintf("Received a request to create an instance with ID %s that already exists", instanceID), ErrInstanceExists)
			return ErrInstanceExists
		}
	}

	// Ask the cluster to create a database.
	d.logger.Info("Creating a database", lager.Data{
		"instance-id": instanceID,
	})
	credentials, err := d.createDatabase(settings)
	if err != nil {
		return err
	}

	// Save the new state.
	s := persisters.ServiceInstance{ // the future state
		ID:          instanceID,
		Credentials: credentials,
	}
	(*state).AvailableInstances = append((*state).AvailableInstances, s)
	d.logger.Info("Saving the broker state", lager.Data{
		"instance-id": instanceID,
	})
	if err = persister.Save(state); err != nil {
		d.logger.Error("Failed to save the new state", err)
		return ErrFailedToSaveState
	}
	return nil
}

func (d *defaultCreator) Update(instanceID string, params map[string]interface{}, persister persisters.StatePersister) error {
	state, err := persister.Load()
	if err != nil {
		d.logger.Error("Failed to load the broker state", err)
		return err
	}
	for _, instance := range state.AvailableInstances {
		if instance.ID == instanceID {
			return d.updateDatabase(instance.Credentials.UID, params)
		}
	}
	return brokerapi.ErrInstanceDoesNotExist
}

func (d *defaultCreator) Destroy(instanceID string, persister persisters.StatePersister) error {
	state, err := persister.Load()
	if err != nil {
		d.logger.Error("Failed to load the broker state", err)
		return err
	}

	instancesLeft := []persisters.ServiceInstance{}
	removed := false
	for _, instance := range state.AvailableInstances {
		if instance.ID == instanceID {
			if err := d.deleteDatabase(instance.Credentials.UID); err != nil {
				return err
			}
			removed = true
		} else {
			instancesLeft = append(instancesLeft, instance)
		}
	}

	if !removed {
		return brokerapi.ErrInstanceDoesNotExist
	}

	// Save the new broker state.
	state.AvailableInstances = instancesLeft
	if err = persister.Save(state); err != nil {
		d.logger.Error("Failed to save the new broker state after the instance removal", err, lager.Data{
			"instance-id": instanceID,
		})
		return err
	}
	return nil
}

func (d *defaultCreator) InstanceExists(instanceID string, persister persisters.StatePersister) (bool, error) {
	return false, nil
}

func (d *defaultCreator) createDatabase(settings map[string]interface{}) (cluster.InstanceCredentials, error) {
	ch, err := d.apiClient.CreateDatabase(settings)
	if err != nil {
		return cluster.InstanceCredentials{}, err //ErrFailedToCreateDatabase
	}

	for {
		select {
		case credentials := <-ch:
			return credentials, nil
		case <-time.After(time.Second * time.Duration(WaitingForDatabaseTimeout)):
			d.logger.Error("Waiting for a database timeout is expired", ErrCreateDatabaseTimeoutExpired)
			return cluster.InstanceCredentials{}, ErrCreateDatabaseTimeoutExpired
		}
	}
}

func (d *defaultCreator) updateDatabase(UID int, params map[string]interface{}) error {
	return d.apiClient.UpdateDatabase(UID, params)
}

func (d *defaultCreator) deleteDatabase(UID int) error {
	return d.apiClient.DeleteDatabase(UID)
}
