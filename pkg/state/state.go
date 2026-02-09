package state

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

type VMState struct {
	ID        string          `json:"id"`
	PID       int             `json:"pid"`
	Status    string          `json:"status"`
	Image     string          `json:"image"`
	CreatedAt time.Time       `json:"created_at"`
	Config    json.RawMessage `json:"config,omitempty"`
}

type Manager struct {
	baseDir string
}

func NewManager() *Manager {
	home, _ := os.UserHomeDir()
	baseDir := filepath.Join(home, ".matchlock", "vms")
	os.MkdirAll(baseDir, 0755)
	return &Manager{baseDir: baseDir}
}

func NewManagerWithDir(baseDir string) *Manager {
	os.MkdirAll(baseDir, 0755)
	return &Manager{baseDir: baseDir}
}

func (m *Manager) Register(id string, config interface{}) error {
	dir := filepath.Join(m.baseDir, id)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}

	if err := os.WriteFile(filepath.Join(dir, "pid"), []byte(strconv.Itoa(os.Getpid())), 0600); err != nil {
		return err
	}

	configJSON, err := json.Marshal(config)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), configJSON, 0600); err != nil {
		return err
	}

	if err := os.WriteFile(filepath.Join(dir, "status"), []byte("running"), 0600); err != nil {
		return err
	}

	if err := os.WriteFile(filepath.Join(dir, "created_at"), []byte(time.Now().Format(time.RFC3339)), 0600); err != nil {
		return err
	}

	logsDir := filepath.Join(dir, "logs")
	os.MkdirAll(logsDir, 0700)

	return nil
}

func (m *Manager) Unregister(id string) error {
	dir := filepath.Join(m.baseDir, id)
	os.WriteFile(filepath.Join(dir, "status"), []byte("stopped"), 0644)
	os.Remove(filepath.Join(dir, "pid"))
	return nil
}

func (m *Manager) List() ([]VMState, error) {
	entries, err := os.ReadDir(m.baseDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var states []VMState
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		state, err := m.Get(entry.Name())
		if err != nil {
			continue
		}

		if state.Status == "running" && !m.isProcessRunning(state.PID) {
			state.Status = "crashed"
			os.WriteFile(filepath.Join(m.baseDir, state.ID, "status"), []byte("crashed"), 0644)
		}

		states = append(states, state)
	}

	return states, nil
}

func (m *Manager) Get(id string) (VMState, error) {
	dir := filepath.Join(m.baseDir, id)

	if _, err := os.Stat(dir); os.IsNotExist(err) {
		return VMState{}, fmt.Errorf("VM %s not found", id)
	}

	var state VMState
	state.ID = id

	if pidBytes, err := os.ReadFile(filepath.Join(dir, "pid")); err == nil {
		state.PID, _ = strconv.Atoi(string(pidBytes))
	}

	if statusBytes, err := os.ReadFile(filepath.Join(dir, "status")); err == nil {
		state.Status = string(statusBytes)
	}

	if configBytes, err := os.ReadFile(filepath.Join(dir, "config.json")); err == nil {
		state.Config = configBytes

		var cfg struct {
			Image string `json:"image"`
		}
		json.Unmarshal(configBytes, &cfg)
		state.Image = cfg.Image
	}

	if createdBytes, err := os.ReadFile(filepath.Join(dir, "created_at")); err == nil {
		state.CreatedAt, _ = time.Parse(time.RFC3339, string(createdBytes))
	}

	return state, nil
}

func (m *Manager) Kill(id string) error {
	state, err := m.Get(id)
	if err != nil {
		return err
	}

	if state.PID == 0 {
		return fmt.Errorf("VM %s is not running", id)
	}

	process, err := os.FindProcess(state.PID)
	if err != nil {
		return err
	}

	return process.Signal(syscall.SIGTERM)
}

func (m *Manager) Remove(id string) error {
	state, err := m.Get(id)
	if err != nil {
		return err
	}
	if state.Status == "running" {
		if m.isProcessRunning(state.PID) {
			return fmt.Errorf("cannot remove running VM %s, kill it first", id)
		}
		os.WriteFile(filepath.Join(m.baseDir, id, "status"), []byte("crashed"), 0644)
	}

	NewSubnetAllocator().Release(id)

	return os.RemoveAll(filepath.Join(m.baseDir, id))
}

func (m *Manager) Prune() ([]string, error) {
	states, err := m.List()
	if err != nil {
		return nil, err
	}

	var pruned []string
	for _, state := range states {
		if state.Status == "crashed" || state.Status == "stopped" {
			if err := m.Remove(state.ID); err == nil {
				pruned = append(pruned, state.ID)
			}
		}
	}

	NewSubnetAllocator().Cleanup(m)

	return pruned, nil
}

func (m *Manager) isProcessRunning(pid int) bool {
	if pid == 0 {
		return false
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = process.Signal(syscall.Signal(0))
	return err == nil
}

func (m *Manager) LogPath(id string) string {
	return filepath.Join(m.baseDir, id, "logs", "vm.log")
}

func (m *Manager) SocketPath(id string) string {
	return filepath.Join(m.baseDir, id, "socket")
}

func (m *Manager) ExecSocketPath(id string) string {
	return filepath.Join(m.baseDir, id, "exec.sock")
}

func (m *Manager) Dir(id string) string {
	return filepath.Join(m.baseDir, id)
}
