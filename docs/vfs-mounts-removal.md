# VFS Mounts Removal and Migration Guide

Matchlock 0.3.0 removes the FUSE-based virtual filesystem (VFS). This includes host-directory mounts, in-memory and snapshot mounts, the guest workspace mount, and VFS interception hooks. Guests now only use their own filesystems. Files are copied between the host and the guest explicitly, and data that needs to persist is stored on ext4 volumes attached to the VM as disks.

This document explains why the VFS was removed and how to migrate.

## What was removed

| Surface | Removed |
|---|---|
| CLI | `matchlock run --workspace`, `-v` / `--volume` (overlay, `host_fs`, `ro`, and `uid=`/`gid=` options) |
| Go SDK | `WithWorkspace`, `Mount`, `MountHostDir`, `MountHostDirReadonly`, `MountHostDirAs`, `MountHostDirReadonlyAs`, `MountMemory`, `MountOverlay`, `WithVFSInterception`, `CreateOptions.Mounts` / `Workspace` / `VFSInterception`, `MountConfig`, and the `VFS*` hook types |
| Python SDK | `with_workspace`, `mount`, `mount_host_dir`, `mount_host_dir_readonly`, `mount_memory`, `mount_overlay`, `with_vfs_interception`, `CreateOptions.mounts` / `workspace` / `vfs_interception`, `MountConfig`, and the `VFS*` hook types and constants |
| TypeScript SDK | `withWorkspace`, `mount`, `mountHostDir`, `mountHostDirReadonly`, `mountMemory`, `mountOverlay`, `withVFSInterception`, `MountConfig`, `MountOwnerOptions`, and the `VFS*` hook types and constants |
| JSON-RPC | The `vfs` object in `create` params, and VFS `file` event notifications |
| Guest runtime | `guest-fused` and the host VFS server on vsock port 5001 |

Old configurations fail loudly:

```console
$ matchlock run --image alpine:latest -v ./src:code -- true
unknown shorthand flag: 'v' in -v

$ # JSON-RPC create with a "vfs" object
{"jsonrpc":"2.0","error":{"code":-32602,"message":"invalid configuration: vfs is no longer supported; use guest file transfer or block disks instead"},"id":1}
```

The following continue to work as before: `WriteFile` / `ReadFile` / `ListFiles`, all exec modes, `--disk` block devices, named volumes, and `matchlock build`. File operations such as `WriteFile` now act directly on the guest filesystem through the guest agent.

## Why it was removed

### Like-for-like syscall compatibility is very hard to achieve

A host-directory mount only works if every file operation in the guest behaves exactly as it would on a normal Linux disk. With FUSE, each operation passed through several steps: the guest's FUSE daemon, a vsock connection, Matchlock's host-side code, and finally the host's own filesystem. If any step behaved even slightly differently from Linux, programs broke, and this kept happening:

- Over time we fixed problems with `O_APPEND` writes, open flags, directory `fsync`, sync errors, and cached paths after `rename`. The `rename` issue broke the common pattern of writing a temporary file and then renaming it.
- A bug in how inodes were tracked made `getcwd` fail on Alpine/musl.
- Files showed the host's UIDs and GIDs inside the guest, so we had to add options to override file ownership.
- Some operations were never supported, or did nothing without reporting an error: symlinks, hard links, extended attributes, `chown`, updating timestamps, and truncating a file to a non-zero length.
- Some differences cannot be fixed at all. On macOS, where filesystems are usually case-insensitive, the Linux guest also saw case-insensitive paths. File locks and file change notifications did not work across the host and the guest.
- Every file operation was a separate round trip over vsock, handled one at a time, so workloads with many small file operations, such as package installs or `git status`, were slow.

Fixing these issues one at a time did not lead to a complete solution. Package managers, compilers, `git`, and databases expect a real Linux filesystem, and when the behavior is slightly different they tend to fail in ways that are hard to debug. An ext4 filesystem inside the guest, managed by the guest kernel, gives normal Linux behavior without any translation layer.

### Host mounts expose a large attack surface

Matchlock is meant for untrusted workloads, such as running LLM-generated code safely. Mounting a host directory into the guest weakens that isolation in several ways:

- The host has to handle file requests from untrusted code. Each `open`, `stat`, `rename`, or `readdir` in the guest turned into a file operation on the host, run by the Matchlock process with your user's permissions. Keeping the guest inside the mounted directory depended on handling `..`, symlinks, and race conditions correctly on both Linux and macOS. A bug there could let the guest read or write files outside the mount.
- A read-write mount lets the guest change files that the host runs later. This does not require an exploit. The guest can edit `.git/hooks/*`, `.envrc`, a `Makefile`, `package.json` scripts, or editor and CI configuration, and those changes then run on the host the next time you or a tool uses them. Because the mount is live, this can happen while the agent is still running and before anyone has reviewed the changes.
- A mount often shares more than intended. Project directories commonly contain `.env` files, credentials in `.git/config`, `.npmrc` tokens, and similar secrets. Giving the guest access to them goes against Matchlock's secret model, where real secrets stay on the host and the guest only sees placeholders.
- Filesystem hooks made this harder rather than easier. VFS interception needed extra logic to avoid recursion and deadlocks, which added more code to the part of the system that separates the guest from the host.

The replacements keep the interface between the host and the guest small and explicit:

- Guest filesystems: the guest kernel manages the filesystem, and the host only reads and writes blocks in a single image file that it created.
- Explicit transfers: the host decides what is copied in, what is copied out, and when. Results come back as files you can inspect before applying them to your working tree.
- Block volumes: persistent storage is an ext4 image attached to the VM as a disk, rather than a view of a host directory.

## Migration quick reference

| Before (0.2.x) | After (0.3.0) |
|---|---|
| `-v ./src:code` (overlay snapshot) | Copy the files in when the sandbox starts: [Copy files in](#copy-files-in) |
| `-v ./src:code:host_fs` (live read-write) | Copy files in, run the work, copy the results out, and review them before applying: [Copy results out](#copy-results-out) |
| `-v ./data:data:ro` | Copy the files in, or attach a prebuilt ext4 image as a read-only disk: [Read-only inputs](#read-only-inputs) |
| `MountMemory` / `mount_memory` / `mountMemory` | An ordinary guest directory: [Scratch space](#scratch-space) |
| `host_fs,uid=…,gid=…` / `MountHostDirAs` / `owner_uid` | `chown` after extracting, or `--disk …:uid=…,gid=…`: [Ownership](#file-ownership) |
| Data persisted through a host mount | `matchlock volume` + `--disk @name:/path`: [Persistent data](#persistent-data) |
| `--workspace` / `WithWorkspace` as the default workdir | Create the directory and pass `-w` / `WorkingDir`: [Working directory](#working-directory) |
| VFS interception rules and hooks | Checks on the host when files are copied in or out: [Filesystem hooks](#filesystem-hooks) |

## Copy files in

Copied files go into the guest's root filesystem, which is writable and is discarded with the sandbox. This matches how the default overlay mount behaved, where the guest worked on its own copy of the files. For directories, send a tar archive through stdin. For single files, use `WriteFile`.

### CLI

For a one-shot run, pipe the archive into `run -i`:

```bash
tar -C ./src -cf - . |
  matchlock run --image alpine:latest -i -- sh -c '
    mkdir -p /workspace && tar -xf - -C /workspace && cd /workspace && ./test.sh'
```

For a long-lived sandbox, pipe it into `exec -i`:

```bash
VM=$(matchlock run --image alpine:latest -d)

tar -C ./src -cf - . |
  matchlock exec -i "$VM" -- sh -c 'mkdir -p /workspace && tar -xf - -C /workspace'

# Single file
matchlock exec -i "$VM" -- sh -c 'cat > /workspace/config.json' < ./config.json

matchlock exec -w /workspace "$VM" -- make test
```

If the source is a Git repository, you can use `git archive HEAD | matchlock …` instead of `tar`. This sends only tracked files, so untracked files such as `.env` and the `.git` directory stay on the host.

### Go

```go
func uploadDir(ctx context.Context, client *sdk.Client, hostDir, guestDir string) error {
	pack := exec.CommandContext(ctx, "tar", "-C", hostDir, "-cf", "-", ".")
	pack.Stderr = os.Stderr
	tarball, err := pack.StdoutPipe()
	if err != nil {
		return err
	}
	if err := pack.Start(); err != nil {
		return err
	}
	cmd := fmt.Sprintf("mkdir -p %[1]q && tar -xf - -C %[1]q", guestDir)
	res, err := client.ExecPipe(ctx, cmd, tarball, io.Discard, os.Stderr)
	if waitErr := pack.Wait(); err == nil {
		err = waitErr
	}
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("guest tar exited with %d", res.ExitCode)
	}
	return nil
}

// Individual files:
err := client.WriteFile(ctx, "/workspace/config.json", data)
```

### Python

```python
import subprocess
import sys

def upload_dir(client, host_dir: str, guest_dir: str) -> None:
    pack = subprocess.Popen(["tar", "-C", host_dir, "-cf", "-", "."], stdout=subprocess.PIPE)
    try:
        result = client.exec_pipe(
            f"mkdir -p '{guest_dir}' && tar -xf - -C '{guest_dir}'",
            stdin=pack.stdout,
            stderr=sys.stderr,
        )
    finally:
        pack.stdout.close()
        pack.wait()
    if pack.returncode != 0 or result.exit_code != 0:
        raise RuntimeError("upload failed")

# Individual files:
client.write_file("/workspace/config.json", data)
```

### TypeScript

Pass a Node readable stream, such as the stdout of a spawned `tar` process, as `stdin` to `client.execPipe`. For single files, use `client.writeFile`. See [Stdin/Stdout File Transfers](../sdk/typescript/README.md#stdinstdout-file-transfers) for an example.

## Copy results out

Instead of a live read-write mount, run the work in the guest, copy the results out, and review them before they reach your working tree.

### CLI

```bash
# One-shot: upload, work, and stream the result back as a tarball.
# Stdout carries the archive, so send other output to stderr.
mkdir -p ./out
tar -C ./src -cf - . |
  matchlock run --image alpine:latest -i -- sh -c '
    mkdir -p /workspace && tar -xf - -C /workspace && cd /workspace &&
    ./build.sh >&2 && tar -C /workspace/dist -cf - .' |
  tar -xf - -C ./out

# Long-lived sandbox
matchlock exec -i "$VM" -- tar -C /workspace -cf - . </dev/null | tar -xf - -C ./out
matchlock exec -i "$VM" -- cat /workspace/report.json </dev/null > report.json
```

The `-i` flag streams the output instead of collecting it in memory first, which works better for archives and other large outputs. When there is no input to send, redirect stdin from `/dev/null`.

### Go

```go
func downloadDir(ctx context.Context, client *sdk.Client, guestDir, hostDir string) error {
	if err := os.MkdirAll(hostDir, 0o755); err != nil {
		return err
	}
	unpack := exec.CommandContext(ctx, "tar", "-xf", "-", "-C", hostDir)
	unpack.Stderr = os.Stderr
	tarball, err := unpack.StdinPipe()
	if err != nil {
		return err
	}
	if err := unpack.Start(); err != nil {
		return err
	}
	res, err := client.ExecStream(ctx, fmt.Sprintf("tar -C %q -cf - .", guestDir), tarball, os.Stderr)
	tarball.Close()
	if waitErr := unpack.Wait(); err == nil {
		err = waitErr
	}
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("guest tar exited with %d", res.ExitCode)
	}
	return nil
}

// Individual files (up to 16 MiB):
data, err := client.ReadFile(ctx, "/workspace/report.json")
```

### Python

```python
import os
import subprocess
import sys

def download_dir(client, guest_dir: str, host_dir: str) -> None:
    os.makedirs(host_dir, exist_ok=True)
    unpack = subprocess.Popen(["tar", "-xf", "-", "-C", host_dir], stdin=subprocess.PIPE)
    try:
        result = client.exec_stream(
            f"tar -C '{guest_dir}' -cf - .",
            stdout=unpack.stdin,  # binary writers receive exact bytes
            stderr=sys.stderr,
        )
    finally:
        unpack.stdin.close()
        unpack.wait()
    if unpack.returncode != 0 or result.exit_code != 0:
        raise RuntimeError("download failed")

# Individual files (up to 16 MiB):
data = client.read_file("/workspace/report.json")
```

In TypeScript, use `client.execStream` with a writable stream, or `client.readFile`. End the stream and wait for it to finish before reading the output.

We recommend copying results into a separate directory rather than straight into your working tree, and reviewing them first. You can diff them, or have the guest create a patch with `git diff --binary` and check it on the host with `git apply --check` before applying it. Take particular care with hooks, scripts, and build or CI configuration produced by an agent.

## Read-only inputs

Copying files in as shown above already works for read-only inputs. The guest can change its own copy, but not the original files on the host.

For large or frequently reused datasets, you can instead build an ext4 image once and attach it as a read-only disk:

```bash
mkfs.ext4 -q -d ./dataset ./dataset.ext4 2G   # e2fsprogs >= 1.43; size must fit the data
matchlock run --image alpine:latest --disk ./dataset.ext4:/data:ro -- ls /data
```

`mkfs.ext4 -d` copies the contents of the directory into the new image, keeping numeric UIDs/GIDs and file modes. Matchlock already needs e2fsprogs for `matchlock volume create`. On macOS, Homebrew installs `e2fsprogs` as keg-only, so run it using its full path under `$(brew --prefix e2fsprogs)/sbin`.

## Scratch space

Use an ordinary directory in the guest, such as `/tmp` or one created with `mkdir -p /workspace`. The root filesystem belongs to the sandbox and is discarded when the sandbox is removed. If you need more space, increase `--disk-size` (`WithDiskSize` / `with_disk_size` / `withDiskSize`).

## Persistent data

Named volumes are ext4 images that Matchlock manages and attaches to the VM as disks:

```bash
matchlock volume create mydata --size 2048
matchlock run --image alpine:latest --disk @mydata:/data -- sh -c 'echo hello > /data/hello.txt'
matchlock run --image alpine:latest --disk @mydata:/data -- cat /data/hello.txt
matchlock volume cp mydata mydata-backup
matchlock volume rm mydata
```

The Go, Python, and TypeScript SDKs can create, list, and remove volumes (`VolumeCreate` / `volume_create` / `volumeCreate`). Attaching a volume to a sandbox is currently only available from the CLI, using `--disk`.

Because the guest can write to a volume, treat its contents as untrusted. Avoid attaching the same volume read-write to more than one running VM, and avoid mounting it on the host while a VM is using it, as ext4 is not designed to be shared this way. To read a volume's contents, attach it to a sandbox and copy the files out, rather than mounting the image on the host.

## File ownership

When root extracts an archive in the guest, the files keep the numeric UIDs/GIDs they had on the host, for example `1000` on Linux or `501` on macOS. If your commands run as a different user, change the ownership after extracting:

```bash
tar -C ./src -cf - . | matchlock exec -i "$VM" -- sh -c \
  'mkdir -p /workspace && tar -xf - -C /workspace && chown -R 1000:1000 /workspace'
```

For disks, `--disk @name:/path:uid=1000,gid=1000` changes the ownership of the top-level mount directory. This only works on writable disks.

## Working directory

Previously, commands ran in the configured workspace by default. They now run in the image's `WORKDIR`, or in `/` if the image does not set one. `/workspace` is no longer created automatically. Create it yourself and select it with `-w /workspace` (CLI), `ExecWithDir` / `ExecPipeOptions.WorkingDir` (Go), `working_dir=` (Python), or `{ workingDir }` (TypeScript), or set `WORKDIR` in your image.

## Filesystem hooks

There is no direct replacement for VFS interception. Since files now only move between the host and the guest through explicit copies, you can run checks on the host when copying files in or out:

| Before | After |
|---|---|
| `action=block` rules, or `action_hook` | Only copy in files the guest should see. Add your own checks around `WriteFile` / `ReadFile` / `ListFiles` calls; SDK action hooks only applied to those calls anyway. Inside the guest, use normal file permissions, such as running as a non-root `--user` and making files read-only and owned by root. |
| `mutate_hook` | Change the content on the host before calling `WriteFile` or copying files in. |
| `hook` / `dangerous_hook` after-events | After the command finishes, check the results with `ListFiles` or `find /workspace -newer <marker>`, or copy them out and diff them. |

For network-level controls, [network interception](network-interception.md) is still available and unchanged.

## Builds

`matchlock build` no longer mounts the build context. Instead, it copies the context into the BuildKit VM, respecting `.dockerignore` and Dockerfile-specific ignore files, and copies the built image back. The only change you may need is to make sure `--build-disk` (default 10240 MB) is large enough to hold the build context.

## Tips

- Copying files through stdin and stdout needs `tar` (or `cat`) in the image, which most images include. For minimal images without a shell, use `WriteFile` / `ReadFile`.
- `ReadFile` is limited to 16 MiB, and `WriteFile` sends all of its content in a single RPC message. For large files, use the streaming exec modes instead.
- On macOS, set `COPYFILE_DISABLE=1` when running `tar` so that AppleDouble `._*` files are not added to the archive.
- Upgrade the CLI and the SDKs at the same time. If an older SDK configures mounts or a workspace, a 0.3.0 `matchlock rpc` rejects it with the `vfs is no longer supported` error shown above.
