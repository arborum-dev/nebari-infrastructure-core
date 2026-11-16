# RKE2 Provider Configuration

Configuration options for a single-node RKE2 cluster installed on an existing host over SSH.

> This documentation is auto-generated from source code using `go generate`.

## Table of Contents

- [Config](#config)
- [SSHConfig](#sshconfig)
- [Settings](#settings)

---

## Config

Config is the cluster.rke2 block: one existing host, reached over SSH, on
which NIC installs and manages RKE2.

| Field | YAML Key | Type | Required | Description |
|-------|----------|------|----------|-------------|
| Host | `host` | string | Yes | Host is the address NIC opens the SSH connection to, such as an IP address or a hostname. Required. |
| SSH | `ssh` | SSHConfig | Yes | SSH describes how to log in to Host. Required. |
| StorageClass | `storage_class` | string | No | StorageClass is the default Kubernetes StorageClass for persistent volumes. Required when Longhorn is disabled, and names a StorageClass you provide some other way. Ignored while Longhorn is enable... |
| Longhorn | `longhorn` | `*longhorn.Config` | No | Longhorn configures NIC's shared Longhorn storage block. Longhorn is enabled when this block is omitted and is disabled only by "longhorn: {enabled: false}". When replica_count is unset the provide... |

---

## SSHConfig

SSHConfig describes the SSH login to the host. NIC always verifies the host
key against known_hosts; there is no way to skip that check.

| Field | YAML Key | Type | Required | Description |
|-------|----------|------|----------|-------------|
| User | `user` | string | No | User is the login user: root, or a user with passwordless sudo. Password logins and sudo password prompts are not supported. Defaults to "root". |
| Port | `port` | int | No | Port is the SSH port. Defaults to 22. |
| PrivateKeyPath | `private_key_path` | string | Yes | PrivateKeyPath is the path to the private key used to log in. The key must not need a passphrase. The path is used as written; "~" is not expanded. Required. |
| KnownHostsPath | `known_hosts_path` | string | No | KnownHostsPath is the known_hosts file that must already hold the host's key. The path is used as written; "~" is not expanded. Defaults to $HOME/.ssh/known_hosts. |
| HostKeyAlias | `host_key_alias` | string | No | HostKeyAlias is a name to look up in known_hosts instead of the host and port. Use it when the host is reached through a forwarded port and known_hosts lists it under another name. Optional. |

---

## Settings

Settings holds the RKE2 options that a provider creating its own host can
reuse. In cluster.rke2 these keys sit directly under the block, not under a
settings: key; a provider that creates hosts can expose them as a named
sub-block.

| Field | YAML Key | Type | Required | Description |
|-------|----------|------|----------|-------------|
| Version | `version` | string | Yes | Version is the exact RKE2 release tag to run, such as v1.37.2+rke2r1. There is no "latest". Required. |
| APIServerAddress | `api_server_address` | string | No | APIServerAddress is the address the returned kubeconfig points at. It is also added to RKE2's tls-san. Set it when it differs from the SSH host, for example when SSH uses a forwarded port. Defaults... |
| StartOnBoot | `start_on_boot` | `*bool` | No | StartOnBoot enables the RKE2 service at boot when true and disables it when false. Defaults to true. |
| Config | `config` | `map[string]any` | No | Config holds extra keys written into RKE2's own config file (/etc/rancher/rke2/config.yaml), such as a CIS profile, the CNI or kubelet arguments. A key here replaces the provider's default of the s... |

