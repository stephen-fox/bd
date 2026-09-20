# TODO

## Cleanup / sweeping

- Need to cleanup `main.go`, it's too long
- Do not allow slashes in VM names
- Fix formatting / spacing of "bd ls"
- Require VM name to be typed when unplugging power
- Use mkgd library for daemonization

## Missing features

- Allow serial console logging to be disabled while having serial console
  enabled
- Implement `delete` command for deleting VMs, require user to type the name
  of the VM in
- Implment `stop-all` command, require user to acknowledge via prompt

## Documentation

- Add in-app documentation for various commands
- Create a user guide

## VM management by non-root users

- Allow members of wheel group to create and manage VMs
- Allow regular users to admin existing VMs
  - May need to restrict the functionality of the "insert disc"
    and various paths that can be passed to bhyve
