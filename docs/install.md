# Installation

Matchlock supports:

- macOS on Apple Silicon via Homebrew
- Linux via upstream `.deb` and `.rpm` packages published on GitHub Releases

## Quick install script

Use the installer script from this repository:

```bash
curl -fsSL https://raw.githubusercontent.com/jingkaihe/matchlock/main/scripts/install.sh | bash
```

To install a specific release:

```bash
curl -fsSL https://raw.githubusercontent.com/jingkaihe/matchlock/main/scripts/install.sh | bash -s -- --version 0.2.4
```

On macOS, pinned installs work once the Homebrew tap has been updated to that same release.

If you already have the repository checked out locally, you can also run:

```bash
./scripts/install.sh
```

The script detects the current OS and architecture:

- on macOS, it installs via Homebrew and upgrades existing installs
- on Linux, it downloads and installs the latest matching `.deb` or `.rpm` package from GitHub Releases

On Linux, the script currently supports:

- Debian / Ubuntu and derivatives (`.deb` via `dpkg` and `apt-get`)
- Fedora / RHEL / CentOS Stream and derivatives (`.rpm` via `dnf` or `yum`)

The package format is chosen from `ID` and `ID_LIKE` in `/etc/os-release`, so having `dpkg` installed on Fedora does not switch it to the `.deb` flow. If the distribution is not recognized, the script falls back to whichever package tools are available.

After installation, verify the host with:

```bash
matchlock diagnose
```

If the host still needs administrative setup, run:

```bash
sudo matchlock setup linux
```

To enroll a specific local user explicitly, run:

```bash
sudo matchlock setup user <name>
```

## macOS

```bash
brew tap jingkaihe/essentials
brew install matchlock
```

## Debian / Ubuntu

Download the matching release asset from GitHub Releases, then install it:

```bash
sudo dpkg -i ./matchlock_<version>_linux_amd64.deb
sudo apt-get install -f
```

For arm64 systems, use the `linux_arm64.deb` release asset instead.

## Fedora / RHEL / CentOS Stream

Download the matching release asset from GitHub Releases, then install it:

```bash
sudo dnf install ./matchlock_<version>_linux_amd64.rpm
```

For arm64 systems, use the `linux_arm64.rpm` release asset instead.
