# Matchlock Linux package notes

This package installs:

- `matchlock`
- `guest-init`
- bundled `firecracker` and `jailer` under `/usr/libexec/matchlock/`
- a sysctl drop-in for `net.ipv4.ip_forward=1`

The package post-install script also attempts to:

- apply `cap_net_admin,cap_net_raw` to `/usr/bin/matchlock`
- enable IP forwarding immediately
- load the `nf_tables` kernel module

The package intentionally does **not**:

- add arbitrary users to `kvm` or `netdev`
- modify home-directory state

After installation, verify the host with:

```bash
matchlock diagnose
```

If the host still needs administrative setup, run:

```bash
sudo matchlock setup linux
```

To enroll a specific local user, run:

```bash
sudo matchlock setup user <name>
```
