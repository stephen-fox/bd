# Configuration

bd is configured using files in an INI syntax. A line starting with
`[` indicates a configuration section. Parameters can be specified
in the form of `Parameter = Value`, or as a list of parameters depending
on the configuration section. Lines starting with `#` are ignored
and can be used to store comments.

Please find the configuration formats documented below.

## Application configuration

Location: `/usr/local/etc/bd/bd.conf`

General application functionality is configured in the `bd.conf` file.
This file does not support variables or templating.

### `General` section

Parameters:

- `BootMediaDir` (string, required) - The path to a directory containing
  boot media for use with the `InsertDisc` parameter in the virtual
  machine's configuration file
- `VmsStorageDir` (string, required) - The path to a parent zfs dataset
  where new virtual machines' disks and UEFI variable files will be stored
  using a child zfs dataset named after the virtual machine

### Example

```ini
[General]
BootMediaDir = /zroot/os-images
VmsStorageDir = /zroot/vms
```

## Virtual machine configuration

Location: `/usr/local/etc/bd/vm-configs/VM_NAME/vm.conf`

Each virtual machine is configured using its own `vm.conf` file. This file
supports referencing variables defined by both bd and the user. Variables
can be referenced using shell syntax with `$`, including: `$VARIABLE_NAME`
and `${VARIABLE_NAME}`.

The following bd-managed variables are automatically defined by and can
be referenced in any of the `vm.conf` sections:

- `BD_CONFIG_BOOT_MEDIA_DIR` - The value of the `BootMediaDir` parameter
  from `bd.conf`
- `BD_CONFIG_STORAGE_DIR` - The value of the `VmsStorageDir` parameter 
  from `bd.conf`
- `BD_CONFIG_VM_STORAGE_DIR` - The path to the virtual machine's storage
  directory
- `BD_CONFIG_VM_NAME` - The name of the virtual machine

Additional variables can be defined by the user in certain configuration
sections, which is documented in each respective configuration section
below.

### `General` section

Specifies general settings for the virtual machine.

Parameters:

- `Autostart` (boolean, default: false) - Whether the virtual machine should
  be started when running `bd autostart`
- `SerialConsole` (boolean, default: false) - Whether to enable bd's
  custom serial console. Requires that `-l com1,stdio` be specified
  in the bhyve arguments
- `InsertDisc` (string) - If defined, automatically attaches an emulated
  CD drive PCI device and inserts the file pointed to by this parameter
  into the drive. If the parameter is a file name or relative path, it
  is treated as being relative to the path specified in the `BootMediaDir`
  parameter. If the string is an absolute path, then that file is inserted
  into the drive

Example:

```ini
[General]
Autostart = true
SerialConsole = true
InsertDisc = FreeBSD-15.1-RELEASE-amd64-disc1.iso
```

### `Prestart` section

Defines one or more programs to execute prior to starting the virtual
machine. The standard output of the last-executed program can be stored
in a variable which can be referenced later. That behavior occurs when
the `Name` parameter is specified, in which case the standard output
will be stored in a variable named `BD_CONFIG_PRESTART_EXAMPLE`, where
`EXAMPLE` is the exact string that appears in the `Name` value. That
variable can be referenced in subsequent `Prestart` sections and in the
current section's `CleanupExec` parameter.

More than one `Prestart` section can be defined.

- `Name` (string) - An optional name to assign to this section. When
  specified, bd will store the standard output of the last `CreateExec`
  in a variable named `BD_CONFIG_PRESTART_EXAMPLE`, where `EXAMPLE` is
  this parameter's value
- `CreateExec` (string, required) - The program to execute using the
  exec family of functions. More than one instance of this parameter
  can be defined. Supports the same "word" syntax as a shell
- `CleanupExec` (string) - An optional program to execute using the
  exec family of functions when the virtual machine is shutdown.
  Supports the same "word" syntax as a shell

Example:

```ini
[Prestart]
Name = tap
CreateExec = ifconfig tap create description bhyve/${BD_CONFIG_VM_NAME}
CleanupExec = ifconfig ${BD_CONFIG_PRESTART_tap} destroy
```

### `BhyveArgs` section (required)

A newline-delimited list of arguments to pass to the bhyve process,
excluding the virtual machine's name (which is automatically added by bd).
Each line supports shell "word" syntax, allowing more than one argument
to be specified per line. For example, a single line containing `-c 2`
would be split into two arguments because the space character.

Example:

```ini
[BhyveArgs]
-A
-D
-H
-P
-c 2
-m 4G
# Comments can be inserted between lines for documentation purposes
# or to comment out individual settings without impacting subsequent
# lines, making it easy to disable bhyve functionality without having
# to restructure the entire file:
# -w
-s 0,hostbridge
-s 3,virtio-blk,${BD_CONFIG_VM_STORAGE_DIR}/disk0.img
-s 10,virtio-net,${BD_CONFIG_PRESTART_tap},mac=52:e5:0c:dd:be:19
-s 20,virtio-rnd
-s 31,lpc
-l com1,stdio
-l bootrom,/usr/local/share/uefi-firmware/BHYVE_UEFI.fd,${BD_CONFIG_VM_STORAGE_DIR}/uefi-vars.fd
```
