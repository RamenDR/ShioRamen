# VRG Type Sequence

## Overview

RamenDR can use Velero to capture Kubernetes object information. By default,
Velero will back up all object types in arbitrary order. However, many
applications have internal dependencies that require specific ordering by
object type. The VRG Type Sequencer attempts to address this deficiency by
taking multiple partial backups, with the order defined a the user.

## Example Use Case: Backup

### Backup Overview

Take an example Backup that requires the following sequence:

1) Deployments first
2) Any resource types that match suffix *.cpd.ibm.com
3) Secrets and ConfigMaps after that (order unimportant)
4) Anything else after that

### YAML example

```yaml
apiVersion: ramendr.openshift.io/v1alpha1
kind: VolumeReplicationGroup
metadata:
  name: volumereplicationgroup-sample
spec:
  ...
  # Type Sequence section
  kubeObjectProtection:
    resourceCaptureOrder:
      - Name: config  # backup Names should be unique
        includeClusterScopedResources: true
        includedResources: ["ConfigMap", "Secret"]
        labelSelector:
          app: my-app
      - Name: cpd
        includedResources: ["sample1.cpd.ibm.com", "sample2.cpd.ibm.com", "sample3.cpd.ibm.com"]
        # labelSelector: "" # intentionally omitted - doesn't require label match
        # includeClusterScopedResources: false # by default
      - Name: deployments
        includedResources: ["Deployment"]
      - Name: everything
        excludedResources: ""  # include everything with no history, even resources in other backups
```

## Example Use Case: Restore

### Restore Overview

Take an example Restore that requires the following sequence:

1) Secrets and ConfigMaps before anything else (but in any order)
2) Any resource matching the suffix cpd.ibm.com
3) Any resource that isn't a Deployment
4) Anything else

### YAML example

```yaml
apiVersion: ramendr.openshift.io/v1alpha1
kind: VolumeReplicationGroup
metadata:
  name: volumereplicationgroup-sample
spec:
  ...

  # Type Sequence section
  KubeObjectProtection:
    ResourceRestoreOrder:
      - backupName: config # API server required matching to backup struct
        includeClusterScopedResources: true
        includedResources: ["ConfigMap", "Secret"]
      - backupName: cpd
        includedResources: ["sample1.cpd.ibm.com", "sample2.cpd.ibm.com", "sample3.cpd.ibm.com"]
        # labelSelector: "" # intentionally omitted - don't require label match
        # includeClusterScopedResources: false # by default
      - backupName: deployments
        includedResources: ["Deployment"]
      - backupName: everything
        excludedResources: ["ConfigMap", "Secret", "Deployment", "sample1.cpd.ibm.com", "sample2.cpd.ibm.com", "sample3.cpd.ibm.com"]  # don't restore again
```

## Technical info

### Capture/Backup locations

This will take several Velero backups in a sequence. The S3 contents are
organized as follows for the example above:

```bash
/s3bucket
    /bucketPrefix
        /backups
            /namespaceName-vrgName-config
              /v1.ConfigMap
              /v1.Secret
            /namespaceName-vrgName-cpd
              /v1alpha1.custom1.cpd.ibm.com
              /v1alpha1.custom2.cpd.ibm.com
              /v1alpha1.custom3.cpd.ibm.com
            /namespaceName-vrgName-deployments
              /v1.Deployments
            /namespaceName-vrgName-everything
              / # everything else here
```

As an example, given the following parameters:

```
s3bucket = minio
bucketPrefix = velero
namespaceName = myApp
vrgName = vrg1
```

The first backup would have path `minio/velero/backups/myApp-vrg1-0`, which
contains Deployment backups.

### Recovery/Restore locations

Users are not restricted to maintaining a consistent Backup/Capture and
Recovery/Restore order. Additionally, Velero requires specifying a backup name
from which Kube objects can be recovered. As a result, Ramen needs to match
an existing sub-backup to a sub-restore.

In the example above, the restore objects will match the objects as follows:

```bash
- ["Secret", "ConfigMap"]  -> backup "config"
- ["custom1.cpd.ibm.com", "custom2.cpd.ibm.com", "custom3.cpd.ibm.com"]  -> backup "cpd"
- ["Deployments", "ReplicaSet", "StatefulSet", "CronJob", "Pod"] -> backups "deployments" and "everything"
```

## Design points

1. Apps that span multiple namespaces: VRG backup type sequencing is limited
   to the same namespace as the VRG itself. If an application spans multiple
   namespaces, then a type sequence should be specified on each VRG in each
   namespace.
