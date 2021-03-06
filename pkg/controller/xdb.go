package controller

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/appscode/log"
	api "github.com/k8sdb/apimachinery/apis/kubedb/v1alpha1"
	"github.com/k8sdb/apimachinery/client/typed/kubedb/v1alpha1/util"
	"github.com/k8sdb/apimachinery/pkg/eventer"
	"github.com/k8sdb/apimachinery/pkg/storage"
	"github.com/k8sdb/xdb/pkg/validator"
	core "k8s.io/api/core/v1"
	kerr "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TODO: Use your resource instead of *tapi.Xdb
func (c *Controller) create(xdb *api.Xdb) error {
	// TODO: Use correct TryPatch method
	_, err := util.TryPatchXdb(c.ExtClient, xdb.ObjectMeta, func(in *api.Xdb) *api.Xdb {
		t := metav1.Now()
		in.Status.CreationTime = &t
		in.Status.Phase = api.DatabasePhaseCreating
		return in
	})

	if err != nil {
		c.recorder.Eventf(xdb.ObjectReference(), core.EventTypeWarning, eventer.EventReasonFailedToUpdate, err.Error())
		return err
	}

	if err := validator.ValidateXdb(c.Client, xdb); err != nil {
		c.recorder.Event(xdb.ObjectReference(), core.EventTypeWarning, eventer.EventReasonInvalid, err.Error())
		return err
	}
	// Event for successful validation
	c.recorder.Event(
		xdb.ObjectReference(),
		core.EventTypeNormal,
		eventer.EventReasonSuccessfulValidate,
		"Successfully validate Xdb",
	)

	// Check DormantDatabase
	matched, err := c.matchDormantDatabase(xdb)
	if err != nil {
		return err
	}
	if matched {
		//TODO: Use Annotation Key
		xdb.Annotations = map[string]string{
			"kubedb.com/ignore": "",
		}
		if err := c.ExtClient.Xdbs(xdb.Namespace).Delete(xdb.Name, &metav1.DeleteOptions{}); err != nil {
			return fmt.Errorf(
				`Failed to resume Xdb "%v" from DormantDatabase "%v". Error: %v`,
				xdb.Name,
				xdb.Name,
				err,
			)
		}

		_, err := util.TryPatchDormantDatabase(c.ExtClient, xdb.ObjectMeta, func(in *api.DormantDatabase) *api.DormantDatabase {
			in.Spec.Resume = true
			return in
		})
		if err != nil {
			c.recorder.Eventf(xdb.ObjectReference(), core.EventTypeWarning, eventer.EventReasonFailedToUpdate, err.Error())
			return err
		}

		return nil
	}

	// Event for notification that kubernetes objects are creating
	c.recorder.Event(xdb.ObjectReference(), core.EventTypeNormal, eventer.EventReasonCreating, "Creating Kubernetes objects")

	// create Governing Service
	governingService := c.opt.GoverningService
	if err := c.CreateGoverningService(governingService, xdb.Namespace); err != nil {
		c.recorder.Eventf(
			xdb.ObjectReference(),
			core.EventTypeWarning,
			eventer.EventReasonFailedToCreate,
			`Failed to create Service: "%v". Reason: %v`,
			governingService,
			err,
		)
		return err
	}

	// ensure database Service
	if err := c.ensureService(xdb); err != nil {
		return err
	}

	// ensure database StatefulSet
	if err := c.ensureStatefulSet(xdb); err != nil {
		return err
	}

	c.recorder.Event(
		xdb.ObjectReference(),
		core.EventTypeNormal,
		eventer.EventReasonSuccessfulCreate,
		"Successfully created Xdb",
	)

	// Ensure Schedule backup
	c.ensureBackupScheduler(xdb)

	if xdb.Spec.Monitor != nil {
		if err := c.addMonitor(xdb); err != nil {
			c.recorder.Eventf(
				xdb.ObjectReference(),
				core.EventTypeWarning,
				eventer.EventReasonFailedToCreate,
				"Failed to add monitoring system. Reason: %v",
				err,
			)
			log.Errorln(err)
			return nil
		}
		c.recorder.Event(
			xdb.ObjectReference(),
			core.EventTypeNormal,
			eventer.EventReasonSuccessfulCreate,
			"Successfully added monitoring system.",
		)
	}
	return nil
}

func (c *Controller) matchDormantDatabase(xdb *api.Xdb) (bool, error) {
	// Check if DormantDatabase exists or not
	dormantDb, err := c.ExtClient.DormantDatabases(xdb.Namespace).Get(xdb.Name, metav1.GetOptions{})
	if err != nil {
		if !kerr.IsNotFound(err) {
			c.recorder.Eventf(
				xdb.ObjectReference(),
				core.EventTypeWarning,
				eventer.EventReasonFailedToGet,
				`Fail to get DormantDatabase: "%v". Reason: %v`,
				xdb.Name,
				err,
			)
			return false, err
		}
		return false, nil
	}

	var sendEvent = func(message string) (bool, error) {
		c.recorder.Event(
			xdb.ObjectReference(),
			core.EventTypeWarning,
			eventer.EventReasonFailedToCreate,
			message,
		)
		return false, errors.New(message)
	}

	// Check DatabaseKind
	// TODO: Change tapi.ResourceKindXdb
	if dormantDb.Labels[api.LabelDatabaseKind] != api.ResourceKindXdb {
		return sendEvent(fmt.Sprintf(`Invalid Xdb: "%v". Exists DormantDatabase "%v" of different Kind`,
			xdb.Name, dormantDb.Name))
	}

	// Check InitSpec
	// TODO: Change tapi.XdbInitSpec
	initSpecAnnotationStr := dormantDb.Annotations[api.XdbInitSpec]
	if initSpecAnnotationStr != "" {
		var initSpecAnnotation *api.InitSpec
		if err := json.Unmarshal([]byte(initSpecAnnotationStr), &initSpecAnnotation); err != nil {
			return sendEvent(err.Error())
		}

		if xdb.Spec.Init != nil {
			if !reflect.DeepEqual(initSpecAnnotation, xdb.Spec.Init) {
				return sendEvent("InitSpec mismatches with DormantDatabase annotation")
			}
		}
	}

	// Check Origin Spec
	drmnOriginSpec := dormantDb.Spec.Origin.Spec.Xdb
	originalSpec := xdb.Spec
	originalSpec.Init = nil

	// ---> Start
	// TODO: Use following part if database secret is supported
	// Otherwise, remove it
	if originalSpec.DatabaseSecret == nil {
		originalSpec.DatabaseSecret = &core.SecretVolumeSource{
			SecretName: xdb.Name + "-admin-auth",
		}
	}
	// ---> End

	if !reflect.DeepEqual(drmnOriginSpec, &originalSpec) {
		return sendEvent("Xdb spec mismatches with OriginSpec in DormantDatabases")
	}

	return true, nil
}

func (c *Controller) ensureService(xdb *api.Xdb) error {
	// Check if service name exists
	found, err := c.findService(xdb)
	if err != nil {
		return err
	}
	if found {
		return nil
	}

	// create database Service
	if err := c.createService(xdb); err != nil {
		c.recorder.Eventf(
			xdb.ObjectReference(),
			core.EventTypeWarning,
			eventer.EventReasonFailedToCreate,
			"Failed to create Service. Reason: %v",
			err,
		)
		return err
	}
	return nil
}

func (c *Controller) ensureStatefulSet(xdb *api.Xdb) error {
	found, err := c.findStatefulSet(xdb)
	if err != nil {
		return err
	}
	if found {
		return nil
	}

	// Create statefulSet for Xdb database
	statefulSet, err := c.createStatefulSet(xdb)
	if err != nil {
		c.recorder.Eventf(
			xdb.ObjectReference(),
			core.EventTypeWarning,
			eventer.EventReasonFailedToCreate,
			"Failed to create StatefulSet. Reason: %v",
			err,
		)
		return err
	}

	// Check StatefulSet Pod status
	if err := c.CheckStatefulSetPodStatus(statefulSet, durationCheckStatefulSet); err != nil {
		c.recorder.Eventf(
			xdb.ObjectReference(),
			core.EventTypeWarning,
			eventer.EventReasonFailedToStart,
			`Failed to create StatefulSet. Reason: %v`,
			err,
		)
		return err
	} else {
		c.recorder.Event(
			xdb.ObjectReference(),
			core.EventTypeNormal,
			eventer.EventReasonSuccessfulCreate,
			"Successfully created StatefulSet",
		)
	}

	if xdb.Spec.Init != nil && xdb.Spec.Init.SnapshotSource != nil {
		// TODO: Use correct TryPatch method
		_, err := util.TryPatchXdb(c.ExtClient, xdb.ObjectMeta, func(in *api.Xdb) *api.Xdb {
			in.Status.Phase = api.DatabasePhaseInitializing
			return in
		})
		if err != nil {
			c.recorder.Eventf(xdb, core.EventTypeWarning, eventer.EventReasonFailedToUpdate, err.Error())
			return err
		}

		if err := c.initialize(xdb); err != nil {
			c.recorder.Eventf(
				xdb.ObjectReference(),
				core.EventTypeWarning,
				eventer.EventReasonFailedToInitialize,
				"Failed to initialize. Reason: %v",
				err,
			)
		}
	}

	// TODO: Use correct TryPatch method
	_, err = util.TryPatchXdb(c.ExtClient, xdb.ObjectMeta, func(in *api.Xdb) *api.Xdb {
		in.Status.Phase = api.DatabasePhaseRunning
		return in
	})
	if err != nil {
		c.recorder.Eventf(xdb, core.EventTypeWarning, eventer.EventReasonFailedToUpdate, err.Error())
		return err
	}
	return nil
}

func (c *Controller) ensureBackupScheduler(xdb *api.Xdb) {
	// Setup Schedule backup
	if xdb.Spec.BackupSchedule != nil {
		err := c.cronController.ScheduleBackup(xdb, xdb.ObjectMeta, xdb.Spec.BackupSchedule)
		if err != nil {
			c.recorder.Eventf(
				xdb.ObjectReference(),
				core.EventTypeWarning,
				eventer.EventReasonFailedToSchedule,
				"Failed to schedule snapshot. Reason: %v",
				err,
			)
			log.Errorln(err)
		}
	} else {
		c.cronController.StopBackupScheduling(xdb.ObjectMeta)
	}
}

const (
	durationCheckRestoreJob = time.Minute * 30
)

func (c *Controller) initialize(xdb *api.Xdb) error {
	snapshotSource := xdb.Spec.Init.SnapshotSource
	// Event for notification that kubernetes objects are creating
	c.recorder.Eventf(
		xdb.ObjectReference(),
		core.EventTypeNormal,
		eventer.EventReasonInitializing,
		`Initializing from Snapshot: "%v"`,
		snapshotSource.Name,
	)

	namespace := snapshotSource.Namespace
	if namespace == "" {
		namespace = xdb.Namespace
	}
	snapshot, err := c.ExtClient.Snapshots(namespace).Get(snapshotSource.Name, metav1.GetOptions{})
	if err != nil {
		return err
	}

	secret, err := storage.NewOSMSecret(c.Client, snapshot)
	if err != nil {
		return err
	}
	_, err = c.Client.CoreV1().Secrets(secret.Namespace).Create(secret)
	if err != nil {
		return err
	}

	job, err := c.createRestoreJob(xdb, snapshot)
	if err != nil {
		return err
	}

	jobSuccess := c.CheckDatabaseRestoreJob(job, xdb, c.recorder, durationCheckRestoreJob)
	if jobSuccess {
		c.recorder.Event(
			xdb.ObjectReference(),
			core.EventTypeNormal,
			eventer.EventReasonSuccessfulInitialize,
			"Successfully completed initialization",
		)
	} else {
		c.recorder.Event(
			xdb.ObjectReference(),
			core.EventTypeWarning,
			eventer.EventReasonFailedToInitialize,
			"Failed to complete initialization",
		)
	}
	return nil
}

func (c *Controller) pause(xdb *api.Xdb) error {
	if xdb.Annotations != nil {
		if val, found := xdb.Annotations["kubedb.com/ignore"]; found {
			//TODO: Add Event Reason "Ignored"
			c.recorder.Event(xdb.ObjectReference(), core.EventTypeNormal, "Ignored", val)
			return nil
		}
	}

	c.recorder.Event(xdb.ObjectReference(), core.EventTypeNormal, eventer.EventReasonPausing, "Pausing Xdb")

	if xdb.Spec.DoNotPause {
		c.recorder.Eventf(
			xdb.ObjectReference(),
			core.EventTypeWarning,
			eventer.EventReasonFailedToPause,
			`Xdb "%v" is locked.`,
			xdb.Name,
		)

		if err := c.reCreateXdb(xdb); err != nil {
			c.recorder.Eventf(
				xdb.ObjectReference(),
				core.EventTypeWarning,
				eventer.EventReasonFailedToCreate,
				`Failed to recreate Xdb: "%v". Reason: %v`,
				xdb.Name,
				err,
			)
			return err
		}
		return nil
	}

	if _, err := c.createDormantDatabase(xdb); err != nil {
		c.recorder.Eventf(
			xdb.ObjectReference(),
			core.EventTypeWarning,
			eventer.EventReasonFailedToCreate,
			`Failed to create DormantDatabase: "%v". Reason: %v`,
			xdb.Name,
			err,
		)
		return err
	}
	c.recorder.Eventf(
		xdb.ObjectReference(),
		core.EventTypeNormal,
		eventer.EventReasonSuccessfulCreate,
		`Successfully created DormantDatabase: "%v"`,
		xdb.Name,
	)

	c.cronController.StopBackupScheduling(xdb.ObjectMeta)

	if xdb.Spec.Monitor != nil {
		if err := c.deleteMonitor(xdb); err != nil {
			c.recorder.Eventf(
				xdb.ObjectReference(),
				core.EventTypeWarning,
				eventer.EventReasonFailedToDelete,
				"Failed to delete monitoring system. Reason: %v",
				err,
			)
			log.Errorln(err)
			return nil
		}
		c.recorder.Event(
			xdb.ObjectReference(),
			core.EventTypeNormal,
			eventer.EventReasonSuccessfulMonitorDelete,
			"Successfully deleted monitoring system.",
		)
	}
	return nil
}

func (c *Controller) update(oldXdb, updatedXdb *api.Xdb) error {
	if err := validator.ValidateXdb(c.Client, updatedXdb); err != nil {
		c.recorder.Event(updatedXdb.ObjectReference(), core.EventTypeWarning, eventer.EventReasonInvalid, err.Error())
		return err
	}
	// Event for successful validation
	c.recorder.Event(
		updatedXdb.ObjectReference(),
		core.EventTypeNormal,
		eventer.EventReasonSuccessfulValidate,
		"Successfully validate Xdb",
	)

	if err := c.ensureService(updatedXdb); err != nil {
		return err
	}
	if err := c.ensureStatefulSet(updatedXdb); err != nil {
		return err
	}

	if !reflect.DeepEqual(updatedXdb.Spec.BackupSchedule, oldXdb.Spec.BackupSchedule) {
		c.ensureBackupScheduler(updatedXdb)
	}

	if !reflect.DeepEqual(oldXdb.Spec.Monitor, updatedXdb.Spec.Monitor) {
		if err := c.updateMonitor(oldXdb, updatedXdb); err != nil {
			c.recorder.Eventf(
				updatedXdb.ObjectReference(),
				core.EventTypeWarning,
				eventer.EventReasonFailedToUpdate,
				"Failed to update monitoring system. Reason: %v",
				err,
			)
			log.Errorln(err)
			return nil
		}
		c.recorder.Event(
			updatedXdb.ObjectReference(),
			core.EventTypeNormal,
			eventer.EventReasonSuccessfulMonitorUpdate,
			"Successfully updated monitoring system.",
		)

	}
	return nil
}
