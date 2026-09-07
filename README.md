# bd

bd (bhyve daemon) is a bhyve virtual machine manager for FreeBSD. It is
similar to the vm-bhyve manager, but purposely seeks to implement less
functionality.

The main goal of bd is to transparently expose bhyve's CLI configuration
levers while adding the minimum amount of functionality required to operate
bhyve processes.

## Features

- Manages the full life cycle of a virtual machine - including creating,
  starting, stopping, restarting, and auto-starting virtual machines
- Automates the creation of virtual machine dependencies like tap devices
  and network configuration
- Configures virtual machines through a simple INI file which supports
  templating and directly exposes bhyve process arguments
- Multi-viewer serial console support via an unprivileged background process.
  Also supports logging serial console output to help debug VM kernel panics

## Requirements

- FreeBSD
- zfs

## Installation

```sh
# Note: All commands below should be run as the root user.

# Enable the vmm kernel module and load it:
sysrc kld_list+="vmm"
kldload vmm

# Install required dependencies:
pkg install bhyve-firmware go

# Compile and install bd:
su -m nobody -c 'cd $(mktemp -d) && export HOME="$(realpath .)" && go install codeberg.org/stephen-fox/bd@latest && realpath' | read tmp && cp -v "${tmp}/go/bin/bd" /usr/local/bin/bd

# Create the required directories and bd configuration file:
bd install

# Optional: Edit the bd configuration file:
# vim /usr/local/etc/bd/bd.conf

# Create VM storage and boot media zfs datasets (these can be customized):
zfs create zroot/vms
zfs create zroot/os-images
```

## Quick start

After installing bd, we can create a virtual machine like this:

```sh
# Note: All commands below should be run as the root user.

su -m nobody -c 'fetch -o - https://download.freebsd.org/releases/amd64/amd64/ISO-IMAGES/15.1/FreeBSD-15.1-RELEASE-amd64-disc1.iso' | cat > /zroot/os-images/FreeBSD-15.1-RELEASE-amd64-disc1.iso

bd new fbsd-example

# Edit the configuration file and change the "InsertDisc" parameter's
# value to be: FreeBSD-15.1-RELEASE-amd64-disc1.iso
vim /usr/local/etc/bd/vm-configs/fbsd-example/vm.conf

bd start fbsd-example

bd console fbsd-example
```

## Documentation

- Installation (see above)
- [Commands](./docs/commands)
- [Configuration](./docs/configuration)

## Project status

I have been using bd for nearly four years to manage virtual machines
in jails. However, FreeBSD 15 seemingly broke support for running bhyve
in a jail. I was already reconsidering my approach to using jails and,
with the most recent breakage, decided to redesign bd to be more like
vm-bhyve in September 2026. A lot changed in that code base, so I would
say we are firmly in the "experimental" phase again.

For more details on why I made this change, refer to the commit message
of [e12c400][redesign-commit].

[redesign-commit]: https://codeberg.org/stephen-fox/bd/commit/e12c40044072cd96f7be42b62404e30ccf8e2a73
