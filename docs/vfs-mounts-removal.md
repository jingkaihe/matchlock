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

The following continue to work as before: `WriteFile` / `ReadFile` / `ListFiles`, all exec modes, `--disk` block devices, named volumes, and `matchlock build`. File operations such as `WriteFile` now act directly on the guest filesystem through the guest agent. Matchlock 0.3.0 also adds `matchlock cp`, `run --copy-in` and `--copy-out`, and copy methods in the SDKs for copying files and directories in and out of a sandbox.

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
| `-v ./src:code` (overlay snapshot) | Copy the files in with `--copy-in` or `matchlock cp`: [Copy files in](#copy-files-in) |
| `-v ./src:code:host_fs` (live read-write) | Copy files in, run the work, copy the results out, and review them before applying: [Copy results out](#copy-results-out) |
| `-v ./data:data:ro` | Copy the files in, or attach a prebuilt ext4 image as a read-only disk: [Read-only inputs](#read-only-inputs) |
| `MountMemory` / `mount_memory` / `mountMemory` | An ordinary guest directory: [Scratch space](#scratch-space) |
| `host_fs,uid=…,gid=…` / `MountHostDirAs` / `owner_uid` | Copied files are owned by the sandbox user, or pass `--owner`: [File ownership](#file-ownership) |
| Data persisted through a host mount | `matchlock volume` + `--disk @name:/path`: [Persistent data](#persistent-data) |
| `--workspace` / `WithWorkspace` as the default workdir | Pass `-w` / `WorkingDir`: [Working directory](#working-directory) |
| VFS interception rules and hooks | Checks on the host when files are copied in or out: [Filesystem hooks](#filesystem-hooks) |

## Copy files in

Copied files go into the guest's root filesystem, which is writable and is discarded with the sandbox. This matches how the default overlay mount behaved, where the guest worked on its own copy of the files.

Paths follow these rules:

- A source ending in `/.` copies the contents of the directory rather than the directory itself.
- If the destination is an existing directory, the source is copied inside it. Otherwise the source is copied to the destination path, replacing an existing file.
- A destination ending in `/` must be an existing directory.

### CLI

For a one-shot run, `--copy-in` copies files in before the command starts:

```bash
matchlock run --image alpine:latest --copy-in ./src/.:/workspace -w /workspace -- ./test.sh
```

For a long-lived sandbox, use `matchlock cp`:

```bash
VM=$(matchlock run --image alpine:latest -d)
matchlock cp ./src/. "$VM":/workspace
matchlock cp ./config.json "$VM":/workspace/
matchlock exec -w /workspace "$VM" -- make test
```

`matchlock cp` accepts `--exclude` with `.dockerignore`-style patterns, for example `--exclude .env --exclude .git`. If the source is a Git repository, `git archive HEAD | matchlock cp - "$VM":/workspace` sends only tracked files. With `-` as the source, the destination directory must already exist.

### SDKs

The Go, Python, and TypeScript SDKs follow the same rules. Relative host paths are resolved against the current directory.

Go:

```go
err := client.CopyIn(ctx, "./src/.", "/workspace", nil)
```

Python:

```python
client.copy_in("./src/.", "/workspace")
```

TypeScript:

```ts
await client.copyIn("./src/.", "/workspace");
```

For single small files, `WriteFile` / `write_file` / `writeFile` still works.

## Copy results out

Instead of a live read-write mount, run the work in the guest, copy the results out, and review them before they reach your working tree.

### CLI

`--copy-out` copies files out after the command exits. It also runs when the command fails, so logs and partial results are kept, and `run` still exits with the command's exit code.

```bash
matchlock run --image alpine:latest --copy-in ./src/.:/workspace --copy-out /workspace/dist:./out -w /workspace -- ./build.sh
```

For a long-lived sandbox:

```bash
matchlock cp "$VM":/workspace/dist ./out
matchlock cp "$VM":/workspace/report.json ./report.json
matchlock cp "$VM":/workspace - > workspace.tar
```

### SDKs

Go:

```go
err := client.CopyOut(ctx, "/workspace/dist", "./out", nil)
```

Python:

```python
client.copy_out("/workspace/dist", "./out")
```

TypeScript:

```ts
await client.copyOut("/workspace/dist", "./out");
```

The SDKs refuse to copy out when they run matchlock with sudo, because the copied files would be owned by root.

### How copied-out files are handled

Matchlock treats files coming out of the sandbox as untrusted:

- setuid and setgid bits are removed, and your umask applies.
- Device files, FIFOs, and symlinks that resolve outside the destination are skipped, with a warning.
- Files are never written through a symlink or hard link. Existing links at the destination are replaced, and a copy that would write through a symlink fails.
- By default, at most 10 GiB and 1,000,000 files are written. You can change this with `--max-size` and `--max-files`, or the matching SDK options, where 0 means no limit.
- A single copied file is never left half-written. An interrupted directory copy may leave partial results, as with `cp` or `rsync`.

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

Files copied into the sandbox are owned by the sandbox's user, which is the image `USER` or the `--user` / `WithUser` setting, or root when neither is set. To choose a different owner, pass `--owner uid:gid` to `matchlock cp`, or `owner` in the SDKs. To keep the numeric owners from the host instead, pass `--archive`, or `archive` in the SDKs.

For disks, `--disk @name:/path:uid=1000,gid=1000` changes the ownership of the top-level mount directory. This only works on writable disks.

## Working directory

Previously, commands ran in the configured workspace by default. They now run in the image's `WORKDIR`, or in `/` if the image does not set one. `/workspace` is no longer created automatically, but copying files to it creates it. Select it with `-w /workspace` (CLI), `ExecWithDir` / `ExecPipeOptions.WorkingDir` (Go), `working_dir=` (Python), or `{ workingDir }` (TypeScript), or set `WORKDIR` in your image.

## Filesystem hooks

There is no direct replacement for VFS interception. Since files now only move between the host and the guest through explicit copies, you can run checks on the host when copying files in or out:

| Before | After |
|---|---|
| `action=block` rules, or `action_hook` | Only copy in files the guest should see, for example with `--exclude`. Add your own checks around copy and `WriteFile` / `ReadFile` / `ListFiles` calls; SDK action hooks only applied to the file calls anyway. Inside the guest, use normal file permissions, such as running as a non-root `--user` and making files read-only and owned by root. |
| `mutate_hook` | Change the content on the host before copying it in. |
| `hook` / `dangerous_hook` after-events | After the command finishes, check the results with `ListFiles` or `find /workspace -newer <marker>`, or copy them out and diff them. |

For network-level controls, [network interception](network-interception.md) is still available and unchanged.

## Builds

`matchlock build` no longer mounts the build context. Instead, it copies the context into the BuildKit VM, respecting `.dockerignore` and Dockerfile-specific ignore files, and copies the built image back. The only change you may need is to make sure `--build-disk` (default 10240 MB) is large enough to hold the build context.

## Tips

- `matchlock cp` and the SDK copy methods work with any image, including images without a shell or `tar`, because the guest agent does the copying.
- `matchlock cp` needs a sandbox started with `--rm=false` or `-d`. For sandboxes created through an SDK, use the SDK copy methods.
- `ReadFile` is limited to 16 MiB. Use the copy methods for larger files.
- Upgrade the CLI and the SDKs at the same time. If an older SDK configures mounts or a workspace, a 0.3.0 `matchlock rpc` rejects it with the `vfs is no longer supported` error shown above.
