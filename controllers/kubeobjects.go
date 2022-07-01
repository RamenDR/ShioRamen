/*
Copyright 2022 The RamenDR authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

	http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// +kubebuilder:rbac:groups=velero.io,resources=backups,verbs=create;delete;get;patch;update
// +kubebuilder:rbac:groups=velero.io,resources=backupstoragelocations,verbs=create;delete;get;patch;update
// +kubebuilder:rbac:groups=velero.io,resources=deletebackuprequests,verbs=create;delete;get;patch;update
// +kubebuilder:rbac:groups=velero.io,resources=downloadrequests,verbs=create;delete;get;patch;update
// +kubebuilder:rbac:groups=velero.io,resources=restores,verbs=create;delete;get;patch;update

package controllers

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/go-logr/logr"
	pkgerrors "github.com/pkg/errors"
	velero "github.com/vmware-tanzu/velero/pkg/apis/velero/v1"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// velero specific
func getBackupPrefix() string {
	return "backups/"
}

func ParseLastSlash(input string) string {
	split := strings.Split(input, "/")

	return split[len(split)-1]
}

func getResourcesTypesFromBackup(objectStore ObjectStorer, backupName string) ([]string, error) {
	// download resource list and check against it
	// resource list: s3/bucket/backups/backupName/backupName-resource-list.json.gz
	key := fmt.Sprintf("%s%s/%s-resource-list.json.gz", getBackupPrefix(), backupName, backupName)

	// resource-list uses this format: {"type1":["name1", "name2"], "type2":["name3"]}  ...etc
	var foundTypes map[string][]string

	err := objectStore.DownloadObject(key, &foundTypes)

	availableTypes := make([]string, 0)
	for fullType := range foundTypes {
		availableTypes = append(availableTypes, ParseLastSlash(fullType))
		// v.log.Info(fmt.Sprintf("key=%s", string(fullType)))
	}

	if err != nil {
		return availableTypes, pkgerrors.Wrap(err, "getResourcesTypesFromBackup")
	}

	return availableTypes, nil
}

func kubeObjectsRecover(
	ctx context.Context,
	writer client.Writer,
	reader client.Reader,
	log logr.Logger,
	s3Url string,
	s3BucketName string,
	s3KeyPrefix string,
	sourceNamespaceName string,
	targetNamespaceName string,
	veleroNamespaceName string,
	includedResourceList []string,
	excludedResourceList []string,
	restoreName string,
	backupSource string,
) error {
	log.Info("kube objects recover",
		"s3 url", s3Url,
		"s3 bucket", s3BucketName,
		"s3 key prefix", s3KeyPrefix,
		"source namespace", sourceNamespaceName,
		"target namespace", targetNamespaceName,
	)

	backupNamespacedName, err := namespacedName(veleroNamespaceName, backupSource, s3Url, s3BucketName)
	if err != nil {
		return err
	}

	return backupDummyCreateAndIfAlreadyExistsThenRestore(
		backupNamespacedName,
		objectWriter{ctx: ctx, Writer: writer, log: log},
		reader,
		sourceNamespaceName,
		targetNamespaceName,
		includedResourceList,
		excludedResourceList,
		restoreName,
		backupSource,
	)
}

func backupDummyCreateAndIfAlreadyExistsThenRestore(
	backupNamespacedName types.NamespacedName,
	w objectWriter,
	reader client.Reader,
	sourceNamespaceName string,
	targetNamespaceName string,
	includedResourceList []string,
	excludedResourceList []string,
	restoreName string,
	backupSource string,
) error {
	// backStorageLocationNamespacedName := getBackupStorageLocationNamespacedName(namespacedName.Namespace)
	// backupLocation := backupLocation(backStorageLocationNamespacedName, s3Url, s3BucketName, s3KeyPrefix)
	// maybe re-use backup location?
	/*if err := w.objectCreate(backupLocation); err != nil {
		return err
	}
	*/
	backup, err := w.backupCreate(
		backupNamespacedName, backupSpecDummy(),
	)
	if err != nil {
		return err
	}

	if err := reader.Get(w.ctx, backupNamespacedName, backup); err != nil {
		return pkgerrors.Wrap(err, "dummy backup get")
	}

	restoreNamespacedName := types.NamespacedName{Name: restoreName, Namespace: backup.Namespace}

	err = backupRestore(backup, restoreNamespacedName, w, reader, sourceNamespaceName, targetNamespaceName,
		includedResourceList, excludedResourceList, backupSource)
	if err != nil {
		return pkgerrors.Wrap(err, "backupRestore")
	}

	return backupDummyStatusProcess(backup, restoreNamespacedName, w, reader,
		sourceNamespaceName, targetNamespaceName)
}

func backupDummyStatusProcess(
	backup *velero.Backup,
	namespacedName types.NamespacedName,
	w objectWriter,
	reader client.Reader,
	sourceNamespaceName string,
	targetNamespaceName string,
) error {
	backupStatusLog(backup, w.log)

	switch backup.Status.Phase {
	case velero.BackupPhaseCompleted:
		// deletes backup and backup object TODO delete bsl also after both are gone?
		return w.objectCreate(backupDeletion(namespacedName))
	case velero.BackupPhasePartiallyFailed:
		fallthrough
	case velero.BackupPhaseFailed:
		// TODO if failed because backup already exists
		temp := make([]string, 0) // TJanssen - see if this is needed or not...

		return backupRestore(backup, namespacedName, w, reader, sourceNamespaceName,
			targetNamespaceName, temp, temp, namespacedName.Name)
	case velero.BackupPhaseNew:
		fallthrough
	case velero.BackupPhaseInProgress:
		fallthrough
	case velero.BackupPhaseUploading:
		fallthrough
	case velero.BackupPhaseUploadingPartialFailure:
		fallthrough
	case velero.BackupPhaseDeleting:
		return errors.New("temporary: backup" + string(backup.Status.Phase))
	case velero.BackupPhaseFailedValidation:
		return errors.New("permanent: backup" + string(backup.Status.Phase))
	}

	return errors.New("temporary: backup.status.phase absent")
}

func BackupAndBackupObjectsDelete(
	backupLocation *velero.BackupStorageLocation,
	backup *velero.Backup,
	namespacedName types.NamespacedName,
	w objectWriter,
	reader client.Reader,
) error {
	if err := backupDelete(namespacedName, w, reader); err != nil {
		return err
	}

	return w.backupObjectsDelete(backup)
}

func backupDelete(
	namespacedName types.NamespacedName,
	w objectWriter,
	reader client.Reader,
) error {
	backupDeletion := backupDeletion(namespacedName)
	if err := w.objectCreate(backupDeletion); err != nil {
		return err
	}

	if err := reader.Get(w.ctx, namespacedName, backupDeletion); err != nil {
		return pkgerrors.Wrap(err, "backup deletion get")
	}

	backupDeletionStatusLog(backupDeletion, w.log)

	switch backupDeletion.Status.Phase {
	case velero.DeleteBackupRequestPhaseNew:
		fallthrough
	case velero.DeleteBackupRequestPhaseInProgress:
		return errors.New("temporary: backup deletion " + string(backupDeletion.Status.Phase))
	case velero.DeleteBackupRequestPhaseProcessed:
		return w.objectDelete(backupDeletion)
	default:
		return errors.New("temporary: backup deletion status.phase absent")
	}
}

func backupRestore(
	backup *velero.Backup,
	namespacedName types.NamespacedName,
	w objectWriter,
	reader client.Reader,
	sourceNamespaceName string,
	targetNamespaceName string,
	includedResourceList []string,
	excludedResourceList []string,
	backupSource string,
) error {
	restore := restore(namespacedName, sourceNamespaceName, targetNamespaceName,
		includedResourceList, excludedResourceList, backupSource)
	if err := w.objectCreate(restore); err != nil {
		return err
	}

	if err := reader.Get(w.ctx, namespacedName, restore); err != nil {
		return pkgerrors.Wrap(err, "restore get")
	}

	return restoreStatusProcess(backup, restore, w)
}

func restoreStatusProcess(
	backup *velero.Backup,
	restore *velero.Restore,
	w objectWriter,
) error {
	restoreStatusLog(restore, w.log)

	switch restore.Status.Phase {
	case velero.RestorePhaseNew:
		fallthrough
	case velero.RestorePhaseInProgress:
		return errors.New("temporary: restore" + string(restore.Status.Phase))
	case velero.RestorePhaseFailed:
		backupObjectPathName := "backups/" + backup.Name + "/" + backup.Name + ".tar.gz"
		if strings.HasPrefix(restore.Status.FailureReason,
			"error downloading backup: error copying Backup to temp file: rpc error: "+
				"code = Unknown desc = error getting object "+backupObjectPathName+": "+
				"NoSuchKey: The specified key does not exist.\n\tstatus code: 404, request id: ",
		) {
			w.log.Info("backup absent", "path", backupObjectPathName)

			return w.restoreObjectsDelete(restore)
		}

		fallthrough
	case velero.RestorePhaseFailedValidation:
		fallthrough
	case velero.RestorePhasePartiallyFailed:
		return errors.New("permanent: restore" + string(restore.Status.Phase))
	case velero.RestorePhaseCompleted:
		// return w.restoreObjectsDelete(restore)
		return nil // keep restore objects around -TJanssen
	default:
		return errors.New("temporary: restore.status.phase absent")
	}
}

func RestoreResultsGet(
	w objectWriter,
	reader client.Reader,
	namespacedName types.NamespacedName,
	results *string,
) error {
	download := download(namespacedName, velero.DownloadTargetKindRestoreResults)
	if err := w.objectCreate(download); err != nil {
		return err
	}

	if err := reader.Get(w.ctx, namespacedName, download); err != nil {
		return pkgerrors.Wrap(err, "restore results download get")
	}

	downloadStatusLog(download, w.log)

	switch download.Status.Phase {
	case velero.DownloadRequestPhaseNew:
		return errors.New("temporary: download " + string(download.Status.Phase))
	case velero.DownloadRequestPhaseProcessed:
		break
	default:
		return errors.New("temporary: download status.phase absent")
	}

	*results = download.Status.DownloadURL // TODO dereference

	return w.objectDelete(download)
}

func kubeObjectsProtect(
	ctx context.Context,
	writer client.Writer,
	reader client.Reader,
	log logr.Logger,
	s3Url string,
	s3BucketName string,
	s3KeyPrefix string,
	sourceNamespaceName string,
	veleroNamespaceName string,
	includedResourceList []string,
	excludedResourceList []string,
	backupName string,
) error {
	log.Info("kube objects protect",
		"s3 url", s3Url,
		"s3 bucket", s3BucketName,
		"s3 key prefix", s3KeyPrefix,
		"source namespace", sourceNamespaceName,
	)

	namespacedName, err := namespacedName(veleroNamespaceName, backupName, s3Url, s3BucketName)
	if err != nil {
		return err
	}

	return backupRealCreate(
		namespacedName,
		objectWriter{ctx: ctx, Writer: writer, log: log},
		reader,
		s3Url,
		s3BucketName,
		s3KeyPrefix,
		sourceNamespaceName,
		includedResourceList,
		excludedResourceList,
	)
}

func backupRealCreate(
	namespacedName types.NamespacedName,
	w objectWriter,
	reader client.Reader,
	s3Url string,
	s3BucketName string,
	s3KeyPrefix string,
	sourceNamespaceName string,
	includedResourceList []string,
	excludedResourceList []string,
) error {
	backStorageLocationNamespacedName := getBackupStorageLocationNamespacedName(namespacedName.Namespace)
	backupLocation := backupLocation(backStorageLocationNamespacedName, s3Url, s3BucketName, s3KeyPrefix)
	/*if err := w.objectCreate(backupLocation); err != nil {
		return err
	}*/

	// TODO: is there a better way to accomplish this?
	backupSpec := velero.BackupSpec{}
	backupSpec.StorageLocation = backupLocation.Name
	backupSpec.IncludedNamespaces = []string{sourceNamespaceName}
	backupSpec.IncludedResources = includedResourceList
	backupSpec.ExcludedResources = excludedResourceList

	backup, err := w.backupCreate(
		namespacedName, backupSpec,
	)
	if err != nil {
		return err
	}

	if err := reader.Get(w.ctx, namespacedName, backup); err != nil {
		return pkgerrors.Wrap(err, "backup get")
	}

	// return backupRealStatusProcess(backupLocation, backup, w)
	return nil
}

// complete here could mean success or failure; but processing is over.
func backupIsDone(ctx context.Context, apiReader client.Reader, writer objectWriter,
	namespacedName types.NamespacedName) (bool, error) {
	backup, err := getBackupObject(ctx, apiReader, namespacedName)
	if err != nil {
		return true, err
	}

	err = backupRealStatusProcess(backup, writer)
	if err != nil {
		return false, err
	}

	completed := backup.Status.Phase == velero.BackupPhaseCompleted ||
		backup.Status.Phase == velero.BackupPhasePartiallyFailed ||
		backup.Status.Phase == velero.BackupPhaseFailed

	return completed, nil
}

func restoreIsDone(ctx context.Context, apiReader client.Reader,
	namespacedName types.NamespacedName) (bool, error) {
	restore := &velero.Restore{}

	err := apiReader.Get(ctx, namespacedName, restore)
	// err = restoreStatusProcess(restore, writer)
	if err != nil {
		return false, pkgerrors.Wrap(err, "restoreIsDone")
	}

	completed := restore.Status.Phase == velero.RestorePhaseFailed ||
		restore.Status.Phase == velero.RestorePhasePartiallyFailed ||
		restore.Status.Phase == velero.RestorePhaseCompleted

	return completed, nil
}

func getBackupObject(ctx context.Context, apiReader client.Reader, namespacedName types.NamespacedName) (
	*velero.Backup, error) {
	backup := &velero.Backup{}

	return backup, apiReader.Get(ctx, namespacedName, backup)
}

func backupRealStatusProcess(
	backup *velero.Backup,
	w objectWriter,
) error {
	backupStatusLog(backup, w.log)

	switch backup.Status.Phase {
	case velero.BackupPhaseCompleted:
		// return w.backupObjectsDelete(backupLocation, backup)  // TODO: delete when all backups complete
		return nil
	case velero.BackupPhaseNew:
		fallthrough
	case velero.BackupPhaseInProgress:
		fallthrough
	case velero.BackupPhaseUploading:
		fallthrough
	case velero.BackupPhaseUploadingPartialFailure:
		fallthrough
	case velero.BackupPhaseDeleting:
		return errors.New("temporary: backup" + string(backup.Status.Phase))
	case velero.BackupPhaseFailedValidation:
		fallthrough
	case velero.BackupPhasePartiallyFailed:
		fallthrough
	case velero.BackupPhaseFailed:
		// return errors.New("permanent: backup" + string(backup.Status.Phase))
		return backupRetry(backup, w)
	}

	return errors.New("temporary: backup.status.phase absent")
}

func backupRetry(
	backup *velero.Backup,
	w objectWriter,
) error {
	if err := w.backupObjectsDelete(backup); err != nil {
		return err
	}

	return errors.New("backup retry")
}

func getBackupStorageLocationNamespacedName(namespace string) types.NamespacedName {
	return types.NamespacedName{
		Namespace: namespace,
		Name:      "default",
	}
}

func namespacedName(namespaceName, name, s3Url, s3BucketName string) (
	types.NamespacedName, error) {
	if true {
		return types.NamespacedName{Namespace: namespaceName, Name: name}, nil
	}

	url, err := url.Parse(s3Url)
	if err != nil {
		return types.NamespacedName{}, pkgerrors.Wrap(err, "s3 url parse")
	}

	// https://kubernetes.io/docs/concepts/overview/working-with-objects/names/#dns-label-names
	// https://docs.aws.amazon.com/AmazonS3/latest/userguide/WebsiteEndpoints.html
	// https://docs.aws.amazon.com/AmazonS3/latest/userguide/bucketnamingrules.html
	return types.NamespacedName{
			Namespace: namespaceName,
			Name: strings.ReplaceAll(url.Hostname(), ".", "-") +
				"-" + url.Port() +
				"-" + strings.ReplaceAll(s3BucketName, ".", "-"),
		},
		nil
}

type objectWriter struct {
	client.Writer
	ctx context.Context
	log logr.Logger
}

func (w objectWriter) backupCreate(
	namespacedName types.NamespacedName,
	backupSpec velero.BackupSpec,
) (*velero.Backup, error) {
	backup := backup(namespacedName, backupSpec)
	if err := w.objectCreate(backup); err != nil {
		return backup, err
	}

	return backup, nil
}

func (w objectWriter) backupObjectsDelete(
	backup *velero.Backup,
) error {
	err := w.objectDelete(backup)

	return err
}

func (w objectWriter) restoreObjectsDelete(
	restore *velero.Restore,
) error {
	err := w.objectDelete(restore)

	return err
}

func (w objectWriter) objectCreate(o client.Object) error {
	if err := w.Create(w.ctx, o); err != nil {
		if !k8serrors.IsAlreadyExists(err) {
			return pkgerrors.Wrap(err, "object create")
		}

		w.log.Info("object created previously", "type", o.GetObjectKind(), "name", o.GetName())
	} else {
		w.log.Info("object created successfully", "type", o.GetObjectKind(), "name", o.GetName())
	}

	return nil
}

func (w objectWriter) objectDelete(o client.Object) error {
	if err := w.Delete(w.ctx, o); err != nil {
		if !k8serrors.IsNotFound(err) {
			return pkgerrors.Wrap(err, "object delete")
		}

		w.log.Info("object deleted previously", "type", o.GetObjectKind(), "name", o.GetName())
	} else {
		w.log.Info("object deleted successfully", "type", o.GetObjectKind(), "name", o.GetName())
	}

	return nil
}

const (
	secretName    = "s3secret"
	secretKeyName = "aws"
)

func backupLocation(namespacedName types.NamespacedName, s3Url, bucketName, s3KeyPrefix string,
) *velero.BackupStorageLocation {
	return &velero.BackupStorageLocation{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "velero.io/v1",
			Kind:       "BackupStorageLocation",
		},
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespacedName.Namespace,
			Name:      namespacedName.Name,
		},
		Spec: velero.BackupStorageLocationSpec{
			Provider: "aws",
			StorageType: velero.StorageType{
				ObjectStorage: &velero.ObjectStorageLocation{
					Bucket: bucketName,
					Prefix: s3KeyPrefix,
				},
			},
			Config: map[string]string{
				"region":           "us-east-1", // TODO input
				"s3ForcePathStyle": "true",
				"s3Url":            s3Url,
			},
			Credential: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{
					Name: secretName,
				},
				Key: secretKeyName,
			},
		},
	}
}

func backup(namespacedName types.NamespacedName, backupSpec velero.BackupSpec) *velero.Backup {
	return &velero.Backup{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "velero.io/v1",
			Kind:       "Backup",
		},
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespacedName.Namespace,
			Name:      namespacedName.Name,
		},
		Spec: backupSpec,
	}
}

func backupSpecDummy() velero.BackupSpec {
	return velero.BackupSpec{
		IncludedResources: []string{"secrets"},
		LabelSelector: &metav1.LabelSelector{
			MatchLabels: map[string]string{
				"dummyKey": "dummyValue",
			},
		},
	}
}

func restore(namespacedName types.NamespacedName, sourceNamespaceName, targetNamespaceName string,
	includedResourceList, excludedResourceList []string, backupSource string) *velero.Restore {
	return &velero.Restore{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "velero.io/v1",
			Kind:       "Restore",
		},
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespacedName.Namespace,
			Name:      namespacedName.Name,
		},
		Spec: velero.RestoreSpec{
			BackupName:        backupSource,
			NamespaceMapping:  map[string]string{sourceNamespaceName: targetNamespaceName},
			IncludedResources: includedResourceList,
			ExcludedResources: excludedResourceList,
			/*ExcludedResources: []string{
				"CustomResourceDefinitions",
				"VolumeReplicationGroups",
				"VolumeReplications",
			},*/
		},
	}
}

func backupDeletion(namespacedName types.NamespacedName) *velero.DeleteBackupRequest {
	return &velero.DeleteBackupRequest{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "velero.io/v1",
			Kind:       "DeleteBackupRequest",
		},
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespacedName.Namespace,
			Name:      namespacedName.Name,
		},
		Spec: velero.DeleteBackupRequestSpec{
			BackupName: namespacedName.Name,
		},
	}
}

func download(namespacedName types.NamespacedName, kind velero.DownloadTargetKind) *velero.DownloadRequest {
	return &velero.DownloadRequest{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "velero.io/v1",
			Kind:       "DeleteBackupRequest",
		},
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespacedName.Namespace,
			Name:      namespacedName.Name + string(kind),
		},
		Spec: velero.DownloadRequestSpec{
			Target: velero.DownloadTarget{
				Kind: kind,
				Name: namespacedName.Name,
			},
		},
	}
}

func backupStatusLog(backup *velero.Backup, log logr.Logger) {
	log.Info("backup",
		"phase", backup.Status.Phase,
		"warnings", backup.Status.Warnings,
		"errors", backup.Status.Errors,
	)

	if backup.Status.StartTimestamp != nil {
		log.Info("backup", "start", backup.Status.StartTimestamp)
	}

	if backup.Status.CompletionTimestamp != nil {
		log.Info("backup", "finish", backup.Status.CompletionTimestamp)
	}

	if backup.Status.Progress != nil {
		log.Info("items",
			"to be backed up", backup.Status.Progress.TotalItems,
			"backed up", backup.Status.Progress.ItemsBackedUp,
		)
	}
}

func restoreStatusLog(restore *velero.Restore, log logr.Logger) {
	log.Info("restore",
		"phase", restore.Status.Phase,
		"warnings", restore.Status.Warnings,
		"errors", restore.Status.Errors,
		"failure", restore.Status.FailureReason,
	)

	if restore.Status.StartTimestamp != nil {
		log.Info("restore", "start", restore.Status.StartTimestamp)
	}

	if restore.Status.CompletionTimestamp != nil {
		log.Info("restore", "finish", restore.Status.CompletionTimestamp)
	}

	if restore.Status.Progress != nil {
		log.Info("items",
			"to be restored", restore.Status.Progress.TotalItems,
			"restored", restore.Status.Progress.ItemsRestored,
		)
	}
}

func backupDeletionStatusLog(backupDeletion *velero.DeleteBackupRequest, log logr.Logger) {
	log.Info("backup deletion",
		"phase", backupDeletion.Status.Phase,
		"errors", len(backupDeletion.Status.Errors),
	)

	for i, message := range backupDeletion.Status.Errors {
		log.Info("backup deletion error", "number", i, "message", message)
	}
}

func downloadStatusLog(download *velero.DownloadRequest, log logr.Logger) {
	log.Info("download",
		"phase", download.Status.Phase,
		"url", download.Status.DownloadURL,
	)

	if download.Status.Expiration != nil {
		log.Info("expiration", download.Status.Expiration)
	}
}
