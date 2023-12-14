/*
Copyright 2023.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/lcenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package backuprestore

import (
	"context"
	"errors"
	"fmt"
	"math"

	"github.com/go-logr/logr"

	"github.com/openshift-kni/lifecycle-agent/internal/common"

	lcav1alpha1 "github.com/openshift-kni/lifecycle-agent/api/v1alpha1"

	configv1 "github.com/openshift/api/config/v1"
	operatorsv1alpha1 "github.com/operator-framework/api/pkg/operators/v1alpha1"
	velerov1 "github.com/vmware-tanzu/velero/pkg/apis/velero/v1"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;create;update;watch
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=delete
// +kubebuilder:rbac:groups=config.openshift.io,resources=clusterversions,verbs=get;list;watch
// +kubebuilder:rbac:groups=velero.io,resources=backups,verbs=get;list;delete;create;update;watch
// +kubebuilder:rbac:groups=velero.io,resources=restores,verbs=get;list;delete;create;update;watch
// +kubebuilder:rbac:groups=velero.io,resources=backupstoragelocations,verbs=get;list;watch
// +kubebuilder:rbac:groups=velero.io,resources=deletebackuprequests,verbs=get;list;delete;create;update;watch
// +kubebuilder:rbac:groups=operators.coreos.com,resources=subscriptions,verbs=get;list;delete;watch
// +kubebuilder:rbac:groups=operators.coreos.com,resources=clusterserviceversions,verbs=get;list;delete;watch
// +kubebuilder:rbac:groups=oadp.openshift.io,resources=dataprotectionapplications,verbs=get;list;create;update;watch

const (
	applyWaveAnn     = "lca.openshift.io/apply-wave"
	clusterIDLabel   = "config.openshift.io/clusterID" // label for backups applied by lifecycle agent
	defaultApplyWave = math.MaxInt32                   // 2147483647, an enough large number

	OadpPath        = "/opt/OADP"
	OadpRestorePath = OadpPath + "/veleroRestore"
	oadpDpaPath     = OadpPath + "/dpa"
	oadpSecretPath  = OadpPath + "/secret"

	// OadpNs is the namespace used for everything related OADP e.g configsMaps, DataProtectionApplicationm, Restore, etc
	OadpNs = "openshift-adp"
)

var (
	hostPath = common.Host

	dpaGvk     = schema.GroupVersionKind{Group: "oadp.openshift.io", Kind: "DataProtectionApplication", Version: "v1alpha1"}
	dpaGvkList = schema.GroupVersionKind{Group: "oadp.openshift.io", Kind: "DataProtectionApplicationList", Version: "v1alpha1"}
	backupGvk  = schema.GroupVersionKind{Group: "velero.io", Kind: "Backup", Version: "v1"}
	restoreGvk = schema.GroupVersionKind{Group: "velero.io", Kind: "Restore", Version: "v1"}
)

// BackuperRestorer interface also used for mocks
type BackuperRestorer interface {
	CleanupBackups(ctx context.Context) (bool, error)
	DeleteOadpOperator(ctx context.Context, namespace string) error
	ExportOadpConfigurationToDir(ctx context.Context, toDir, oadpNamespace string) error
	ExportRestoresToDir(ctx context.Context, configMaps []lcav1alpha1.ConfigMapRef, toDir string) error
	GetSortedBackupsFromConfigmap(ctx context.Context, content []lcav1alpha1.ConfigMapRef) ([][]*velerov1.Backup, error)
	LoadRestoresFromOadpRestorePath() ([][]*velerov1.Restore, error)
	RestoreOadpConfigurations(ctx context.Context) error
	StartOrTrackBackup(ctx context.Context, backups []*velerov1.Backup) (*BackupTracker, error)
	StartOrTrackRestore(ctx context.Context, restores []*velerov1.Restore) (*RestoreTracker, error)
}

// BRHandler handles the backup and restore
type BRHandler struct {
	client.Client
	Log logr.Logger
}

// BRStatusError type
type BRStatusError struct {
	Type       string
	Reason     string
	ErrMessage string
}

func (e *BRStatusError) Error() string {
	return fmt.Sprintf(e.ErrMessage)
}

func NewBRNotFoundError(msg string) *BRStatusError {
	return &BRStatusError{
		Type:       "configmap",
		Reason:     "NotFound",
		ErrMessage: msg,
	}
}

func NewBRFailedError(brType, msg string) *BRStatusError {
	return &BRStatusError{
		Type:       brType,
		Reason:     "Failed",
		ErrMessage: msg,
	}
}

func NewBRFailedValidationError(brType, msg string) *BRStatusError {
	return &BRStatusError{
		Type:       brType,
		Reason:     "FailedValidation",
		ErrMessage: msg,
	}
}

func NewBRStorageBackendUnavailableError(msg string) *BRStatusError {
	return &BRStatusError{
		Type:       "StorageBackend",
		Reason:     "Unavailable",
		ErrMessage: msg,
	}
}

func IsBRNotFoundError(err error) bool {
	var brErr *BRStatusError
	if errors.As(err, &brErr) {
		if brErr.Type == "configmap" {
			return brErr.Reason == "NotFound"
		}
	}
	return false
}

func IsBRFailedError(err error) bool {
	var brErr *BRStatusError
	if errors.As(err, &brErr) {
		if brErr.Type == "Backup" || brErr.Type == "Restore" || brErr.Type == "OADP" {
			return brErr.Reason == "Failed"
		}
	}
	return false
}

func IsBRFailedValidationError(err error) bool {
	var brErr *BRStatusError
	if errors.As(err, &brErr) {
		if brErr.Type == "Backup" || brErr.Type == "Restore" {
			return brErr.Reason == "FailedValidation"
		}
	}
	return false
}

func IsBRStorageBackendUnavailableError(err error) bool {
	var brErr *BRStatusError
	if errors.As(err, &brErr) {
		if brErr.Type == "StorageBackend" {
			return brErr.Reason == "Unavailable"
		}
	}
	return false
}

func getBackup(ctx context.Context, c client.Client, name, namespace string) (*velerov1.Backup, error) {
	backup := &velerov1.Backup{}
	if err := c.Get(ctx, types.NamespacedName{
		Name:      name,
		Namespace: namespace,
	}, backup); err != nil {
		if k8serrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}

	return backup, nil
}

func getClusterID(ctx context.Context, c client.Client) (string, error) {

	clusterVersion := &configv1.ClusterVersion{}
	if err := c.Get(ctx, types.NamespacedName{
		Name: "version",
	}, clusterVersion); err != nil {
		return "", err
	}

	return string(clusterVersion.Spec.ClusterID), nil
}

func setBackupLabel(backup *velerov1.Backup, newLabels map[string]string) {
	labels := backup.GetLabels()
	if labels == nil {
		labels = make(map[string]string)
	}

	for k, v := range newLabels {
		labels[k] = v
	}
	backup.SetLabels(labels)
}

// DeleteOadpOperator deletes the oadp operator
func (h *BRHandler) DeleteOadpOperator(ctx context.Context, namespace string) error {
	// Should only be one oadp subscription in the namespace
	listOpts := []client.ListOption{
		client.InNamespace(namespace),
		client.HasLabels{"operators.coreos.com/redhat-oadp-operator." + namespace},
	}

	// Ensure that the dependent resources are deleted
	deleteOpts := []client.DeleteOption{
		client.PropagationPolicy(metav1.DeletePropagationForeground),
	}

	oadpSub := &operatorsv1alpha1.SubscriptionList{}
	if err := h.List(ctx, oadpSub, listOpts...); err == nil {
		for _, sub := range oadpSub.Items {
			if err := h.Delete(ctx, &sub, deleteOpts...); err != nil {
				return err
			}
		}
	} else {
		return err
	}

	oadpCsv := &operatorsv1alpha1.ClusterServiceVersionList{}
	if err := h.List(ctx, oadpCsv, listOpts...); err == nil {
		for _, csv := range oadpCsv.Items {
			if err := h.Delete(ctx, &csv, deleteOpts...); err != nil {
				return err
			}
		}
	} else {
		return err
	}

	if err := h.Delete(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: namespace,
		}}, deleteOpts...); err != nil {
		if !k8serrors.IsNotFound(err) {
			return err
		}
	}

	if len(oadpSub.Items) == 0 {
		h.Log.Info("Found no OADP operator to delete")
	} else if len(oadpSub.Items) == 1 {
		h.Log.Info("OADP operator has been deleted", "name", oadpSub.Items[0].Name, "namespace", namespace)
	} else {
		h.Log.Info("WARN: Found more than 1 OADP operator. Deleted all OADP operators.")
	}
	return nil
}

func isDPAReconciled(dpa *unstructured.Unstructured) bool {
	if dpa.Object["status"] == nil {
		return false
	}

	dpaStatus := dpa.Object["status"].(map[string]interface{})
	if dpaStatus["conditions"] == nil {
		return false
	}

	dpaStatusConditions := dpaStatus["conditions"].([]interface{})
	for _, condition := range dpaStatusConditions {
		conditionMap := condition.(map[string]interface{})
		if conditionMap["type"] == "Reconciled" {
			return conditionMap["status"] == "True"
		}
	}
	return false
}
