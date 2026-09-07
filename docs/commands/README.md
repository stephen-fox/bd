# Commands

bd exposes git-like commands for managing virtual machines. Please find
a full list of commands and their usage below.

## `install`

Creates the various directories bd needs to operate and generates
an application-level configuration file. This only needs to be run
once per hypervisor.

## `new VM_NAME`

Creates a new virtual machine with the name `VM_NAME` and a basic
virtual machine configuration file.

## `ls`

Lists existing virtual machines and their status.

## `status VM_NAME`

Retrieves the status of a particular virtual machine with the name `VM_NAME`.

## `start VM_NAME`

Starts a virtual machine with the name `VM_NAME`.

## `restart VM_NAME`

Restarts a virtual machine with the name `VM_NAME` by essentially
executing `stop` followed by `start`.

## `stop VM_NAME`

Stops a virtual machine with the name `VM_NAME` by simulating a ACPI
power button press (i.e., a standard, graceful shutdown).

## `unplug VM_NAME`

Stops a virtual machine with the name `VM_NAME` by simulating the power
cable being unplugged (i.e., a *non-graceful* shutdown). Only use this
if a VM is unresponsive.

## `autostart`

Starts all virtual machines that are configured with the `Autostart`
configuration parameter set to `true`.

## `console VM_NAME`

Connects to the serial console of a virtual machine named `VM_NAME` which
has the `SerialConsole` configuration parameter set to `true`. This command
uses a bd-specific serial console implementation and is not compatible
with an emulated serial console device attached using the `-l` bhyve
CLI argument.

If you are connected to the hypervisor using an application like OpenSSH,
you can exit the serial console by typing:

```
Enter
~
~
.
```

If you are sitting in front of the hypervisor with a keyboard, you can
exit the serial console by typing:

```
Enter
~
.
```

##  `genmac`

Generates a random MAC address and writes it to standard output.
