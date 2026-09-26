package postgres

import "sync"

type recLogger struct {
	mu      sync.Mutex
	entries []string
}

func (r *recLogger) add(level, msg string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entries = append(r.entries, level+":"+msg)
}
func (r *recLogger) Debug(m string, _ map[string]any) { r.add("debug", m) }
func (r *recLogger) Info(m string, _ map[string]any)  { r.add("info", m) }
func (r *recLogger) Warn(m string, _ map[string]any)  { r.add("warn", m) }
func (r *recLogger) Error(m string, _ map[string]any) { r.add("error", m) }
