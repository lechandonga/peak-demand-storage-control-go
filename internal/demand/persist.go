package demand

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// persistedState is the on-disk snapshot enabling idempotent restart:
// accepted sample IDs prevent re-accumulation, setpoint/emit memory prevents
// duplicate actions, and overrides + audit trail survive the restart.
type persistedState struct {
	Version int       `json:"version"`
	SavedAt time.Time `json:"saved_at"`

	Seen       map[string]int64           `json:"seen"`
	Devices    map[string]persistedDevice `json:"devices"`
	LastEnergy map[string]float64         `json:"last_energy"`
	LastSOC    map[string]float64         `json:"last_soc"`
	Reconcile  map[string]ReconcileConfig `json:"reconcile"`

	Window    windowPersist          `json:"window"`
	BatteryKW map[string]float64     `json:"battery_kw"`
	LoadAllow map[string]float64     `json:"load_allow"`
	LastEmit  map[string]string      `json:"last_emit"`
	Healthy   int                    `json:"healthy"`
	Fault     FaultState             `json:"fault"`
	Overrides map[string]AuditRecord `json:"overrides"`
	Audits    []AuditRecord          `json:"audits"`
	TickCount int64                  `json:"tick_count"`
	LastTick  time.Time              `json:"last_tick"`
	Started   bool                   `json:"started"`
}

type persistedDevice struct {
	Sample     Sample    `json:"sample"`
	ReceivedAt time.Time `json:"received_at"`
}

// Save writes an atomic snapshot to path.
func (c *Controller) Save(path string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.store.mu.Lock()
	defer c.store.mu.Unlock()

	st := persistedState{Version: 1, SavedAt: c.now()}
	st.Seen = make(map[string]int64, len(c.store.seen))
	for k, v := range c.store.seen {
		st.Seen[k] = v
	}
	st.Devices = map[string]persistedDevice{}
	for k, v := range c.store.devices {
		st.Devices[k] = persistedDevice{Sample: v.sample, ReceivedAt: v.receivedAt}
	}
	st.LastEnergy = copyFloatMap(c.store.lastEnergy)
	st.LastSOC = copyFloatMap(c.store.lastSOC)
	st.Reconcile = copyReconcile(c.store.reconcile)
	st.Window = c.book.persist()
	st.BatteryKW = copyFloatMap(c.batteryKW)
	st.LoadAllow = copyFloatMap(c.loadAllow)
	st.LastEmit = copyStringMap(c.lastEmit)
	st.Healthy = c.healthy
	st.Fault = c.fault
	st.Overrides = make(map[string]AuditRecord, len(c.overrides))
	for k, v := range c.overrides {
		st.Overrides[k] = v
	}
	st.Audits = append([]AuditRecord(nil), c.audits...)
	st.TickCount = c.tickCount
	st.LastTick = c.lastTick
	st.Started = c.started

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	if err := enc.Encode(&st); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

// Load restores a snapshot previously written by Save.
func (c *Controller) Load(path string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var st persistedState
	if err := json.Unmarshal(data, &st); err != nil {
		return err
	}

	c.store.mu.Lock()
	defer c.store.mu.Unlock()

	for k, v := range st.Seen {
		c.store.seen[k] = v
	}
	for k, v := range st.Devices {
		c.store.devices[k] = deviceState{sample: v.Sample, receivedAt: v.ReceivedAt}
	}
	for k, v := range st.LastEnergy {
		c.store.lastEnergy[k] = v
	}
	for k, v := range st.LastSOC {
		c.store.lastSOC[k] = v
	}
	for k, v := range st.Reconcile {
		c.store.reconcile[k] = v
		// Rebuild the last-battery anchor from restored devices.
		if d, ok := c.store.devices[string(SourceBattery)+":"+k]; ok {
			c.store.lastBatt[k] = d.sample
		}
	}

	c.book.Restore(st.Window)
	for k, v := range st.BatteryKW {
		c.batteryKW[k] = v
	}
	for k, v := range st.LoadAllow {
		c.loadAllow[k] = v
	}
	for k, v := range st.LastEmit {
		c.lastEmit[k] = v
	}
	c.healthy = st.Healthy
	c.fault = st.Fault
	c.overrides = st.Overrides
	if c.overrides == nil {
		c.overrides = map[string]AuditRecord{}
	}
	c.audits = st.Audits
	c.tickCount = st.TickCount
	c.lastTick = st.LastTick
	c.started = st.Started
	return nil
}

func copyFloatMap(m map[string]float64) map[string]float64 {
	out := make(map[string]float64, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func copyStringMap(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func copyReconcile(m map[string]ReconcileConfig) map[string]ReconcileConfig {
	out := make(map[string]ReconcileConfig, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
