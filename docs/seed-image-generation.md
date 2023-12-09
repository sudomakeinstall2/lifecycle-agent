# IBU Seed Image Generation

- [IBU Seed Image Generation](#ibu-seed-image-generation)
  - [Overview](#overview)
  - [Seed SNO Pre-Requisites](#seed-sno-pre-requisites)
    - [Shared Container Storage](#shared-container-storage)
    - [Required dnsmasq Configuration](#required-dnsmasq-configuration)
  - [SeedGenerator CR](#seedgenerator-cr)
    - [Creating the seedgen Secret CR](#creating-the-seedgen-secret-cr)
    - [Creating the seedimage SeedGenerator CR](#creating-the-seedimage-seedgenerator-cr)
  - [Generating the IBU Seed Image](#generating-the-ibu-seed-image)
    - [Monitoring Progress](#monitoring-progress)
  - [ACM and ZTP GitOps Considerations](#acm-and-ztp-gitops-considerations)

## Overview

The Lifecycle Agent provides orchestration of IBU Seed Image generation via the `SeedGenerator` CRD. As part of orchestration, the LCA will:

- Perform system configuration checks to ensure any required configuration is present.
- Perform any necessary system cleanup prior to generating the seed image.
- Launch the ibu-imager tool, which will:
  - Shutdown the cluster operators
  - Prepare seed image config
  - Generate and publish the seed image
  - Restore the cluster operators
- (not yet implemented) After LCA operator recovers, restore the seedgen CR and update its status to reflect success or failure of image generation, and restore the `ManagedCluster` CR on the hub (if applicable).

## Seed SNO Pre-Requisites

The seed SNO configuration has some pre-requisites:

- The CPU topology must align with the target SNO(s).
  - Same number of cores.
  - Same tuned performance configuration (ie. reserved CPUs).
  - FIPS enablement (? *TODO*: confirm)
- Deployed in the same manner as target SNO(s).
  - Same ACM/MCE version.
    - Exception: Hub for seed SNO must not have extra ACM addons enabled (ie. observability). The LCA orchestration confirms that no such addons are present on the seed SNO as part of its system config validation.
- OADP operator must be deployed.
- Container storage must be setup as shared between stateroots, such as with a separate partition.
- Required dnsmasq configuration to support updating cluster name, domain, and IP from the seed image as part of IBU.

### Shared Container Storage

Image Based Upgrade (IBU) requires that container storage (`/var/lib/containers`) is setup to be shared between stateroots. This can be done by creating a separate partition for container storage when the node is installed. Please see this [blog article](https://cloud.redhat.com/blog/a-guide-to-creating-a-separate-disk-partition-at-installation-time) for more information on how this is done using a `MachineConfig`.

With ZTP GitOps, the `MachineConfig` can be added as `extra-manifests` under the site-config. For example:

```yaml
apiVersion: machineconfiguration.openshift.io/v1
kind: MachineConfig
metadata:
  labels:
    machineconfiguration.openshift.io/role: master
  name: 98-var-lib-containers-partitioned
spec:
  config:
    ignition:
      version: 3.2.0
    storage:
      disks:
        - device: /dev/disk/by-id/wwn-0x62cea7f04d10350026c6f2ec315557a0
          partitions:
            - label: varlibcontainers
              startMiB: 250000 # Leave room for rootfs
              sizeMiB: 0 # Use available space
      filesystems:
        - device: /dev/disk/by-partlabel/varlibcontainers
          format: xfs
          mountOptions:
            - defaults
            - prjquota
          path: /var/lib/containers
          wipeFilesystem: true
    systemd:
      units:
        - contents: |-
            # Generated by Butane
            [Unit]
            Before=local-fs.target
            Requires=systemd-fsck@dev-disk-by\x2dpartlabel-varlibcontainers.service
            After=systemd-fsck@dev-disk-by\x2dpartlabel-varlibcontainers.service

            [Mount]
            Where=/var/lib/containers
            What=/dev/disk/by-partlabel/varlibcontainers
            Type=xfs
            Options=defaults,prjquota

            [Install]
            RequiredBy=local-fs.target
          enabled: true
          name: var-lib-containers.mount
```

### Required dnsmasq Configuration

Image Based Upgrade (IBU) requires that dnsmasq is configured such that it is able to use updated cluster name, domain, and IP address, as these will all be different for the target SNO compared to the seed SNO. The dnsmasq configuration is managed by a `MachineConfig` created during installation by the assisted-installer named `50-master-dnsmasq-configuration`. The changes required for IBU are not yet available in a GA ACM release, however, so a workaround is needed to override this `MachineConfig` until such time as we can define a minimum ACM version requirement.

A helper script, [generate-dnsmasq-site-policy-section.sh](../hack/generate-dnsmasq-site-policy-section.sh), is provided to aid in creating a cluster-specific site-policy subsection or `MachineConfig`.

```console
$ hack/generate-dnsmasq-site-policy-section.sh --help
Usage: generate-dnsmasq-site-policy-section.sh --name <cluster> --domain <domain> --ip <addr> [ --mc ] [ --wave <1-100> ]
Options:
    --name       - Cluster name
    --domain     - Cluster baseDomain
    --ip         - Node IP
    --mc         - Generate machine-config (default is site-policy)
    --wave       - Add ztp-deploy-wave annotation with specified value to site-policy

Summary:
    Generates a subsection of site-policy to include dnsmasq config for an SNO.

Example:
    generate-dnsmasq-site-policy-section.sh --name mysno --domain sno.cluster-domain.com --ip 10.20.30.5
```

Generating site-policy subsection for ZTP GitOps:

```console
# NOTE: The --wave option is needed if this subsection is in a standalone site-polocy PGT with no other source CRs,
# in order to add the necessary ztp-deploy-wave annotation.

$ hack/generate-dnsmasq-site-policy-section.sh --name mysno --domain sno.cluster-domain.com --ip 10.20.30.5 --wave 100
    # Override 50-master-dnsmasq-configuration
    - fileName: MachineConfigGeneric.yaml
      policyName: "config-policy"
      complianceType: mustonlyhave # This is to update array entry as opposed to appending a new entry.
      metadata:
        labels:
          machineconfiguration.openshift.io/role: master
        name: 50-master-dnsmasq-configuration
        annotations:
          ran.openshift.io/ztp-deploy-wave: "100"
      spec:
        config:
          ignition:
            version: 3.1.0
          storage:
            files:
              - contents:
                  source: data:text/plain;charset=utf-8;base64,IyEvdXNyL2Jpbi9lbnYgYmFzaAoKIyBJbiBvcmRlciB0byBvdmVycmlkZSBjbHVzdGVyIGRvbWFpbiBwbGVhc2UgcHJvdmlkZSB0aGlzIGZpbGUgd2l0aCB0aGUgZm9sbG93aW5nIHBhcmFtczoKIyBTTk9fQ0xVU1RFUl9OQU1FX09WRVJSSURFPTxuZXcgY2x1c3RlciBuYW1lPgojIFNOT19CQVNFX0RPTUFJTl9PVkVSUklERT08eW91ciBuZXcgYmFzZSBkb21haW4+CiMgU05PX0ROU01BU1FfSVBfT1ZFUlJJREU9PG5ldyBpcD4Kc291cmNlIC9ldGMvZGVmYXVsdC9zbm9fZG5zbWFzcV9jb25maWd1cmF0aW9uX292ZXJyaWRlcwoKSE9TVF9JUD0ke1NOT19ETlNNQVNRX0lQX09WRVJSSURFOi0iMTAuMjAuMzAuNSJ9CkNMVVNURVJfTkFNRT0ke1NOT19DTFVTVEVSX05BTUVfT1ZFUlJJREU6LSJteXNubyJ9CkJBU0VfRE9NQUlOPSR7U05PX0JBU0VfRE9NQUlOX09WRVJSSURFOi0ic25vLmNsdXN0ZXItZG9tYWluLmNvbSJ9CkNMVVNURVJfRlVMTF9ET01BSU49IiR7Q0xVU1RFUl9OQU1FfS4ke0JBU0VfRE9NQUlOfSIKCmNhdCA8PCBFT0YgPiAvZXRjL2Ruc21hc3EuZC9zaW5nbGUtbm9kZS5jb25mCmFkZHJlc3M9L2FwcHMuJHtDTFVTVEVSX0ZVTExfRE9NQUlOfS8ke0hPU1RfSVB9CmFkZHJlc3M9L2FwaS1pbnQuJHtDTFVTVEVSX0ZVTExfRE9NQUlOfS8ke0hPU1RfSVB9CmFkZHJlc3M9L2FwaS4ke0NMVVNURVJfRlVMTF9ET01BSU59LyR7SE9TVF9JUH0KRU9GCg==
                mode: 365
                path: /usr/local/bin/dnsmasq_config.sh
                overwrite: true
              - contents:
                  source: data:text/plain;charset=utf-8;base64,IyEvYmluL2Jhc2gKCiMgSW4gb3JkZXIgdG8gb3ZlcnJpZGUgY2x1c3RlciBkb21haW4gcGxlYXNlIHByb3ZpZGUgdGhpcyBmaWxlIHdpdGggdGhlIGZvbGxvd2luZyBwYXJhbXM6CiMgU05PX0NMVVNURVJfTkFNRV9PVkVSUklERT08bmV3IGNsdXN0ZXIgbmFtZT4KIyBTTk9fQkFTRV9ET01BSU5fT1ZFUlJJREU9PHlvdXIgbmV3IGJhc2UgZG9tYWluPgojIFNOT19ETlNNQVNRX0lQX09WRVJSSURFPTxuZXcgaXA+CnNvdXJjZSAvZXRjL2RlZmF1bHQvc25vX2Ruc21hc3FfY29uZmlndXJhdGlvbl9vdmVycmlkZXMKCkhPU1RfSVA9JHtTTk9fRE5TTUFTUV9JUF9PVkVSUklERTotIjEwLjIwLjMwLjUifQpDTFVTVEVSX05BTUU9JHtTTk9fQ0xVU1RFUl9OQU1FX09WRVJSSURFOi0ibXlzbm8ifQpCQVNFX0RPTUFJTj0ke1NOT19CQVNFX0RPTUFJTl9PVkVSUklERTotInNuby5jbHVzdGVyLWRvbWFpbi5jb20ifQpDTFVTVEVSX0ZVTExfRE9NQUlOPSIke0NMVVNURVJfTkFNRX0uJHtCQVNFX0RPTUFJTn0iCgpleHBvcnQgQkFTRV9SRVNPTFZfQ09ORj0vcnVuL05ldHdvcmtNYW5hZ2VyL3Jlc29sdi5jb25mCmlmIFsgIiQyIiA9ICJkaGNwNC1jaGFuZ2UiIF0gfHwgWyAiJDIiID0gImRoY3A2LWNoYW5nZSIgXSB8fCBbICIkMiIgPSAidXAiIF0gfHwgWyAiJDIiID0gImNvbm5lY3Rpdml0eS1jaGFuZ2UiIF07IHRoZW4KICAgIGV4cG9ydCBUTVBfRklMRT0kKG1rdGVtcCAvZXRjL2ZvcmNlZG5zX3Jlc29sdi5jb25mLlhYWFhYWCkKICAgIGNwICAkQkFTRV9SRVNPTFZfQ09ORiAkVE1QX0ZJTEUKICAgIGNobW9kIC0tcmVmZXJlbmNlPSRCQVNFX1JFU09MVl9DT05GICRUTVBfRklMRQogICAgc2VkIC1pIC1lICJzLyR7Q0xVU1RFUl9GVUxMX0RPTUFJTn0vLyIgICAgICAgICAtZSAicy9zZWFyY2ggLyYgJHtDTFVTVEVSX0ZVTExfRE9NQUlOfSAvIiAgICAgICAgIC1lICIwLC9uYW1lc2VydmVyL3MvbmFtZXNlcnZlci8mICRIT1NUX0lQXG4mLyIgJFRNUF9GSUxFCiAgICBtdiAkVE1QX0ZJTEUgL2V0Yy9yZXNvbHYuY29uZgpmaQo=
                mode: 365
                path: /etc/NetworkManager/dispatcher.d/forcedns
                overwrite: true
              - contents:
                  source: data:text/plain;charset=utf-8;base64,ClttYWluXQpyYy1tYW5hZ2VyPXVubWFuYWdlZAo=
                mode: 420
                path: /etc/NetworkManager/conf.d/single-node.conf
                overwrite: true
          systemd:
            units:
              - name: dnsmasq.service
                enabled: true
                contents: |
                  [Unit]
                  Description=Run dnsmasq to provide local dns for Single Node OpenShift
                  Before=kubelet.service crio.service
                  After=network.target

                  [Service]
                  TimeoutStartSec=30
                  ExecStartPre=/usr/local/bin/dnsmasq_config.sh
                  ExecStart=/usr/sbin/dnsmasq -k
                  Restart=always

                  [Install]
                  WantedBy=multi-user.target
```

Generating `MachineConfig` for manual override:

```console
$ hack/generate-dnsmasq-site-policy-section.sh --name mysno --domain sno.cluster-domain.com --ip 10.20.30.5 --mc
apiVersion: machineconfiguration.openshift.io/v1
kind: MachineConfig
metadata:
  labels:
    machineconfiguration.openshift.io/role: master
  name: 50-master-dnsmasq-configuration
spec:
  config:
    ignition:
      version: 3.1.0
    storage:
      files:
        - contents:
            source: data:text/plain;charset=utf-8;base64,IyEvdXNyL2Jpbi9lbnYgYmFzaAoKIyBJbiBvcmRlciB0byBvdmVycmlkZSBjbHVzdGVyIGRvbWFpbiBwbGVhc2UgcHJvdmlkZSB0aGlzIGZpbGUgd2l0aCB0aGUgZm9sbG93aW5nIHBhcmFtczoKIyBTTk9fQ0xVU1RFUl9OQU1FX09WRVJSSURFPTxuZXcgY2x1c3RlciBuYW1lPgojIFNOT19CQVNFX0RPTUFJTl9PVkVSUklERT08eW91ciBuZXcgYmFzZSBkb21haW4+CiMgU05PX0ROU01BU1FfSVBfT1ZFUlJJREU9PG5ldyBpcD4Kc291cmNlIC9ldGMvZGVmYXVsdC9zbm9fZG5zbWFzcV9jb25maWd1cmF0aW9uX292ZXJyaWRlcwoKSE9TVF9JUD0ke1NOT19ETlNNQVNRX0lQX09WRVJSSURFOi0iMTAuMjAuMzAuNSJ9CkNMVVNURVJfTkFNRT0ke1NOT19DTFVTVEVSX05BTUVfT1ZFUlJJREU6LSJteXNubyJ9CkJBU0VfRE9NQUlOPSR7U05PX0JBU0VfRE9NQUlOX09WRVJSSURFOi0ic25vLmNsdXN0ZXItZG9tYWluLmNvbSJ9CkNMVVNURVJfRlVMTF9ET01BSU49IiR7Q0xVU1RFUl9OQU1FfS4ke0JBU0VfRE9NQUlOfSIKCmNhdCA8PCBFT0YgPiAvZXRjL2Ruc21hc3EuZC9zaW5nbGUtbm9kZS5jb25mCmFkZHJlc3M9L2FwcHMuJHtDTFVTVEVSX0ZVTExfRE9NQUlOfS8ke0hPU1RfSVB9CmFkZHJlc3M9L2FwaS1pbnQuJHtDTFVTVEVSX0ZVTExfRE9NQUlOfS8ke0hPU1RfSVB9CmFkZHJlc3M9L2FwaS4ke0NMVVNURVJfRlVMTF9ET01BSU59LyR7SE9TVF9JUH0KRU9GCg==
          mode: 365
          path: /usr/local/bin/dnsmasq_config.sh
          overwrite: true
        - contents:
            source: data:text/plain;charset=utf-8;base64,IyEvYmluL2Jhc2gKCiMgSW4gb3JkZXIgdG8gb3ZlcnJpZGUgY2x1c3RlciBkb21haW4gcGxlYXNlIHByb3ZpZGUgdGhpcyBmaWxlIHdpdGggdGhlIGZvbGxvd2luZyBwYXJhbXM6CiMgU05PX0NMVVNURVJfTkFNRV9PVkVSUklERT08bmV3IGNsdXN0ZXIgbmFtZT4KIyBTTk9fQkFTRV9ET01BSU5fT1ZFUlJJREU9PHlvdXIgbmV3IGJhc2UgZG9tYWluPgojIFNOT19ETlNNQVNRX0lQX09WRVJSSURFPTxuZXcgaXA+CnNvdXJjZSAvZXRjL2RlZmF1bHQvc25vX2Ruc21hc3FfY29uZmlndXJhdGlvbl9vdmVycmlkZXMKCkhPU1RfSVA9JHtTTk9fRE5TTUFTUV9JUF9PVkVSUklERTotIjEwLjIwLjMwLjUifQpDTFVTVEVSX05BTUU9JHtTTk9fQ0xVU1RFUl9OQU1FX09WRVJSSURFOi0ibXlzbm8ifQpCQVNFX0RPTUFJTj0ke1NOT19CQVNFX0RPTUFJTl9PVkVSUklERTotInNuby5jbHVzdGVyLWRvbWFpbi5jb20ifQpDTFVTVEVSX0ZVTExfRE9NQUlOPSIke0NMVVNURVJfTkFNRX0uJHtCQVNFX0RPTUFJTn0iCgpleHBvcnQgQkFTRV9SRVNPTFZfQ09ORj0vcnVuL05ldHdvcmtNYW5hZ2VyL3Jlc29sdi5jb25mCmlmIFsgIiQyIiA9ICJkaGNwNC1jaGFuZ2UiIF0gfHwgWyAiJDIiID0gImRoY3A2LWNoYW5nZSIgXSB8fCBbICIkMiIgPSAidXAiIF0gfHwgWyAiJDIiID0gImNvbm5lY3Rpdml0eS1jaGFuZ2UiIF07IHRoZW4KICAgIGV4cG9ydCBUTVBfRklMRT0kKG1rdGVtcCAvZXRjL2ZvcmNlZG5zX3Jlc29sdi5jb25mLlhYWFhYWCkKICAgIGNwICAkQkFTRV9SRVNPTFZfQ09ORiAkVE1QX0ZJTEUKICAgIGNobW9kIC0tcmVmZXJlbmNlPSRCQVNFX1JFU09MVl9DT05GICRUTVBfRklMRQogICAgc2VkIC1pIC1lICJzLyR7Q0xVU1RFUl9GVUxMX0RPTUFJTn0vLyIgICAgICAgICAtZSAicy9zZWFyY2ggLyYgJHtDTFVTVEVSX0ZVTExfRE9NQUlOfSAvIiAgICAgICAgIC1lICIwLC9uYW1lc2VydmVyL3MvbmFtZXNlcnZlci8mICRIT1NUX0lQXG4mLyIgJFRNUF9GSUxFCiAgICBtdiAkVE1QX0ZJTEUgL2V0Yy9yZXNvbHYuY29uZgpmaQo=
          mode: 365
          path: /etc/NetworkManager/dispatcher.d/forcedns
          overwrite: true
        - contents:
            source: data:text/plain;charset=utf-8;base64,ClttYWluXQpyYy1tYW5hZ2VyPXVubWFuYWdlZAo=
          mode: 420
          path: /etc/NetworkManager/conf.d/single-node.conf
          overwrite: true
    systemd:
      units:
        - name: dnsmasq.service
          enabled: true
          contents: |
            [Unit]
            Description=Run dnsmasq to provide local dns for Single Node OpenShift
            Before=kubelet.service crio.service
            After=network.target

            [Service]
            TimeoutStartSec=30
            ExecStartPre=/usr/local/bin/dnsmasq_config.sh
            ExecStart=/usr/sbin/dnsmasq -k
            Restart=always

            [Install]
            WantedBy=multi-user.target
```

## SeedGenerator CR

The Lifecycle Agent provides orchestration of the IBU Seed Image generation, triggered by creating a `SeedGenerator` CR. Additionally, a `seedgen` `Secret` is required to provide the auth necessary for publishing the seed image, as well as the optional `hubKubeconfig` if the LCA is to handle the `ManagedCluster`.

*TODO*: Provide helper script for generating the `seedgen` `Secret` and `seedimage` `SeedGenerator` CRs

### Creating the seedgen Secret CR

The `seedgen` `Secret`, created in the `openshift-lifecycle-agent` namespace, allows the user to provide the following information:

- `seedAuth`: base64-encoded auth file for write-access to the registry for pushing the generated seed image
- `hubKubeconfig`: (Optional) base64-encoded kubeconfig for admin access to the hub, in order to deregister the seed
  cluster from ACM. If this is not present in the secret, the ACM cleanup will be skipped.

> [!IMPORTANT]  
> This `Secret` must be named `seedgen` and must be created in the `openshift-lifecycle-agent` namespace.

The `seedAuth` is the base64-encoded auth file containing credentials with write-access to the registry to which the seed image is to be published. The auth file itself can be created with limited credentials by running a `podman login` command, such as:

```console
# MY_USER=myuserid
# AUTHFILE=/tmp/my-auth.json
# podman login --authfile ${AUTHFILE} -u ${MY_USER} quay.io/${MY_USER}
Password:
Login Succeeded!
# base64 -w 0 ${AUTHFILE} ; echo
ewoJImF1dGhzIjogewoJCSJxdWF5LmlvL215dXNlcmlkIjogewoJCQkiYXV0aCI6ICJub3R0aGVyZWFsYXV0aHN0cmluZyIKCQl9Cgl9Cn0K
#
```

Example:

```yaml
---
apiVersion: v1
kind: Secret
metadata:
  name: seedgen
  namespace: openshift-lifecycle-agent
type: Opaque
data:
  seedAuth: <encoded authfile>
  hubKubeconfig: <encoded kubeconfig>
```

### Creating the seedimage SeedGenerator CR

The `seedimage` `SeedGenerator` CR allows the user to provide the following information:

- `seedImage`: The pullspec (ie. registry/repo:tag) for the generated image

> [!IMPORTANT]  
> This `SeedGenerator` CR must be named `seedimage`.

Example:

```yaml
---
apiVersion: lca.openshift.io/v1alpha1
kind: SeedGenerator
metadata:
  name: seedimage
spec:
  seedImage: quay.io/dpenney/upgbackup:orchestrated-seed-image
```

## Generating the IBU Seed Image

Creating the `seedimage` `SeedGenerator` will trigger the LCA operator to launch the seed image generation.

First, the orchestrator will run its system config validation checks to ensure the required seed SNO configuration is present. If the validation fails, the CR will be updated with conditions and status message noting the image could not be generated and provide a rejection message. The LCA operator manager logs will also provide the rejection message, along with related log messages.

*TODO*: Provide example of CR with rejection message.

After the system config has been validated successfully, the orchestor will perform any necessary cleanup and launch the ibu-imager tool to generate and publish the image.

> [!WARNING]  
> As part of preparing the generate the seed image, the ibu-imager will shut down all running operators and pods. Once the ibu-imager is complete, it will restart kubelet to trigger recovery of the operators.

### Monitoring Progress

LCA Operator logs:

```console
oc logs -n openshift-lifecycle-agent --selector app.kubernetes.io/component=lifecycle-agent --container manager --follow
```

Once the ibu-imager is launched, you can SSH to the seed SNO and monitor the container logs by running the following:

```console
podman logs -f ibu_imager
```

## ACM and ZTP GitOps Considerations

If you provide a `hubKubeconfig` in your `seedgen` `Secret`, the orchestrator will interact with the hub to verify whether the `ManagedCluster` exists for the seed SNO. If it exists, the orchestrator will detach the cluster from ACM by deleting the `ManagedCluster`, saving the CR to be restored as part of post-imager recovery.

> [!WARNING]  
> When the orchestrator deletes the `ManagedCluster`, ArgoCD will mark the site-config "out of sync". If you have the `selfHeal` option enabled in ArgoCD, it will automatically sync and recreate the CR, triggering ACM to reimport the seed SNO while it is preparing to generate the image. This means you must drop the site-config in gitops prior to triggering the seed image generation, rather than providing the `hubKubeconfig`. Similarly, if you are using a shared hub, a sync could be triggered by someone else.

> [!IMPORTANT]  
> If you are using gitops to deploy your seed SNO, it is highly recommended that you do not provide a `hubKubeconfig`, due to the potential race condition of resyncing, regardless of whether `selfHeal` is enabled in ArgoCD. Rather, you should drop the site-config from gitops and sync/prune the cluster first. Once the cluster has been removed from the hub, you can safely trigger the seed image generation.