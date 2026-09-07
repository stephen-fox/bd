# TODO

## Cleanup / sweeping

- Need to cleanup `main.go`, it's too long

## Missing features

- Allow serial console logging to be disabled while having serial console enabled
- Implement `delete` command for deleting VMs, require user to type the name
  of the VM in
- Require VM name to be typed when unplugging power

## Documentation

- Add in-app documentation for various commands
- Create a user guide

## VM management by non-root users

- Allow members of wheel group to create and manage VMs
- Allow regular users to admin existing VMs
  - May need to restrict the functionality of the "insert disc"
    and various paths that can be passed to bhyve
