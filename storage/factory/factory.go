// Copyright 2016 NetApp, Inc. All Rights Reserved.

package factory

import (
	"fmt"
	"strings"

	log "github.com/Sirupsen/logrus"
	"github.com/netapp/netappdvp/apis/sfapi"
	dvp "github.com/netapp/netappdvp/storage_drivers"
	fake_driver "github.com/netapp/trident/drivers/fake"

	"github.com/netapp/trident/config"
	"github.com/netapp/trident/storage"
	"github.com/netapp/trident/storage/eseries"
	"github.com/netapp/trident/storage/fake"
	"github.com/netapp/trident/storage/ontap"
	"github.com/netapp/trident/storage/solidfire"
)

// Note:  isPassed is copied verbatim from dvp.ontap_common.
func isPassed(s string) bool {
	const passed = "passed"
	return s == passed
}

func NewStorageBackendForConfig(configJSON string) (
	sb *storage.StorageBackend, err error,
) {
	var storageDriver storage.StorageDriver

	// Some drivers may panic during initialize if given invalid parameters,
	// so catch any panics that might occur and return an error.
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("Unable to instantiate backend:  %v", r)
		}
	}()

	commonConfig, err := dvp.ValidateCommonSettings(configJSON)
	if err != nil {
		err = fmt.Errorf("Input failed validation: %v", err)
		return
	}
	// Pre-driver initialization setup
	switch commonConfig.StorageDriverName {
	case dvp.OntapNASStorageDriverName:
		storageDriver = &ontap.OntapNASStorageDriver{}
	case dvp.OntapNASQtreeStorageDriverName:
		storageDriver = &ontap.OntapNASQtreeStorageDriver{}
	case dvp.OntapSANStorageDriverName:
		storageDriver = &ontap.OntapSANStorageDriver{}
	case dvp.SolidfireSANStorageDriverName:
		storageDriver = &solidfire.SolidfireSANStorageDriver{}
	case dvp.EseriesIscsiStorageDriverName:
		storageDriver = &eseries.EseriesStorageDriver{}
	case fake_driver.FakeStorageDriverName:
		storageDriver = &fake.FakeStorageDriver{}
	default:
		err = fmt.Errorf("Unknown storage driver: %v",
			commonConfig.StorageDriverName)
		return
	}

	// Warn about ignored fields in common config if any are set
	if commonConfig.Debug {
		log.WithFields(log.Fields{
			"driverName": commonConfig.StorageDriverName,
		}).Warn("debug set in backend config.  This will be ignored.")
	}
	if commonConfig.DisableDelete {
		log.WithFields(log.Fields{
			"driverName": commonConfig.StorageDriverName,
		}).Warn("disableDelete set in backend config.  This will be ignored.")
	}

	if initializeErr := storageDriver.Initialize(
		dvp.ContextTrident, configJSON, commonConfig); initializeErr != nil {
		err = fmt.Errorf("Problem initializing storage driver: '%v' error: %v",
			commonConfig.StorageDriverName, initializeErr)
		return
	}

	// Post-driver initialization setup
	switch commonConfig.StorageDriverName {
	case dvp.OntapNASStorageDriverName:
		break

	case dvp.OntapNASQtreeStorageDriverName:
		break

	case dvp.OntapSANStorageDriverName:
		driver := storageDriver.(*ontap.OntapSANStorageDriver)
		if driver.Config.IgroupName == "netappdvp" {
			// If 'netappdvp' is intended to be the default igroup,
			// config.DefaultOntapIgroup should be set to 'netappdvp'.
			driver.Config.IgroupName = config.DefaultOntapIgroup
		}

		response, errIgroupList := driver.API.IgroupList()
		if !isPassed(response.Result.ResultStatusAttr) {
			err = fmt.Errorf("Problem listing igroups for SVM %v: %v, %v",
				driver.Config.SVM, errIgroupList, response.Result.ResultErrnoAttr)
			return
		}

		found := false
		initiators := ""
		for _, igroupInfo := range response.Result.AttributesList() {
			if igroupInfo.Vserver() == driver.Config.SVM &&
				igroupInfo.InitiatorGroupName() ==
					driver.Config.IgroupName {
				found = true
				initiatorList := igroupInfo.Initiators()
				for _, initiator := range initiatorList {
					initiators = initiators + initiator.InitiatorName() + ","
				}
				initiators = strings.TrimSuffix(initiators, ",")
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("Initiator group %v doesn't exist for SVM %v "+
				"and needs to be manually created! Please also ensure all "+
				"relevant hosts are added to the igroup.",
				driver.Config.IgroupName, driver.Config.SVM)
		} else {
			log.WithFields(log.Fields{
				"driver":     dvp.OntapSANStorageDriverName,
				"SVM":        driver.Config.SVM,
				"igroup":     driver.Config.IgroupName,
				"initiators": initiators,
			}).Warn("Please ensure all relevant hosts are added to the ",
				"initiator group.")
		}

		/* TODO: DON'T DELETE
		// Create igroup automatically and use a REST endpoint for adding hosts
		// (code from nDVP's ontap_san.go)
		driver := storageDriver.(*ontap.OntapSANStorageDriver)
		response, err := driver.API.IgroupCreate(config.DefaultOntapIgroup, "iscsi", "linux")
		if !isPassed(response.Result.ResultStatusAttr) {
			if response.Result.ResultErrnoAttr != azgo.EVDISK_ERROR_INITGROUP_EXISTS {
				return nil, fmt.Errorf("Problem creating igroup %v: %v, %v",
					config.DefaultOntapIgroup, response.Result, err)
			}
		}

		// Not required for Trident but harmless to add host IQNs to the igroup
		iqns, errIqn := utils.GetInitiatorIqns()
		if errIqn != nil {
			return nil, fmt.Errorf("Problem determining host initiator IQNs: %v", errIqn)
		}
		// Add each IQN we found to the igroup
		for _, iqn := range iqns {
			response2, err2 := driver.API.IgroupAdd(config.DefaultOntapIgroup, iqn)
			if !isPassed(response2.Result.ResultStatusAttr) {
				if response2.Result.ResultErrnoAttr != azgo.EVDISK_ERROR_INITGROUP_HAS_NODE {
					return nil, fmt.Errorf("Problem adding IQN: %v to igroup: %v\n%verror: %v", iqn, config.DefaultOntapIgroup, response2.Result, err2)
				}
			}
		}*/
	case dvp.SolidfireSANStorageDriverName:
		driver := storageDriver.(*solidfire.SolidfireSANStorageDriver)

		// If zero AccessGroups are specified it could be that this is an upgrade where we
		// just utilize the default 'trident' group automatically.  Or, perhaps the deployment
		// doesn't need more than one set of 64 initiators, so we'll just use the old way of
		// doing it here, and look for/set the default group.
		if len(driver.Config.AccessGroups) == 0 {
			// We're going to do some hacky stuff here and make sure that if this is an upgrade
			// that we verify that one of the AccessGroups in the list is the default Trident VAG ID
			listVAGReq := &sfapi.ListVolumeAccessGroupsRequest{
				StartVAGID: 0,
				Limit:      0,
			}
			vags, vagErr := driver.Client.ListVolumeAccessGroups(listVAGReq)
			if vagErr != nil {
				err = fmt.Errorf("Could not list VAGs for backend %s: %s",
					driver.Config.SVIP, vagErr.Error())
				return
			}

			found := false
			initiators := ""
			for _, vag := range vags {
				//TODO: SolidFire backend config should support taking VAG as an arg
				if vag.Name == config.DefaultSolidFireVAG {
					driver.Config.AccessGroups = append(driver.Config.AccessGroups, vag.VAGID)
					found = true
					for _, initiator := range vag.Initiators {
						initiators = initiators + initiator + ","
					}
					initiators = strings.TrimSuffix(initiators, ",")
					log.Infof("no AccessGroup ID's configured, using the default group: %v, "+
						"with initiators: %+v", vag.Name, initiators)
					break
				}
			}
			if !found {
				err = fmt.Errorf("Volume Access Group %v doesn't exist at %v "+
					"and needs to be manually created! Please also ensure all "+
					"relevant hosts are added to the VAG.",
					config.DefaultSolidFireVAG, driver.Config.SVIP)
				return
			}
		} else if len(driver.Config.AccessGroups) > 4 {
			err = fmt.Errorf("The maximum number of allowed Volume Access Groups per config is 4 "+
				"but your config has specified %v!", len(driver.Config.AccessGroups))
			return
		} else {
			// We only need this in the case that AccessGroups were specified, if it was zero and we
			// used the default we already verified it in that step so we're good here.
			missingVags := driver.VerifyVags(driver.Config.AccessGroups)
			if len(missingVags) != 0 {
				err = fmt.Errorf("Failed to discover 1 or more of the specified VAG ID's! "+
					"Missing VAG IDS from Cluster discovery: %+v", missingVags)
				return
			}
		}

		log.WithFields(log.Fields{
			"driver":       dvp.SolidfireSANStorageDriverName,
			"SVIP":         driver.Config.SVIP,
			"AccessGroups": driver.Config.AccessGroups,
		}).Warn("Please ensure all relevant hosts are added to one of ",
			"the specified Volume Access Groups.")

		// Deal with upgrades for versions prior to handling multiple VAG ID's
		var vIDs []int64
		var req sfapi.ListVolumesForAccountRequest
		req.AccountID = driver.TenantID
		volumes, _ := driver.Client.ListVolumesForAccount(&req)
		for _, v := range volumes {
			if v.Status != "deleted" {
				vIDs = append(vIDs, v.VolumeID)
			}
		}
		for _, vag := range driver.Config.AccessGroups {
			addAGErr := driver.AddMissingVolumesToVag(vag, vIDs)
			if addAGErr != nil {
				err = fmt.Errorf("Failed to update AccessGroup membership of volume "+
					"%+v", addAGErr)
				return
			}
		}

		/* TODO: DON'T DELETE
		// Create VAG automatically and use a REST endpoint for adding hosts
		if !found {
			// Add the orchestrator Volume Access Group (VAG)
			// lookp host iqns
			iqns, errIqn := utils.GetInitiatorIqns()
			if errIqn != nil {
				err = fmt.Errorf("Problem determining host initiator IQNs: %v", errIqn)
				return
			}
			createVAGReq := &sfapi.CreateVolumeAccessGroupRequest{
				Name:       config.DefaultSolidFireVAG,
				Initiators: iqns,
			}
			vagID, vagErr := driver.Client.CreateVolumeAccessGroup(createVAGReq)
			if vagErr != nil {
				err = fmt.Errorf("Problem creating Volume Access Group %s: %v",
					config.DefaultSolidFireVAG, vagErr)
				return
			}
			driver.VagID = vagID
		}*/

	case dvp.EseriesIscsiStorageDriverName:
		driver := storageDriver.(*eseries.EseriesStorageDriver)

		// Override default HostGroup name if it is "netappdvp"
		if driver.Config.AccessGroup == "netappdvp" {
			driver.Config.AccessGroup = config.DefaultEseriesHostGroup
			log.Debugf("Set default E-series HostGroup to %s", config.DefaultEseriesHostGroup)
		}

		// Make sure the Trident Host Group exists
		hostGroup, err := driver.API.GetHostGroup(driver.Config.AccessGroup)
		if err != nil {
			return nil, err
		} else if hostGroup.ClusterRef == "" {
			return nil, fmt.Errorf("Host Group %s doesn't exist for E-Series array %s "+
				"and needs to be manually created! Please also ensure all "+
				"relevant Hosts are defined on the array and added to the Host Group.",
				driver.Config.AccessGroup, driver.Config.ControllerA)
		} else {
			log.WithFields(log.Fields{
				"driver":     dvp.EseriesIscsiStorageDriverName,
				"controller": driver.Config.ControllerA,
				"hostGroup":  hostGroup.Label,
			}).Warnf("Please ensure all relevant hosts are added to Host Group %s.", driver.Config.AccessGroup)
		}

	case fake_driver.FakeStorageDriverName:
	default:
		err = fmt.Errorf("Unknown storage driver: %v",
			commonConfig.StorageDriverName)
		return
	}
	sb, err = storage.NewStorageBackend(storageDriver)
	return
}
