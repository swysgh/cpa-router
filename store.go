package main

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"
)

// groupStore reads and writes the group state file. All file IO is confined to
// this file; other code interacts with parsed groupFile / []group.
type groupStore struct {
	path string
}

// newGroupStore creates a store for the given (possibly relative) path.
func newGroupStore(path string) *groupStore {
	return &groupStore{path: path}
}

// fileModTime returns the modification time of the state file, and whether it
// exists.
func (s *groupStore) fileModTime() (time.Time, bool) {
	fi, err := os.Stat(s.path)
	if err != nil {
		return time.Time{}, false
	}
	return fi.ModTime(), true
}

// load reads and parses the state file. It returns the parsed groupFile and the
// absolute path actually used.
func (s *groupStore) load() (groupFile, string, error) {
	abs, err := filepath.Abs(s.path)
	if err != nil {
		abs = s.path
	}
	raw, err := os.ReadFile(s.path)
	if err != nil {
		return groupFile{}, abs, fmt.Errorf("读取状态文件失败: %w", err)
	}
	var f groupFile
	if err := yaml.Unmarshal(raw, &f); err != nil {
		return groupFile{}, abs, fmt.Errorf("解析状态文件失败: %w", err)
	}
	return f, abs, nil
}

// save atomically writes the groupFile: temp file + os.Rename, 0600 perms,
// directory 0755.
func (s *groupStore) save(f groupFile) error {
	// Normalize nil slices to empty so write/read round-trips are stable
	// (yaml emits a nil slice as nothing and reloads it as an empty non-nil
	// slice, which trips reflect.DeepEqual in callers/tests).
	norm := make([]rawGroup, len(f.Groups))
	for i, g := range f.Groups {
		if g.Aliases == nil {
			g.Aliases = []string{}
		}
		mm := make([]rawMember, len(g.Members))
		copy(mm, g.Members)
		g.Members = mm
		norm[i] = g
	}
	f.Groups = norm
	raw, err := yaml.Marshal(f)
	if err != nil {
		return fmt.Errorf("序列化状态文件失败: %w", err)
	}
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("创建状态文件目录失败: %w", err)
	}
	abs, err := filepath.Abs(s.path)
	if err != nil {
		abs = s.path
	}
	tmp, err := os.CreateTemp(dir, ".groups-*.yaml.tmp")
	if err != nil {
		return fmt.Errorf("创建临时文件失败: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("写入临时文件失败: %w", err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("设置临时文件权限失败: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("关闭临时文件失败: %w", err)
	}
	if err := os.Rename(tmpName, abs); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("替换状态文件失败: %w", err)
	}
	return nil
}
