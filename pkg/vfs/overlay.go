package vfs

import (
	"os"
	"syscall"
)

type OverlayProvider struct {
	upper Provider
	lower Provider
}

func NewOverlayProvider(upper, lower Provider) *OverlayProvider {
	return &OverlayProvider{upper: upper, lower: lower}
}

func (p *OverlayProvider) Readonly() bool { return false }

func (p *OverlayProvider) Stat(path string) (FileInfo, error) {
	info, err := p.upper.Stat(path)
	if err == nil {
		return info, nil
	}
	return p.lower.Stat(path)
}

func (p *OverlayProvider) ReadDir(path string) ([]DirEntry, error) {
	upperEntries, upperErr := p.upper.ReadDir(path)
	lowerEntries, lowerErr := p.lower.ReadDir(path)

	if upperErr != nil && lowerErr != nil {
		return nil, upperErr
	}

	seen := make(map[string]bool)
	var result []DirEntry

	for _, e := range upperEntries {
		seen[e.Name()] = true
		result = append(result, e)
	}

	for _, e := range lowerEntries {
		if !seen[e.Name()] {
			result = append(result, e)
		}
	}

	return result, nil
}

func (p *OverlayProvider) Open(path string, flags int, mode os.FileMode) (Handle, error) {
	if flags&(os.O_WRONLY|os.O_RDWR|os.O_CREATE|os.O_TRUNC|os.O_APPEND) != 0 {
		if flags&os.O_CREATE == 0 {
			_, err := p.upper.Stat(path)
			if err != nil {
				if err := p.copyUp(path); err != nil {
					return nil, err
				}
			}
		}
		return p.upper.Open(path, flags, mode)
	}

	h, err := p.upper.Open(path, flags, mode)
	if err == nil {
		return h, nil
	}
	return p.lower.Open(path, flags, mode)
}

func (p *OverlayProvider) Create(path string, mode os.FileMode) (Handle, error) {
	return p.upper.Create(path, mode)
}

func (p *OverlayProvider) Mkdir(path string, mode os.FileMode) error {
	return p.upper.Mkdir(path, mode)
}

func (p *OverlayProvider) Remove(path string) error {
	return p.upper.Remove(path)
}

func (p *OverlayProvider) RemoveAll(path string) error {
	return p.upper.RemoveAll(path)
}

func (p *OverlayProvider) Rename(oldPath, newPath string) error {
	_, err := p.upper.Stat(oldPath)
	if err != nil {
		if err := p.copyUp(oldPath); err != nil {
			return err
		}
	}
	return p.upper.Rename(oldPath, newPath)
}

func (p *OverlayProvider) Symlink(target, link string) error {
	return p.upper.Symlink(target, link)
}

func (p *OverlayProvider) Readlink(path string) (string, error) {
	link, err := p.upper.Readlink(path)
	if err == nil {
		return link, nil
	}
	return p.lower.Readlink(path)
}

func (p *OverlayProvider) copyUp(path string) error {
	info, err := p.lower.Stat(path)
	if err != nil {
		return err
	}

	if info.IsDir() {
		return p.upper.Mkdir(path, info.Mode())
	}

	src, err := p.lower.Open(path, os.O_RDONLY, 0)
	if err != nil {
		return err
	}
	defer src.Close()

	dst, err := p.upper.Create(path, info.Mode())
	if err != nil {
		return err
	}
	defer dst.Close()

	buf := make([]byte, 32*1024)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return werr
			}
		}
		if err != nil {
			if err.Error() == "EOF" {
				break
			}
			return err
		}
	}

	return nil
}

func (p *OverlayProvider) WhiteoutPath(path string) error {
	memUpper, ok := p.upper.(*MemoryProvider)
	if !ok {
		return syscall.ENOSYS
	}
	return memUpper.WriteFile(path+".whiteout", []byte{}, 0)
}
