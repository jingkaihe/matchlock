package vfs

// CloseProvider releases any underlying OS resources held by a provider tree.
// It is optional: providers that do not hold resources (for example
// MemoryProvider) simply do nothing. This is used by the sandbox lifecycle to
// close os.Root directory handles after the VFS server has stopped, so host_fs
// mounts do not leak directory file descriptors for the lifetime of the
// process.
func CloseProvider(p Provider) error {
	if p == nil {
		return nil
	}
	switch p := p.(type) {
	case *RealFSProvider:
		return p.Close()
	case *ReadonlyProvider:
		return CloseProvider(p.inner)
	case *MountRouter:
		var err error
		for _, m := range p.mounts {
			if mErr := CloseProvider(m.provider); err == nil {
				err = mErr
			}
		}
		return err
	case *interceptProvider:
		return CloseProvider(p.inner)
	default:
		return nil
	}
}
